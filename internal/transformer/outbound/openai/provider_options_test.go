package openai

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	inboundopenai "github.com/bluelightgit/octopus/internal/transformer/inbound/openai"
)

func TestChatProviderOptionsSurviveInboundAndOutboundConversion(t *testing.T) {
	raw := []byte(`{
		"model":"deepseek-reasoner",
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"enabled","budget_tokens":1024},
		"chat_template_kwargs":{"enable_thinking":true}
	}`)

	inbound := &inboundopenai.ChatInbound{}
	request, err := inbound.TransformRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("inbound TransformRequest failed: %v", err)
	}
	if string(request.Thinking) != `{"type":"enabled","budget_tokens":1024}` {
		t.Fatalf("thinking = %s", request.Thinking)
	}
	if string(request.ChatTemplateKwargs) != `{"enable_thinking":true}` {
		t.Fatalf("chat_template_kwargs = %s", request.ChatTemplateKwargs)
	}

	outboundRequest, err := (&ChatOutbound{}).TransformRequest(context.Background(), request, "https://example.com/v1", "key")
	if err != nil {
		t.Fatalf("outbound TransformRequest failed: %v", err)
	}
	body, err := io.ReadAll(outboundRequest.Body)
	if err != nil {
		t.Fatalf("read outbound body failed: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal outbound body failed: %v", err)
	}
	var gotThinking, wantThinking any
	if err := json.Unmarshal(payload["thinking"], &gotThinking); err != nil {
		t.Fatalf("unmarshal outbound thinking failed: %v", err)
	}
	if err := json.Unmarshal(request.Thinking, &wantThinking); err != nil {
		t.Fatalf("unmarshal request thinking failed: %v", err)
	}
	if !reflect.DeepEqual(gotThinking, wantThinking) {
		t.Fatalf("outbound thinking = %s, want %s", payload["thinking"], request.Thinking)
	}
	var gotTemplateKwargs, wantTemplateKwargs any
	if err := json.Unmarshal(payload["chat_template_kwargs"], &gotTemplateKwargs); err != nil {
		t.Fatalf("unmarshal outbound chat_template_kwargs failed: %v", err)
	}
	if err := json.Unmarshal(request.ChatTemplateKwargs, &wantTemplateKwargs); err != nil {
		t.Fatalf("unmarshal request chat_template_kwargs failed: %v", err)
	}
	if !reflect.DeepEqual(gotTemplateKwargs, wantTemplateKwargs) {
		t.Fatalf("outbound chat_template_kwargs = %s, want %s", payload["chat_template_kwargs"], request.ChatTemplateKwargs)
	}
}
