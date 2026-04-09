package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func setupRelayTestEnv(t *testing.T) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "relay-test.db")
	if err := db.InitDB("sqlite", dbPath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}

	ctx := context.Background()
	if err := op.RelayLogClear(ctx); err != nil {
		t.Fatalf("RelayLogClear failed: %v", err)
	}
	return ctx
}

func createRelayTestAPIKey(t *testing.T, ctx context.Context) *dbmodel.APIKey {
	t.Helper()
	apiKey := &dbmodel.APIKey{Name: "test-key", APIKey: "local-test-key", Enabled: true}
	if err := op.APIKeyCreate(apiKey, ctx); err != nil {
		t.Fatalf("APIKeyCreate failed: %v", err)
	}
	return apiKey
}

func createRelayTestChannel(t *testing.T, ctx context.Context, name string, channelType outbound.OutboundType, upstreamURL string) *dbmodel.Channel {
	t.Helper()
	channel := &dbmodel.Channel{
		Name:     name,
		Type:     channelType,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "upstream-key"}},
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	return channel
}

func createRelayTestGroupItem(t *testing.T, ctx context.Context, requestModel string, channelID int, upstreamModel string) {
	t.Helper()
	group := &dbmodel.Group{Name: requestModel, Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channelID, ModelName: upstreamModel, Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}
}

func lastRelayLog(t *testing.T, ctx context.Context) dbmodel.RelayLog {
	t.Helper()
	page := 1
	pageSize := 10
	logs, err := op.RelayLogList(ctx, nil, nil, page, pageSize)
	if err != nil {
		t.Fatalf("RelayLogList failed: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected relay logs, got none")
	}
	return logs[0]
}

func assertRelayLogContainsEventTypes(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, expected := range want {
		found := false
		for _, actual := range got {
			if actual == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected relay log event types to contain %q, got %#v", expected, got)
		}
	}
}

func assertRelayLogContainsTrace(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, expected := range want {
		found := false
		for _, actual := range got {
			if strings.Contains(actual, expected) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected relay execution trace to contain %q, got %#v", expected, got)
		}
	}
}

