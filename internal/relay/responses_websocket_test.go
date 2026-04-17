package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
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

func TestHandleResponsesWebsocket_ReportsUnexpectedUpstreamCloseWithoutTerminal(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "responses-ws-unexpected-close"
	upstreamModel := "responses-ws-upstream"
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_1","object":"response","model":"`+upstreamModel+`","status":"in_progress","output":[]}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"partial"}`))
		_ = conn.Close()
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-ws-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	group := createResponsesWebsocketGroup(t, ctx, requestModel, true)
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: upstreamModel, Priority: 1, Weight: 1}, ctx); err != nil {
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

	for {
		_, _, err = conn.ReadMessage()
		if err != nil {
			break
		}
	}

	logItem := waitForLatestRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "before terminal event") {
		t.Fatalf("expected relay log error about upstream close before terminal, got %q", logItem.Error)
	}
	if logItem.TerminalSeen == nil || *logItem.TerminalSeen {
		t.Fatalf("expected terminal seen to be false, got %v", logItem.TerminalSeen)
	}
}

func TestNormalizeResponsesWebsocketReadError_ConvertsTimeout(t *testing.T) {
	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 40 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	idleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		serverConn, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			t.Errorf("upgrade failed: %v", upgradeErr)
			return
		}
		defer serverConn.Close()
		time.Sleep(200 * time.Millisecond)
	}))
	defer idleServer.Close()

	conn, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(idleServer.URL), nil)
	if err != nil {
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	applyResponsesWebsocketReadDeadline(conn)
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected read timeout")
	}
	normalized := normalizeResponsesWebsocketReadError(err)
	if normalized == nil || !strings.Contains(normalized.Error(), "idle timeout") {
		t.Fatalf("expected normalized idle timeout, got %v", normalized)
	}
}

