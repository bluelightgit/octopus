package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/bluelightgit/octopus/internal/transformer/model"
)

func TestRewriteChatCompletionsRequestBodyWithRoleOverride(t *testing.T) {
	raw := []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"s"},{"role":"developer","content":"d"},{"role":"user","content":"u"}],"unknown":{"keep":true}}`)

	out, err := rewriteChatCompletionsRequestBodyWithRoleOverride(raw, "gpt-5", nil, model.SystemPromptRoleOverrideDeveloper)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	messages := payload["messages"].([]any)
	wantRoles := []string{"developer", "developer", "user"}
	for i, want := range wantRoles {
		got := messages[i].(map[string]any)["role"]
		if got != want {
			t.Fatalf("messages[%d].role = %v, want %s", i, got, want)
		}
	}
	if _, ok := payload["unknown"]; !ok {
		t.Fatal("unknown field was dropped")
	}
}

func TestRewriteChatCompletionsRequestBodyWithRoleOverrideRequiresMessages(t *testing.T) {
	_, err := rewriteChatCompletionsRequestBodyWithRoleOverride(
		[]byte(`{"model":"gpt-4o"}`),
		"gpt-5",
		nil,
		model.SystemPromptRoleOverrideSystem,
	)
	if err == nil {
		t.Fatal("expected missing messages to fail")
	}
}

func TestChatOutboundRoleOverrideAppliesToStructuredRequestOnly(t *testing.T) {
	systemText := "system prompt"
	developerText := "developer prompt"
	userText := "user prompt"
	stream := false
	request := &model.InternalLLMRequest{
		Model: "gpt-4o",
		Messages: []model.Message{
			{Role: "system", Content: model.MessageContent{Content: &systemText}},
			{Role: "developer", Content: model.MessageContent{Content: &developerText}},
			{Role: "user", Content: model.MessageContent{Content: &userText}},
		},
		Stream: &stream,
		TransformOptions: model.TransformOptions{
			SystemPromptRoleOverride: model.SystemPromptRoleOverrideSystem,
		},
	}

	httpRequest, err := (&ChatOutbound{}).TransformRequest(context.Background(), request, "https://example.com/v1", "key")
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}
	body, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var payload struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	wantRoles := []string{"system", "system", "user"}
	for i, want := range wantRoles {
		if payload.Messages[i].Role != want {
			t.Fatalf("messages[%d].role = %q, want %q", i, payload.Messages[i].Role, want)
		}
	}
	if request.Messages[1].Role != "developer" {
		t.Fatalf("original developer role = %q, want developer", request.Messages[1].Role)
	}
}

func TestChatOutboundRoleOverrideAppliesToRawPassthrough(t *testing.T) {
	stream := false
	request := &model.InternalLLMRequest{
		Model:        "gpt-4o",
		Stream:       &stream,
		RawAPIFormat: model.APIFormatOpenAIChatCompletion,
		RawRequest:   []byte(`{"model":"gpt-4o","messages":[{"role":"developer","content":"d"},{"role":"user","content":"u"}]}`),
		TransformOptions: model.TransformOptions{
			SystemPromptRoleOverride: model.SystemPromptRoleOverrideSystem,
		},
	}

	httpRequest, err := (&ChatOutbound{}).TransformRequest(context.Background(), request, "https://example.com/v1", "key")
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}
	body, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var payload struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if payload.Messages[0].Role != "system" || payload.Messages[1].Role != "user" {
		t.Fatalf("unexpected raw roles: %#v", payload.Messages)
	}
}