func TestRelayHandler_ResponsesToChatChannel_UsesChatToolSchema(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "relay-test.db")
	if err := db.InitDB("sqlite", dbPath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}

	ctx := context.Background()
	apiKey := &dbmodel.APIKey{
		Name:    "test-key",
		APIKey:  "local-test-key",
		Enabled: true,
	}
	if err := op.APIKeyCreate(apiKey, ctx); err != nil {
		t.Fatalf("APIKeyCreate failed: %v", err)
	}

	requestModel := "relay-test-model"
	upstreamModel := "relay-test-model-upstream"
	channelName := "relay-test-openai-chat"

	var gotPath string
	var gotPayload map[string]any

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to read body"}}`))
			return
		}
		if err := json.Unmarshal(body, &gotPayload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid request json"}}`))
			return
		}

		tools, ok := gotPayload["tools"].([]any)
		if !ok || len(tools) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"tools missing"}}`))
			return
		}
		firstTool, ok := tools[0].(map[string]any)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"tools.0 invalid"}}`))
			return
		}
		if _, ok := firstTool["function"].(map[string]any); !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"'function' is a required property, expected an object - 'tools.0'","type":"invalid_request_error"}}`))
			return
		}
		if _, exists := firstTool["name"]; exists {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected responses tool shape"}}`))
			return
		}

		toolChoice, ok := gotPayload["tool_choice"].(map[string]any)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"tool_choice invalid"}}`))
			return
		}
		if _, ok := toolChoice["function"].(map[string]any); !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"tool_choice.function missing"}}`))
			return
		}
		if _, exists := toolChoice["name"]; exists {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected responses tool_choice shape"}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1730000000,"model":"relay-test-model-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	channel := &dbmodel.Channel{
		Name:    channelName,
		Type:    outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{
			{URL: upstream.URL + "/v1"},
		},
		Keys: []dbmodel.ChannelKey{
			{Enabled: true, ChannelKey: "upstream-key"},
		},
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	group := &dbmodel.Group{
		Name: requestModel,
		Mode: dbmodel.GroupModeFailover,
	}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}

	if err := op.GroupItemAdd(&dbmodel.GroupItem{
		GroupID:   group.ID,
		ChannelID: channel.ID,
		ModelName: upstreamModel,
		Priority:  1,
		Weight:    1,
	}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	requestBody := []byte(`{"model":"relay-test-model","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"function","name":"get_weather"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("unexpected upstream path: %s", gotPath)
	}

	var respBody map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("response unmarshal failed: %v, body=%s", err, w.Body.String())
	}
	if object, _ := respBody["object"].(string); object != "response" {
		t.Fatalf("unexpected response object: %v", respBody["object"])
	}
	if modelName, _ := respBody["model"].(string); modelName != upstreamModel {
		t.Fatalf("unexpected response model: %v", respBody["model"])
	}
}

func TestRelayHandler_AnthropicChannel_PreservesBetaHeaderAndProviderKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "relay-anthropic-test.db")
	if err := db.InitDB("sqlite", dbPath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}

	ctx := context.Background()
	apiKey := &dbmodel.APIKey{Name: "test-key", APIKey: "local-test-key", Enabled: true}
	if err := op.APIKeyCreate(apiKey, ctx); err != nil {
		t.Fatalf("APIKeyCreate failed: %v", err)
	}

	requestModel := "claude-request-model"
	upstreamModel := "claude-3-5-sonnet-20241022"

	var gotAPIKey string
	var gotBeta string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-API-Key")
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	channel := &dbmodel.Channel{
		Name:     "anthropic-channel",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL + "/v1"}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "provider-upstream-key"}},
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	group := &dbmodel.Group{Name: requestModel, Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: upstreamModel, Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	requestBody := []byte(`{"model":"claude-request-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "client-should-not-pass-through")
	req.Header.Set("Anthropic-Beta", "tools-2024-04-04")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)
	c.Set("request_type", "anthropic")

	Handler(inbound.InboundTypeAnthropic, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if gotAPIKey != "provider-upstream-key" {
		t.Fatalf("expected upstream provider key, got %q", gotAPIKey)
	}
	if gotBeta != "tools-2024-04-04" {
		t.Fatalf("expected anthropic beta header to pass through, got %q", gotBeta)
	}
}

func TestRelayHandler_AnthropicChannel_RejectsUnsupportedInputFile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "relay-test.db")
	if err := db.InitDB("sqlite", dbPath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}

	ctx := context.Background()
	apiKey := &dbmodel.APIKey{Name: "test-key", APIKey: "local-test-key", Enabled: true}
	if err := op.APIKeyCreate(apiKey, ctx); err != nil {
		t.Fatalf("APIKeyCreate failed: %v", err)
	}

	requestModel := "relay-anthropic-incompatible"
	upstreamModel := "claude-3-5-sonnet-20241022"
	channelName := "relay-test-anthropic"
	upstreamCalls := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"should not be called"}}`))
	}))
	defer upstream.Close()

	channel := &dbmodel.Channel{
		Name:     channelName,
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "provider-key"}},
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	group := &dbmodel.Group{Name: requestModel, Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: upstreamModel, Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	requestBody := []byte(`{"model":"relay-anthropic-incompatible","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-123","filename":"note.txt","file_data":"ZmlsZS1kYXRh"}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream should not be called, got %d", upstreamCalls)
	}
}

func TestRelayHandler_PreferSameProtocol_FallsBackAfterFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "relay-routing-fallback.db")
	if err := db.InitDB("sqlite", dbPath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}

	ctx := context.Background()
	apiKey := &dbmodel.APIKey{Name: "test-key", APIKey: "local-test-key", Enabled: true}
	if err := op.APIKeyCreate(apiKey, ctx); err != nil {
		t.Fatalf("APIKeyCreate failed: %v", err)
	}

	failedSameProtocolCalls := 0
	fallbackCalls := 0

	sameProtocolUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failedSameProtocolCalls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"same-protocol failed"}}`))
	}))
	defer sameProtocolUpstream.Close()

	fallbackUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1730000000,"model":"upstream-chat-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer fallbackUpstream.Close()

	responsesChannel := &dbmodel.Channel{Name: "responses-channel", Type: outbound.OutboundTypeOpenAIResponse, Enabled: true, BaseUrls: []dbmodel.BaseUrl{{URL: sameProtocolUpstream.URL + "/v1"}}, Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "responses-key"}}}
	chatChannel := &dbmodel.Channel{Name: "chat-channel", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, BaseUrls: []dbmodel.BaseUrl{{URL: fallbackUpstream.URL + "/v1"}}, Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "chat-key"}}}
	if err := op.ChannelCreate(responsesChannel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	if err := op.ChannelCreate(chatChannel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	group := &dbmodel.Group{Name: "routing-test-model", Mode: dbmodel.GroupModeFailover, PreferredProtocolFamily: dbmodel.GroupProtocolFamilyOpenAIResponses, ProtocolRoutingMode: dbmodel.GroupProtocolRoutingModePreferSameProtocol}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: responsesChannel.ID, ModelName: "upstream-response-model", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: chatChannel.ID, ModelName: "upstream-chat-model", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	requestBody := []byte(`{"model":"routing-test-model","stream":false,"input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if failedSameProtocolCalls != 1 {
		t.Fatalf("expected same protocol upstream to be tried once, got %d", failedSameProtocolCalls)
	}
	if fallbackCalls != 1 {
		t.Fatalf("expected cross protocol fallback once, got %d", fallbackCalls)
	}
}