func TestProxyResponsesWebsocket_ClientDisconnectAfterTerminalReturnsNil(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverConnCh := make(chan *websocket.Conn, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		serverConnCh <- conn
	}))
	defer server.Close()

	clientA, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(server.URL), nil)
	if err != nil {
		t.Fatalf("dial clientA failed: %v", err)
	}
	defer clientA.Close()
	clientB, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(server.URL), nil)
	if err != nil {
		t.Fatalf("dial clientB failed: %v", err)
	}
	defer clientB.Close()

	serverA := <-serverConnCh
	defer serverA.Close()
	serverB := <-serverConnCh
	defer serverB.Close()

	metrics := NewRelayMetrics(0, "terminal-disconnect", nil)
	go func() {
		_ = clientB.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[]}}`))
		_ = clientB.Close()
	}()

	err = proxyResponsesWebsocket(serverA, &responsesWebsocketSession{upstreamConn: serverB}, metrics)
	if err != nil {
		t.Fatalf("expected client disconnect after terminal to be ignored, got %v", err)
	}
	if !metrics.TerminalSeen {
		t.Fatal("expected terminal event to be recorded")
	}
}

func TestEstablishResponsesWebsocketSession_DialerErrorFallsBack(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	requestModel := "responses-ws-dialer-fallback"
	group := createResponsesWebsocketGroup(t, ctx, requestModel, true)

	firstChannel := createRelayTestChannel(t, ctx, "responses-ws-first", outbound.OutboundTypeOpenAIResponse, "http://example.invalid/v1")
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	secondChannel := createRelayTestChannel(t, ctx, "responses-ws-second", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")

	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "first-upstream", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("add first channel failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "second-upstream", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("add second channel failed: %v", err)
	}

	loadedGroup, err := op.GroupGetEnabledMap(requestModel, ctx)
	if err != nil {
		t.Fatalf("GroupGetEnabledMap failed: %v", err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set("api_key_id", 1)
	metrics := NewRelayMetrics(1, requestModel, nil)

	var dialerCalls int32
	session, err := establishResponsesWebsocketSessionWithDialer(ctx, c, loadedGroup, requestModel, metrics, nil, func(channel *dbmodel.Channel) (*websocket.Dialer, error) {
		if channel.Name == "responses-ws-first" {
			atomic.AddInt32(&dialerCalls, 1)
			return nil, fmt.Errorf("dialer init failed")
		}
		return websocket.DefaultDialer, nil
	})
	if err != nil {
		t.Fatalf("expected fallback session, got error %v", err)
	}
	defer session.upstreamConn.Close()
	if session.channel == nil || session.channel.Name != "responses-ws-second" {
		t.Fatalf("expected second channel to be selected, got %#v", session.channel)
	}
	if atomic.LoadInt32(&dialerCalls) != 1 {
		t.Fatalf("expected first dialer to be attempted once, got %d", atomic.LoadInt32(&dialerCalls))
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

func TestEstablishResponsesWebsocketSession_UsesPinnedResponsesStatefulRoute(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	requestModel := "responses-ws-pinned-route"
	group := createResponsesWebsocketGroup(t, ctx, requestModel, true)

	firstChannel := createRelayTestChannel(t, ctx, "responses-ws-first", outbound.OutboundTypeOpenAIResponse, "http://example.invalid/v1")
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	secondChannel := createRelayTestChannel(t, ctx, "responses-ws-second", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")

	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "first-upstream", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("add first channel failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "second-upstream", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("add second channel failed: %v", err)
	}

	loadedGroup, err := op.GroupGetEnabledMap(requestModel, ctx)
	if err != nil {
		t.Fatalf("GroupGetEnabledMap failed: %v", err)
	}
	req := &transformerModel.InternalLLMRequest{
		Model:        requestModel,
		RawAPIFormat: transformerModel.APIFormatOpenAIResponse,
		RawRequest:   []byte(`{"model":"responses-ws-pinned-route","previous_response_id":"resp_prev","input":"hi"}`),
	}
	responsesStateful := buildResponsesStatefulRequestContext(loadedGroup, 1001, requestModel, req)
	if responsesStateful == nil {
		t.Fatal("expected responses stateful context")
	}
	rememberResponsesAffinityRoute(1001, responsesStateful.AllLookupKeys(), affinityRoute{
		ChannelID:    secondChannel.ID,
		ChannelKeyID: secondChannel.Keys[0].ID,
		BaseURL:      secondChannel.BaseUrls[0].URL,
	})
	t.Cleanup(func() {
		for _, key := range responsesStateful.AllLookupKeys() {
			responsesAffinityStore.Delete(responsesAffinityStoreKey(1001, key))
		}
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set("api_key_id", 1001)
	metrics := NewRelayMetrics(1001, requestModel, req)

	session, err := establishResponsesWebsocketSessionWithDialer(ctx, c, loadedGroup, requestModel, metrics, responsesStateful, func(channel *dbmodel.Channel) (*websocket.Dialer, error) {
		return websocket.DefaultDialer, nil
	})
	if err != nil {
		t.Fatalf("expected pinned session, got error %v", err)
	}
	defer session.upstreamConn.Close()
	if session.channel == nil || session.channel.ID != secondChannel.ID {
		t.Fatalf("expected pinned second channel, got %#v", session.channel)
	}
	if session.usedKey.ID != secondChannel.Keys[0].ID {
		t.Fatalf("expected pinned key %d, got %d", secondChannel.Keys[0].ID, session.usedKey.ID)
	}
	if normalizeAffinityBaseURL(session.selectedBaseURL) != normalizeAffinityBaseURL(secondChannel.BaseUrls[0].URL) {
		t.Fatalf("expected pinned base url %s, got %s", secondChannel.BaseUrls[0].URL, session.selectedBaseURL)
	}
	if session.maxLifetime != secondChannel.GetResponsesWebsocketMaxLifetime() {
		t.Fatalf("expected max lifetime %s, got %s", secondChannel.GetResponsesWebsocketMaxLifetime(), session.maxLifetime)
	}
	if upstreamAuth != "Bearer upstream-key" {
		t.Fatalf("unexpected upstream authorization: %s", upstreamAuth)
	}
}

func TestEvaluateResponsesWebsocketProxyResult_IgnoresConfiguredLifetimeClose(t *testing.T) {
	err := evaluateResponsesWebsocketProxyResult("upstream_to_client", &responsesWebsocketRelayIOError{Op: "read", Err: &websocket.CloseError{Code: websocket.CloseGoingAway, Text: "timeout"}}, nil, true)
	if err != nil {
		t.Fatalf("expected lifetime close to be ignored, got %v", err)
	}
}

func TestChannelCreate_NormalizesResponsesWebsocketMaxLifetime(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	channel := &dbmodel.Channel{
		Name:                             "responses-ws-lifetime-default",
		Type:                             outbound.OutboundTypeOpenAIResponse,
		Enabled:                          true,
		BaseUrls:                         []dbmodel.BaseUrl{{URL: "https://example.invalid/v1"}},
		Keys:                             []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "upstream-key"}},
		ResponsesWebsocketMaxLifetimeSec: 0,
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	if channel.ResponsesWebsocketMaxLifetimeSec != dbmodel.DefaultResponsesWebsocketMaxLifetimeSec {
		t.Fatalf("expected normalized lifetime %d, got %d", dbmodel.DefaultResponsesWebsocketMaxLifetimeSec, channel.ResponsesWebsocketMaxLifetimeSec)
	}
	stored, err := op.ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet failed: %v", err)
	}
	if stored.GetResponsesWebsocketMaxLifetimeSec() != dbmodel.DefaultResponsesWebsocketMaxLifetimeSec {
		t.Fatalf("expected stored lifetime %d, got %d", dbmodel.DefaultResponsesWebsocketMaxLifetimeSec, stored.GetResponsesWebsocketMaxLifetimeSec())
	}
}

func TestChannelUpdate_UpdatesResponsesWebsocketMaxLifetime(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	channel := createRelayTestChannel(t, ctx, "responses-ws-update-lifetime", outbound.OutboundTypeOpenAIResponse, "https://example.invalid/v1")
	lifetime := 120
	updated, err := op.ChannelUpdate(&dbmodel.ChannelUpdateRequest{ID: channel.ID, ResponsesWebsocketMaxLifetimeSec: &lifetime}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate failed: %v", err)
	}
	if updated.GetResponsesWebsocketMaxLifetimeSec() != 120 {
		t.Fatalf("expected updated lifetime 120, got %d", updated.GetResponsesWebsocketMaxLifetimeSec())
	}
}

func TestHandleResponsesWebsocket_ClosesAfterConfiguredLifetime(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "responses-ws-lifetime-close"
	upstreamModel := "responses-ws-upstream"
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_1","object":"response","model":"responses-ws-upstream","status":"in_progress","output":[]}}`))
		time.Sleep(1500 * time.Millisecond)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-ws-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	lifetime := 1
	if _, err := op.ChannelUpdate(&dbmodel.ChannelUpdateRequest{ID: channel.ID, ResponsesWebsocketMaxLifetimeSec: &lifetime}, ctx); err != nil {
		t.Fatalf("ChannelUpdate failed: %v", err)
	}
	group := createResponsesWebsocketGroup(t, ctx, requestModel, true)
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: upstreamModel, Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	octopus := newResponsesWebsocketTestServer(apiKey.ID, requestModel)
	defer octopus.Close()

	conn, _, err := websocket.DefaultDialer.Dial(websocketURLFromHTTP(octopus.URL)+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("dial octopus websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"responses-ws-lifetime-close","input":"hi"}`)); err != nil {
		t.Fatalf("write first frame failed: %v", err)
	}
	_, _, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("expected initial response.created, got %v", err)
	}
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected websocket close after configured lifetime")
	}
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("expected close error, got %T: %v", err, err)
	}
	if closeErr.Code != websocket.CloseGoingAway {
		t.Fatalf("unexpected close code: %d", closeErr.Code)
	}
	if !strings.Contains(closeErr.Text, "max lifetime") {
		t.Fatalf("unexpected close reason: %s", closeErr.Text)
	}
}
