package openai

import (
	"testing"
)

func TestChatInbound_RawOnlyFallback_MinimalRoutingFields(t *testing.T) {
	in := &ChatInbound{}
	body := []byte(`{"model":"gpt-4o","messages":"invalid","stream":true,"tools":[{"type":"custom"}],"tool_choice":{"type":"custom","custom":{"name":"x"}}}`)
	req, err := in.TransformRequest(nil, body)
	if err != nil {
		t.Fatalf("TransformRequest err: %v", err)
	}
	if !req.RawOnly {
		t.Fatalf("expected RawOnly")
	}
	if req.RawAPIFormat != "openai/chat_completions" {
		t.Fatalf("RawAPIFormat: %s", req.RawAPIFormat)
	}
	if req.Model != "gpt-4o" {
		t.Fatalf("model: %s", req.Model)
	}
	if req.Stream == nil || *req.Stream != true {
		t.Fatalf("stream not parsed")
	}
	if len(req.RawRequest) == 0 {
		t.Fatalf("raw request missing")
	}
	if len(req.ExtraBody) == 0 {
		t.Fatalf("expected ExtraBody to preserve tools/tool_choice")
	}
}

func TestResponseInbound_RawOnlyFallback_MinimalRoutingFields(t *testing.T) {
	in := &ResponseInbound{}
	body := []byte(`{"model":"gpt-4.1","input":{},"stream":false,"tools":[{"type":"web_search"}]}`)
	// Force invalid shape by putting input as object (our struct expects string or array), so unmarshal fails.
	req, err := in.TransformRequest(nil, body)
	if err != nil {
		t.Fatalf("TransformRequest err: %v", err)
	}
	if !req.RawOnly {
		t.Fatalf("expected RawOnly")
	}
	if req.RawAPIFormat != "openai/responses" {
		t.Fatalf("RawAPIFormat: %s", req.RawAPIFormat)
	}
	if req.Model != "gpt-4.1" {
		t.Fatalf("model: %s", req.Model)
	}
	if req.Stream == nil || *req.Stream != false {
		t.Fatalf("stream not parsed as false")
	}
	if len(req.RawRequest) == 0 {
		t.Fatalf("raw request missing")
	}
	if len(req.ExtraBody) == 0 {
		t.Fatalf("expected ExtraBody to preserve tools/tool_choice")
	}
}

func TestChatInbound_AdvancedToolsRequireSameProtocolPassthrough(t *testing.T) {
	in := &ChatInbound{}
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":false,"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}},{"type":"custom","custom":{"name":"code_exec"}}],"tool_choice":{"type":"custom","custom":{"name":"code_exec"}}}`)
	req, err := in.TransformRequest(nil, body)
	if err != nil {
		t.Fatalf("TransformRequest err: %v", err)
	}
	if !req.RawOnly {
		t.Fatalf("expected RawOnly for advanced chat tools")
	}
	if len(req.ExtraBody) == 0 {
		t.Fatalf("expected ExtraBody to preserve advanced tools/tool_choice")
	}
}

func TestResponseInbound_AdvancedToolsRequireSameProtocolPassthrough(t *testing.T) {
	in := &ResponseInbound{}
	body := []byte(`{"model":"gpt-4.1","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}},{"type":"web_search"}],"tool_choice":{"type":"mcp","server_label":"docs"}}`)
	req, err := in.TransformRequest(nil, body)
	if err != nil {
		t.Fatalf("TransformRequest err: %v", err)
	}
	if !req.RawOnly {
		t.Fatalf("expected RawOnly for advanced responses tools")
	}
	if len(req.ExtraBody) == 0 {
		t.Fatalf("expected ExtraBody to preserve advanced tools/tool_choice")
	}
}

func TestResponseInbound_SupportedToolsStayConvertible(t *testing.T) {
	in := &ResponseInbound{}
	body := []byte(`{"model":"gpt-4.1","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}},{"type":"image_generation","quality":"low"}],"tool_choice":"required"}`)
	req, err := in.TransformRequest(nil, body)
	if err != nil {
		t.Fatalf("TransformRequest err: %v", err)
	}
	if req.RawOnly {
		t.Fatalf("did not expect RawOnly for supported responses tools")
	}
}