func TestRelayHandler_SameProtocolOnly_RejectsWhenNoSameProtocolChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "relay-routing-same-only.db")
	if err := db.InitDB("sqlite", dbPath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}

	ctx := context.Background()
	apiKey := &dbmodel.APIKey{Name: "test-key", APIKey: "local-test-key", Enabled: true}
	if err := op.APIKeyCreate(apiKey, ctx); err != nil {
		t.Fatalf("APIKeyCreate failed: %v", err)
	}

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	chatChannel := &dbmodel.Channel{Name: "chat-channel", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL + "/v1"}}, Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "chat-key"}}}
	if err := op.ChannelCreate(chatChannel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	group := &dbmodel.Group{Name: "routing-test-same-only", Mode: dbmodel.GroupModeFailover, PreferredProtocolFamily: dbmodel.GroupProtocolFamilyOpenAIResponses, ProtocolRoutingMode: dbmodel.GroupProtocolRoutingModeSameProtocolOnly}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: chatChannel.ID, ModelName: "upstream-chat-model", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	requestBody := []byte(`{"model":"routing-test-same-only","stream":false,"input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream should not be called, got %d", upstreamCalls)
	}
}

func TestRelayHandler_StreamEOFBeforeFirstEvent_Fails(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "stream-eof-before-first-event"
	upstreamModel := "stream-eof-before-first-event-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"stream-eof-before-first-event","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream ended before first event") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
	if !strings.Contains(logItem.ResponseContent, `"message":"all channels failed"`) {
		t.Fatalf("expected local error response body to be logged, got %q", logItem.ResponseContent)
	}
	assertRelayLogContainsTrace(t, logItem.ExecutionTrace,
		"stream_reader_closed:",
		"local_error_response: status=502 message=all channels failed",
	)
}

func TestRelayHandler_StreamEOFBeforeTerminalEvent_Fails(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "stream-eof-before-terminal"
	upstreamModel := "stream-eof-before-terminal-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"upstream\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"stream-eof-before-terminal","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream ended unexpectedly before terminal event") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
	if !strings.Contains(logItem.ResponseContent, `"message":"all channels failed"`) {
		t.Fatalf("expected local error response body to be logged, got %q", logItem.ResponseContent)
	}
	assertRelayLogContainsTrace(t, logItem.ExecutionTrace,
		"stream_reader_closed:",
		"local_error_response: status=502 message=all channels failed",
	)
}

func TestRelayHandler_StreamTerminalEvent_Succeeds(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "stream-terminal-success"
	upstreamModel := "stream-terminal-success-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"upstream\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"upstream\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"stream-terminal-success","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("expected terminal stream body, got %q", w.Body.String())
	}
	logItem := lastRelayLog(t, ctx)
	if logItem.Error != "" {
		t.Fatalf("expected empty relay error, got %q", logItem.Error)
	}
	if logItem.TerminalSeen == nil || !*logItem.TerminalSeen {
		t.Fatalf("expected terminal seen diagnostic to be true, got %v", logItem.TerminalSeen)
	}
	if logItem.FailureStage != nil {
		t.Fatalf("expected empty failure stage, got %v", logItem.FailureStage)
	}
}

func TestRelayHandler_StreamIdleTimeout_Fails(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	requestModel := "stream-idle-timeout"
	upstreamModel := "stream-idle-timeout-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"upstream\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"stream-idle-timeout","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream idle timeout") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
	if !strings.Contains(logItem.ResponseContent, `"message":"all channels failed"`) {
		t.Fatalf("expected local error response body to be logged, got %q", logItem.ResponseContent)
	}
	assertRelayLogContainsTrace(t, logItem.ExecutionTrace,
		"stream_timeout: idle",
		"local_error_response: status=502 message=all channels failed",
	)
}

func TestRelayHandler_NonStreamTimeout_Fails(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevTimeout := relayNonStreamTimeout
	relayNonStreamTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayNonStreamTimeout = prevTimeout })

	requestModel := "non-stream-timeout"
	upstreamModel := "non-stream-timeout-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1730000000,"status":"completed","model":"upstream","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"non-stream-timeout","stream":false,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream non-stream timeout") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
	if logItem.UpstreamFirstEventMs != nil || logItem.ClientFirstWriteMs != nil || logItem.UpstreamEventCount != nil || logItem.ClientChunkCount != nil || logItem.TerminalSeen != nil || logItem.FailureStage != nil {
		t.Fatalf("expected non-stream request to omit stream diagnostics, got %+v", logItem)
	}
}

func TestRelayHandler_ResponsesPreludeTimeout_FallsBackBeforeClientWrite(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 80 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	requestModel := "responses-prelude-fallback"

	firstCalls := 0
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stalled\",\"model\":\"upstream-one\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer firstUpstream.Close()

	secondCalls := 0
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"model\":\"upstream-two\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"model\":\"upstream-two\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer secondUpstream.Close()

	firstChannel := createRelayTestChannel(t, ctx, "responses-stalled", outbound.OutboundTypeOpenAIResponse, firstUpstream.URL+"/v1")
	secondChannel := createRelayTestChannel(t, ctx, "responses-good", outbound.OutboundTypeOpenAIResponse, secondUpstream.URL+"/v1")

	group := &dbmodel.Group{Name: requestModel, Mode: dbmodel.GroupModeFailover, PreferredProtocolFamily: dbmodel.GroupProtocolFamilyOpenAIResponses, ProtocolRoutingMode: dbmodel.GroupProtocolRoutingModePreferSameProtocol}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "upstream-one", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "upstream-two", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"responses-prelude-fallback","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("expected both channels to be attempted once, got first=%d second=%d", firstCalls, secondCalls)
	}
	body := w.Body.String()
	if strings.Contains(body, "resp_stalled") {
		t.Fatalf("unexpected prelude from stalled channel leaked to client: %q", body)
	}
	if !strings.Contains(body, "resp_ok") || !strings.Contains(body, "hello") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected fallback channel output, got %q", body)
	}
	logItem := lastRelayLog(t, ctx)
	if logItem.Error != "" {
		t.Fatalf("expected successful relay log, got error %q", logItem.Error)
	}
	if logItem.TotalAttempts != 2 {
		t.Fatalf("expected two attempts, got %d", logItem.TotalAttempts)
	}
}

func TestRelayHandler_ResponsesPreludeTimeout_FallsBackWithDefaultGuardWhenGroupTimeoutDisabled(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 120 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	prevPrelude := relayResponsesPreludeTimeout
	relayResponsesPreludeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayResponsesPreludeTimeout = prevPrelude })

	requestModel := "responses-prelude-default-guard"

	firstCalls := 0
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stalled\",\"model\":\"upstream-one\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(120 * time.Millisecond)
	}))
	defer firstUpstream.Close()

	secondCalls := 0
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"model\":\"upstream-two\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"model\":\"upstream-two\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer secondUpstream.Close()

	firstChannel := createRelayTestChannel(t, ctx, "responses-stalled-default-guard", outbound.OutboundTypeOpenAIResponse, firstUpstream.URL+"/v1")
	secondChannel := createRelayTestChannel(t, ctx, "responses-good-default-guard", outbound.OutboundTypeOpenAIResponse, secondUpstream.URL+"/v1")

	group := &dbmodel.Group{Name: requestModel, Mode: dbmodel.GroupModeFailover, PreferredProtocolFamily: dbmodel.GroupProtocolFamilyOpenAIResponses, ProtocolRoutingMode: dbmodel.GroupProtocolRoutingModePreferSameProtocol}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "upstream-one", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "upstream-two", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"responses-prelude-default-guard","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("expected both channels to be attempted once, got first=%d second=%d", firstCalls, secondCalls)
	}
	body := w.Body.String()
	if strings.Contains(body, "resp_stalled") {
		t.Fatalf("unexpected prelude from stalled channel leaked to client: %q", body)
	}
	if !strings.Contains(body, "resp_ok") || !strings.Contains(body, "hello") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected fallback channel output, got %q", body)
	}
	logItem := lastRelayLog(t, ctx)
	if logItem.Error != "" {
		t.Fatalf("expected successful relay log, got error %q", logItem.Error)
	}
	if logItem.TotalAttempts != 2 {
		t.Fatalf("expected two attempts, got %d", logItem.TotalAttempts)
	}
	if len(logItem.Attempts) == 0 || !strings.Contains(logItem.Attempts[0].Msg, "first meaningful output timeout after 50ms") {
		t.Fatalf("expected first attempt timeout guard message, got %+v", logItem.Attempts)
	}
}

func TestRelayHandler_StreamIdleTimeoutAfterClientWrite_RecordsDiagnostics(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	requestModel := "stream-idle-after-client-write"
	upstreamModel := "stream-idle-after-client-write-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"upstream\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"stream-idle-after-client-write","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `response.output_text.delta`) {
		t.Fatalf("expected partial streamed body, got %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `response.failed`) {
		t.Fatalf("expected synthetic failure trailer, got %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `data: [DONE]`) {
		t.Fatalf("expected done marker after failure trailer, got %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"message":"upstream stream idle timeout after 50ms"`) {
		t.Fatalf("expected failure message in trailer, got %q", w.Body.String())
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream idle timeout") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
	if logItem.UpstreamFirstEventMs == nil || *logItem.UpstreamFirstEventMs < 0 {
		t.Fatalf("expected upstream first event timestamp, got %v", logItem.UpstreamFirstEventMs)
	}
	if logItem.ClientFirstWriteMs == nil || *logItem.ClientFirstWriteMs < 0 {
		t.Fatalf("expected client first write timestamp, got %v", logItem.ClientFirstWriteMs)
	}
	if logItem.UpstreamEventCount == nil || *logItem.UpstreamEventCount < 2 {
		t.Fatalf("expected upstream event count >= 2, got %v", logItem.UpstreamEventCount)
	}
	if logItem.ClientChunkCount == nil || *logItem.ClientChunkCount < 2 {
		t.Fatalf("expected client chunk count >= 2, got %v", logItem.ClientChunkCount)
	}
	if logItem.TerminalSeen == nil {
		t.Fatalf("expected terminal seen diagnostic to be present")
	}
	if *logItem.TerminalSeen {
		t.Fatalf("expected terminal seen to be false")
	}
	if logItem.FailureStage == nil || *logItem.FailureStage != "stream_idle_timeout" {
		t.Fatalf("unexpected failure stage: %v", logItem.FailureStage)
	}
}

func TestRelayHandler_ResponsesToChatStreamIdleTimeoutAfterClientWrite_EmitsFailureTrailer(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	requestModel := "responses-to-chat-stream-idle-after-client-write"
	upstreamModel := "responses-to-chat-stream-idle-after-client-write-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "chat-channel", outbound.OutboundTypeOpenAIChat, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"responses-to-chat-stream-idle-after-client-write","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `response.output_text.delta`) {
		t.Fatalf("expected transformed responses delta, got %q", body)
	}
	if !strings.Contains(body, `response.failed`) {
		t.Fatalf("expected transformed responses failure trailer, got %q", body)
	}
	if !strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("expected done marker after failure trailer, got %q", body)
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream idle timeout") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
	if !strings.Contains(logItem.ResponseContent, `response.output_text.delta`) {
		t.Fatalf("expected streamed response body to be logged, got %q", logItem.ResponseContent)
	}
	assertRelayLogContainsTrace(t, logItem.ExecutionTrace,
		"stream_open:",
		"passthrough=false",
		"first_client_write:",
		"emit_failure_trailer:",
	)
}

