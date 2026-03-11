package anthropic

import (
	"context"
	"testing"
)

func TestMessagesInbound_TransformsNamedToolChoiceAndParallel(t *testing.T) {
	in := &MessagesInbound{}
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":64,"system":"sys","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"get_weather","disable_parallel_tool_use":true}}`)

	req, err := in.TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest err: %v", err)
	}
	if req.ToolChoice == nil || req.ToolChoice.NamedToolChoice == nil {
		t.Fatalf("named tool choice missing: %#v", req.ToolChoice)
	}
	if req.ToolChoice.NamedToolChoice.Type != "function" || req.ToolChoice.NamedToolChoice.Function.Name != "get_weather" {
		t.Fatalf("unexpected named tool choice: %#v", req.ToolChoice)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("unexpected parallel tool calls: %#v", req.ParallelToolCalls)
	}
}

func TestMessagesInbound_TransformsAnyToolChoiceToRequired(t *testing.T) {
	in := &MessagesInbound{}
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}}`)

	req, err := in.TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("TransformRequest err: %v", err)
	}
	if req.ToolChoice == nil || req.ToolChoice.ToolChoice == nil || *req.ToolChoice.ToolChoice != "required" {
		t.Fatalf("unexpected tool choice: %#v", req.ToolChoice)
	}
}
