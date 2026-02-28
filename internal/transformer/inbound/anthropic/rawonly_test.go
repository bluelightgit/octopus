package anthropic

import "testing"

func TestMessagesInbound_RawOnlyFallback_MinimalRoutingFields(t *testing.T) {
	in := &MessagesInbound{}
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":"bad","messages":"bad","stream":true,"metadata":{"user_id":"u1"}}`)
	req, err := in.TransformRequest(nil, body)
	if err != nil {
		t.Fatalf("TransformRequest err: %v", err)
	}
	if !req.RawOnly {
		t.Fatalf("expected RawOnly")
	}
	if req.RawAPIFormat != "anthropic/messages" {
		t.Fatalf("RawAPIFormat: %s", req.RawAPIFormat)
	}
	if req.Model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("model: %s", req.Model)
	}
	if req.Stream == nil || *req.Stream != true {
		t.Fatalf("stream not parsed")
	}
	if req.MaxTokens == nil || *req.MaxTokens < 1 {
		t.Fatalf("max_tokens default not set")
	}
	if req.Metadata == nil || req.Metadata["user_id"] != "u1" {
		t.Fatalf("metadata user_id not preserved")
	}
	if len(req.RawRequest) == 0 {
		t.Fatalf("raw request missing")
	}
}