func TestRelayHandler_OpenAIChatStreamIdleTimeoutAfterClientWrite_EmitsFailureTrailer(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	requestModel := "chat-stream-idle-after-client-write"
	upstreamModel := "chat-stream-idle-after-client-write-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "chat-channel", outbound.OutboundTypeOpenAIChat, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"chat-stream-idle-after-client-write","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIChat, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"hello"`) {
		t.Fatalf("expected partial chat chunk, got %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"error"`) {
		t.Fatalf("expected synthetic error finish_reason, got %q", body)
	}
	if !strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("expected done marker after failure trailer, got %q", body)
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream idle timeout") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
}

func TestRelayHandler_OpenAICompletionsStreamIdleTimeoutAfterClientWrite_EmitsFailureTrailer(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	requestModel := "completions-stream-idle-after-client-write"
	upstreamModel := "completions-stream-idle-after-client-write-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"cmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "completions-channel", outbound.OutboundTypeOpenAIChat, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader([]byte(`{"model":"completions-stream-idle-after-client-write","stream":true,"prompt":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAICompletions, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"text":"hello"`) {
		t.Fatalf("expected partial completions chunk, got %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"error"`) {
		t.Fatalf("expected synthetic error finish_reason, got %q", body)
	}
	if !strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("expected done marker after failure trailer, got %q", body)
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream idle timeout") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
}
func TestRelayHandler_AnthropicStreamIdleTimeoutAfterClientWrite_EmitsErrorEvent(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	requestModel := "anthropic-stream-idle-after-client-write"
	upstreamModel := "anthropic-stream-idle-after-client-write-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "anthropic-channel", outbound.OutboundTypeAnthropic, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"anthropic-stream-idle-after-client-write","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)
	c.Set("request_type", "anthropic")

	Handler(inbound.InboundTypeAnthropic, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `event: content_block_delta`) || !strings.Contains(body, `"text":"hello"`) {
		t.Fatalf("expected partial anthropic stream body, got %q", body)
	}
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected synthetic anthropic error event, got %q", body)
	}
	if !strings.Contains(body, `"message":"upstream stream idle timeout after 50ms"`) {
		t.Fatalf("expected timeout message in anthropic error event, got %q", body)
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream idle timeout") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
}

func TestRelayHandler_ResponsesUnknownPreludeEvent_FallsBackWithoutLeakingClientWrite(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 80 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	prevPrelude := relayResponsesPreludeTimeout
	relayResponsesPreludeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayResponsesPreludeTimeout = prevPrelude })

	requestModel := "responses-unknown-prelude-fallback"

	firstCalls := 0
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stalled\",\"model\":\"upstream-one\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.heartbeat\",\"id\":\"hb_1\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(150 * time.Millisecond)
	}))
	defer firstUpstream.Close()

	secondCalls := 0
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"model\":\"upstream-two\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"model\":\"upstream-two\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer secondUpstream.Close()

	firstChannel := createRelayTestChannel(t, ctx, "responses-unknown-prelude-stalled", outbound.OutboundTypeOpenAIResponse, firstUpstream.URL+"/v1")
	secondChannel := createRelayTestChannel(t, ctx, "responses-unknown-prelude-good", outbound.OutboundTypeOpenAIResponse, secondUpstream.URL+"/v1")

	group := &dbmodel.Group{Name: requestModel, Mode: dbmodel.GroupModeFailover, PreferredProtocolFamily: dbmodel.GroupProtocolFamilyOpenAIResponses, ProtocolRoutingMode: dbmodel.GroupProtocolRoutingModePreferSameProtocol}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: "upstream-one", Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: "upstream-two", Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("GroupItemAdd failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"responses-unknown-prelude-fallback","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("expected both channels to be attempted once, got first=%d second=%d", firstCalls, secondCalls)
	}
	body := w.Body.String()
	if strings.Contains(body, "response.heartbeat") || strings.Contains(body, "resp_stalled") {
		t.Fatalf("unexpected stalled prelude leaked to client: %q", body)
	}
	if !strings.Contains(body, "resp_ok") || !strings.Contains(body, "hello") {
		t.Fatalf("expected fallback channel output, got %q", body)
	}
	logItem := lastRelayLog(t, ctx)
	if logItem.Error != "" {
		t.Fatalf("expected successful relay log, got error %q", logItem.Error)
	}
	assertRelayLogContainsEventTypes(t, logItem.UpstreamEventTypes,
		"response.created",
		"response.heartbeat",
		"response.output_text.delta",
		"response.completed",
		"[DONE]",
	)
}

