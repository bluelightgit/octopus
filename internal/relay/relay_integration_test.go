package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

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
