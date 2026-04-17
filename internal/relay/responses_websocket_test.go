package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func newResponsesWebsocketTestServer(apiKeyID int, supportedModels string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		c.Set("api_key_id", apiKeyID)
		c.Set("supported_models", supportedModels)
		HandleResponsesWebsocket(c)
	}))
}

func websocketURLFromHTTP(raw string) string {
	return "ws" + strings.TrimPrefix(raw, "http")
}

func waitForLatestRelayLog(t *testing.T, ctx context.Context) dbmodel.RelayLog {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		page := 1
		pageSize := 10
		logs, err := op.RelayLogList(ctx, nil, nil, page, pageSize)
		if err == nil && len(logs) > 0 {
			return logs[0]
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("RelayLogList failed: %v", err)
			}
			t.Fatal("expected relay logs, got none")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func createResponsesWebsocketGroup(t *testing.T, ctx context.Context, requestModel string, enabled bool) *dbmodel.Group {
	t.Helper()
	group := &dbmodel.Group{
		Name:                      requestModel,
		Mode:                      dbmodel.GroupModeFailover,
		PreferredProtocolFamily:   dbmodel.GroupProtocolFamilyOpenAIResponses,
		ResponsesWebsocketEnabled: enabled,
	}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	return group
}

func TestHandleResponsesWebsocket_ProxiesFramesToSameProtocolChannel(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "responses-ws-test"
	upstreamModel := "responses-ws-upstream"
	var upstreamPath string
	var upstreamAuth string
	var upstreamQuery string
	var firstFrame []byte
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamQuery = r.URL.RawQuery
		upstreamAuth = r.Header.Get("Authorization")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read first frame failed: %v", err)
			return
		}
		firstFrame = append([]byte(nil), payload...)

		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_1","object":"response","model":"`+upstreamModel+`","status":"in_progress","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","model":"`+upstreamModel+`","status":"completed","output":[]}}`))
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-ws-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	group := createResponsesWebsocketGroup(t, ctx, requestModel, true)
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: upstreamModel, Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	octopus := newResponsesWebsocketTestServer(apiKey.ID, requestModel)
	defer octopus.Close()

	conn, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(octopus.URL)+"/v1/responses?trace=1", nil)
	if err != nil {
		t.Fatalf("dial octopus websocket failed: %v", err)
	}
	defer conn.Close()

	firstRequest := []byte(`{"type":"response.create","model":"` + requestModel + `","input":"hi","store":false}`)
	if err := conn.WriteMessage(websocket.TextMessage, firstRequest); err != nil {
		t.Fatalf("write first frame failed: %v", err)
	}

	gotEvents := make([]string, 0, 3)
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				break
			}
			t.Fatalf("read proxied message failed: %v", err)
		}
		gotEvents = append(gotEvents, string(payload))
	}

	if upstreamPath != "/v1/responses" {
		t.Fatalf("unexpected upstream path: %s", upstreamPath)
	}
	if upstreamQuery != "trace=1" {
		t.Fatalf("unexpected upstream query: %s", upstreamQuery)
	}
	if upstreamAuth != "Bearer upstream-key" {
		t.Fatalf("unexpected upstream authorization: %s", upstreamAuth)
	}
	var upstreamCreate map[string]any
	if err := json.Unmarshal(firstFrame, &upstreamCreate); err != nil {
		t.Fatalf("failed to decode upstream first frame: %v", err)
	}
	if got := upstreamCreate["model"]; got != upstreamModel {
		t.Fatalf("unexpected upstream model in first frame: %v", got)
	}
	if got := upstreamCreate["type"]; got != "response.create" {
		t.Fatalf("unexpected upstream event type: %v", got)
	}
	if len(gotEvents) != 3 {
		t.Fatalf("expected 3 upstream events, got %d: %#v", len(gotEvents), gotEvents)
	}
	if !strings.Contains(gotEvents[1], `response.output_text.delta`) {
		t.Fatalf("expected output_text event, got %q", gotEvents[1])
	}

	logItem := waitForLatestRelayLog(t, ctx)
	if logItem.Error != "" {
		t.Fatalf("expected successful websocket relay log, got error %q", logItem.Error)
	}
	assertRelayLogContainsEventTypes(t, logItem.UpstreamEventTypes, "response.created", "response.output_text.delta", "response.completed")
	assertRelayLogContainsTrace(t, logItem.ExecutionTrace, "ws_connected:", "ws_forwarded_first_frame:", "ws_proxy_complete:")
}