func TestRelayHandler_ResponsesUnknownEventAfterClientWrite_DoesNotKeepStreamAliveForever(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	prevIdle := relayStreamIdleTimeout
	relayStreamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { relayStreamIdleTimeout = prevIdle })

	requestModel := "responses-unknown-after-write"
	upstreamModel := "responses-unknown-after-write-upstream"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"upstream\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		for i := 0; i < 4; i++ {
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"type\":\"response.heartbeat\",\"id\":\"hb_1\"}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		time.Sleep(120 * time.Millisecond)
	}))
	defer upstream.Close()

	channel := createRelayTestChannel(t, ctx, "responses-channel", outbound.OutboundTypeOpenAIResponse, upstream.URL+"/v1")
	createRelayTestGroupItem(t, ctx, requestModel, channel.ID, upstreamModel)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"responses-unknown-after-write","stream":true,"input":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `response.output_text.delta`) {
		t.Fatalf("expected partial streamed body, got %q", body)
	}
	if !strings.Contains(body, `response.failed`) {
		t.Fatalf("expected synthetic failure trailer, got %q", body)
	}
	if !strings.Contains(body, `upstream stream idle timeout after 50ms`) {
		t.Fatalf("expected idle timeout message, got %q", body)
	}
	if strings.Contains(body, `response.heartbeat`) {
		t.Fatalf("unexpected unknown heartbeat event leaked to client: %q", body)
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.Error, "upstream stream idle timeout") {
		t.Fatalf("unexpected relay error: %s", logItem.Error)
	}
	assertRelayLogContainsEventTypes(t, logItem.UpstreamEventTypes,
		"response.created",
		"response.output_text.delta",
		"response.heartbeat",
	)
}

