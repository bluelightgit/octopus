package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/op"
	"github.com/bluelightgit/octopus/internal/transformer/inbound"
	transformerModel "github.com/bluelightgit/octopus/internal/transformer/model"
	"github.com/bluelightgit/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

func TestRelayHandler_ChannelSystemPromptRoleOverride(t *testing.T) {
	ctx := setupRelayTestEnv(t)
	apiKey := createRelayTestAPIKey(t, ctx)

	var receivedRoles []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusInternalServerError)
			return
		}
		var payload struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		receivedRoles = make([]string, 0, len(payload.Messages))
		for _, message := range payload.Messages {
			receivedRoles = append(receivedRoles, message.Role)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-role-override","object":"chat.completion","model":"role-override-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	channel := &dbmodel.Channel{
		Name:                     "role-override-channel",
		Type:                     outbound.OutboundTypeOpenAIChat,
		Enabled:                  true,
		BaseUrls:                 []dbmodel.BaseUrl{{URL: upstream.URL + "/v1"}},
		Keys:                     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "provider-key"}},
		SystemPromptRoleOverride: transformerModel.SystemPromptRoleOverrideSystem,
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	createRelayTestGroupItem(t, ctx, "role-override-model", channel.ID, "role-override-upstream")

	requestBody := []byte(`{"model":"role-override-model","stream":false,"messages":[{"role":"system","content":"system prompt"},{"role":"developer","content":"developer prompt"},{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set("api_key_id", apiKey.ID)

	Handler(inbound.InboundTypeOpenAIChat, c)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body: %s", w.Code, w.Body.String())
	}
	wantRoles := []string{"system", "system", "user"}
	if len(receivedRoles) != len(wantRoles) {
		t.Fatalf("received roles = %#v, want %#v", receivedRoles, wantRoles)
	}
	for i, want := range wantRoles {
		if receivedRoles[i] != want {
			t.Fatalf("received role[%d] = %q, want %q", i, receivedRoles[i], want)
		}
	}

	if err := op.RelayLogSaveDBTask(ctx); err != nil {
		t.Fatalf("flush relay log: %v", err)
	}
	logItem := lastRelayLog(t, ctx)
	if !strings.Contains(logItem.RequestContent, `"role":"developer"`) {
		t.Fatalf("stored request lost original developer role: %s", logItem.RequestContent)
	}
	assertRelayLogContainsTrace(t, logItem.ExecutionTrace, "system_prompt_role_override: system")
}

func TestSystemPromptRoleOverrideOnlyAppliesToOpenAIChat(t *testing.T) {
	if got := effectiveSystemPromptRoleOverride(outbound.OutboundTypeOpenAIChat, transformerModel.SystemPromptRoleOverrideDeveloper); got != transformerModel.SystemPromptRoleOverrideDeveloper {
		t.Fatalf("OpenAI Chat role override = %q, want developer", got)
	}
	if got := effectiveSystemPromptRoleOverride(outbound.OutboundTypeOpenAIResponse, transformerModel.SystemPromptRoleOverrideDeveloper); got != transformerModel.SystemPromptRoleOverrideAuto {
		t.Fatalf("OpenAI Responses role override = %q, want auto", got)
	}
	if got := effectiveSystemPromptRoleOverride(outbound.OutboundTypeAnthropic, transformerModel.SystemPromptRoleOverrideSystem); got != transformerModel.SystemPromptRoleOverrideAuto {
		t.Fatalf("Anthropic role override = %q, want auto", got)
	}
}