func TestHandleResponsesWebsocket_FallsBackBeforeFirstFrameWhenHandshakeFails(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "responses-ws-fallback"
	var firstHits int32
	var secondHits int32

	failedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("handshake failed"))
	}))
	defer failedUpstream.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	successUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_ok","object":"response","model":"ok","status":"in_progress","output":[]}}`))
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	}))
	defer successUpstream.Close()

	firstChannel := createRelayTestChannel(t, ctx, "responses-ws-failed", outbound.OutboundTypeOpenAIResponse, failedUpstream.URL+"/v1")
	secondChannel := createRelayTestChannel(t, ctx, "responses-ws-success", outbound.OutboundTypeOpenAIResponse, successUpstream.URL+"/v1")
	group := createResponsesWebsocketGroup(t, ctx, requestModel, true)
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "failed", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("add failed channel: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "ok", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("add success channel: %v", err)
	}

	octopus := newResponsesWebsocketTestServer(apiKey.ID, requestModel)
	defer octopus.Close()

	conn, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(octopus.URL)+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("dial octopus websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"`+requestModel+`","input":"hi"}`)); err != nil {
		t.Fatalf("write first frame failed: %v", err)
	}

	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read proxied fallback message failed: %v", err)
	}
	if !strings.Contains(string(payload), `response.created`) {
		t.Fatalf("expected fallback response.created event, got %q", string(payload))
	}
	if got := atomic.LoadInt32(&firstHits); got != 1 {
		t.Fatalf("expected first upstream handshake once, got %d", got)
	}
	if got := atomic.LoadInt32(&secondHits); got != 1 {
		t.Fatalf("expected second upstream handshake once, got %d", got)
	}
}

func TestHandleResponsesWebsocket_RejectsWhenGroupToggleDisabled(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "responses-ws-disabled"
	channel := createRelayTestChannel(t, ctx, "responses-ws-channel", outbound.OutboundTypeOpenAIResponse, "http://example.invalid/v1")
	group := createResponsesWebsocketGroup(t, ctx, requestModel, false)
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: "upstream-model", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	octopus := newResponsesWebsocketTestServer(apiKey.ID, requestModel)
	defer octopus.Close()

	conn, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(octopus.URL)+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("dial octopus websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"`+requestModel+`","input":"hi"}`)); err != nil {
		t.Fatalf("write first frame failed: %v", err)
	}

	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected websocket close error")
	}
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("expected close error, got %T: %v", err, err)
	}
	if closeErr.Code != websocket.ClosePolicyViolation {
		t.Fatalf("unexpected close code: %d", closeErr.Code)
	}
	if !strings.Contains(closeErr.Text, "disabled") {
		t.Fatalf("unexpected close reason: %s", closeErr.Text)
	}

	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "disabled") {
		t.Fatalf("expected relay log error about disabled websocket, got %q", logItem.Error)
	}
}

func TestHandleResponsesWebsocket_RejectsCrossProtocolOnlyGroup(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "responses-ws-cross-protocol"
	chatChannel := createRelayTestChannel(t, ctx, "chat-only", outbound.OutboundTypeOpenAIChat, "http://example.invalid/v1")
	group := createResponsesWebsocketGroup(t, ctx, requestModel, true)
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: chatChannel.ID, ModelName: "chat-model", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	octopus := newResponsesWebsocketTestServer(apiKey.ID, requestModel)
	defer octopus.Close()

	conn, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(octopus.URL)+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("dial octopus websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"`+requestModel+`","input":"hi"}`)); err != nil {
		t.Fatalf("write first frame failed: %v", err)
	}

	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected websocket close error")
	}
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("expected close error, got %T: %v", err, err)
	}
	if closeErr.Code != websocket.CloseTryAgainLater {
		t.Fatalf("unexpected close code: %d", closeErr.Code)
	}
	if !strings.Contains(closeErr.Text, "same-protocol") {
		t.Fatalf("unexpected close reason: %s", closeErr.Text)
	}

	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "same-protocol") {
		t.Fatalf("expected relay log error about same-protocol channel, got %q", logItem.Error)
	}
}

func TestReadResponsesWebsocketCreateFrame_ParsesBody(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverConnCh := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		serverConnCh <- conn
	}))
	defer server.Close()

	clientConn, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(server.URL), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer clientConn.Close()

	serverConn := <-serverConnCh
	defer serverConn.Close()

	raw := map[string]any{"type": "response.create", "model": "gpt-5.4", "input": "hi", "store": false}
	payload, _ := json.Marshal(raw)
	if err := clientConn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	messageType, firstPayload, internalRequest, err := readResponsesWebsocketCreateFrame(serverConn)
	if err != nil {
		t.Fatalf("readResponsesWebsocketCreateFrame failed: %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("unexpected message type: %d", messageType)
	}
	if !bytes.Equal(bytes.TrimSpace(firstPayload), bytes.TrimSpace(payload)) {
		t.Fatalf("payload mismatch")
	}
	if internalRequest == nil || internalRequest.Model != "gpt-5.4" {
		t.Fatalf("unexpected internal request: %#v", internalRequest)
	}
	if strings.Contains(string(internalRequest.RawRequest), `"type"`) {
		t.Fatalf("expected raw request body without event envelope, got %s", string(internalRequest.RawRequest))
	}
	if !strings.Contains(string(internalRequest.RawRequest), `"store":false`) {
		t.Fatalf("expected original body fields preserved, got %s", string(internalRequest.RawRequest))
	}
}
