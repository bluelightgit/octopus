package openai

import (
	"encoding/json"
	"testing"
)

func TestRewriteChatCompletionsRequestBody_PreservesUnknownFieldsAndForcesUsage(t *testing.T) {
	raw := []byte(`{"model":"gpt-4o","stream":false,"messages":[{"role":"developer","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"mcp"}}],"unknown":{"a":1}}`)
	stream := true

	out, err := rewriteChatCompletionsRequestBody(raw, "gpt-5", &stream)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}

	if obj["model"].(string) != "gpt-5" {
		t.Fatalf("model not rewritten: %v", obj["model"])
	}
	if obj["stream"].(bool) != true {
		t.Fatalf("stream not rewritten: %v", obj["stream"])
	}

	msgs := obj["messages"].([]any)
	msg0 := msgs[0].(map[string]any)
	if msg0["role"].(string) != "system" {
		t.Fatalf("developer role not converted: %v", msg0["role"])
	}

	so := obj["stream_options"].(map[string]any)
	if so["include_usage"].(bool) != true {
		t.Fatalf("include_usage not forced: %v", so["include_usage"])
	}

	if _, ok := obj["unknown"]; !ok {
		t.Fatalf("unknown field dropped")
	}
	if _, ok := obj["tools"]; !ok {
		t.Fatalf("tools field dropped")
	}
}

func TestRewriteResponsesRequestBody_PreservesUnknownFields(t *testing.T) {
	raw := []byte(`{"model":"gpt-4.1","stream":false,"input":"hi","tools":[{"type":"web_search"}],"unknown":{"b":2}}`)
	stream := true

	out, err := rewriteResponsesRequestBody(raw, "gpt-5", &stream)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	if obj["model"].(string) != "gpt-5" {
		t.Fatalf("model not rewritten: %v", obj["model"])
	}
	if obj["stream"].(bool) != true {
		t.Fatalf("stream not rewritten: %v", obj["stream"])
	}
	if _, ok := obj["unknown"]; !ok {
		t.Fatalf("unknown field dropped")
	}
	if _, ok := obj["tools"]; !ok {
		t.Fatalf("tools field dropped")
	}
}
