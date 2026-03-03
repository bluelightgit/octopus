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