func TestRelayHandler_ConversationAffinityStrictStopsCrossRouteRetry(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	requestModel := "conversation-affinity-strict"
	var firstHits int32
	var secondHits int32

	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"strict pinned route failed"}}`))
	}))
	defer firstUpstream.Close()

	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":0,"model":"` + requestModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"fallback"},"finish_reason":"stop"}]}`))
	}))
	defer secondUpstream.Close()

	firstChannel := createRelayTestChannel(t, ctx, "chat-strict-a", outbound.OutboundTypeOpenAIChat, firstUpstream.URL+"/v1")
	secondChannel := createRelayTestChannel(t, ctx, "chat-strict-b", outbound.OutboundTypeOpenAIChat, secondUpstream.URL+"/v1")

	group := &dbmodel.Group{Name: requestModel, Mode: dbmodel.GroupModeFailover, SessionKeepTime: int((5 * time.Minute) / time.Second), RouteAffinityMode: dbmodel.GroupRouteAffinityModeStrict}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: firstChannel.ID, ModelName: requestModel, Priority: 1, Weight: 1}, ctx); err != nil {
		t.Fatalf("add first channel: %v", err)
	}
	if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: secondChannel.ID, ModelName: requestModel, Priority: 2, Weight: 1}, ctx); err != nil {
		t.Fatalf("add second channel: %v", err)
	}

	prefillReq := &transformerModel.InternalLLMRequest{
		RawAPIFormat: transformerModel.APIFormatOpenAIChatCompletion,
		Messages: []transformerModel.Message{
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("hello")}},
			{Role: "assistant", Content: transformerModel.MessageContent{Content: strPtr("hi")}},
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("continue")}},
		},
	}
	prefillCtx := buildConversationAffinityRequestContext(*group, apiKey.ID, requestModel, prefillReq)
	if prefillCtx == nil || len(prefillCtx.LookupKeys) == 0 {
		t.Fatal("expected conversation affinity lookup keys")
	}
	rememberConversationAffinityRoute(apiKey.ID, prefillCtx.LookupKeys, affinityRoute{ChannelID: firstChannel.ID, ChannelKeyID: firstChannel.Keys[0].ID, BaseURL: firstUpstream.URL + "/v1"})

	reqBody := []byte(`{"model":"` + requestModel + `","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"continue"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIChat, c)

	if got := atomic.LoadInt32(&firstHits); got != 1 {
		t.Fatalf("expected pinned channel to be tried once, got %d", got)
	}
	if got := atomic.LoadInt32(&secondHits); got != 0 {
		t.Fatalf("expected strict conversation affinity to block fallback, got %d hits", got)
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 from pinned route failure, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "fallback") {
		t.Fatalf("expected no fallback response, got %s", w.Body.String())
	}
}
