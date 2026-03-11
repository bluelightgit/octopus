package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	inboundopenai "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	"github.com/bestruirui/octopus/internal/transformer/model"
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
	if msg0["role"].(string) != "developer" {
		t.Fatalf("developer role not preserved: %v", msg0["role"])
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

func TestChatOutbound_ResponsesExtraBody_DoesNotOverrideConvertedTools(t *testing.T) {
	stream := false
	prompt := "hi"

	req := &model.InternalLLMRequest{
		Model:        "gpt-4.1",
		Messages:     []model.Message{{Role: "user", Content: model.MessageContent{Content: &prompt}}},
		Stream:       &stream,
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Tools: []model.Tool{{
			Type: "function",
			Function: model.Function{
				Name:       "get_weather",
				Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			},
		}},
		ToolChoice: &model.ToolChoice{NamedToolChoice: &model.NamedToolChoice{
			Type: "function",
			Function: model.ToolFunction{
				Name: "get_weather",
			},
		}},
		ExtraBody: json.RawMessage(`{"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"get_weather"}}`),
	}

	outbound := &ChatOutbound{}
	httpReq, err := outbound.TransformRequest(context.Background(), req, "https://example.com/v1", "key")
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}

	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing or invalid: %v", payload["tools"])
	}
	firstTool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] invalid: %T", tools[0])
	}
	if _, ok := firstTool["function"].(map[string]any); !ok {
		t.Fatalf("expected chat tool schema with function object, got: %v", firstTool)
	}

	toolChoice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing or invalid: %v", payload["tool_choice"])
	}
	if _, ok := toolChoice["function"].(map[string]any); !ok {
		t.Fatalf("expected chat tool_choice schema with function object, got: %v", toolChoice)
	}
}

func TestResponseOutbound_ChatExtraBody_DoesNotOverrideConvertedTools(t *testing.T) {
	stream := false
	prompt := "hi"

	req := &model.InternalLLMRequest{
		Model:        "gpt-4.1",
		Messages:     []model.Message{{Role: "user", Content: model.MessageContent{Content: &prompt}}},
		Stream:       &stream,
		RawAPIFormat: model.APIFormatOpenAIChatCompletion,
		Tools: []model.Tool{{
			Type: "function",
			Function: model.Function{
				Name:       "get_weather",
				Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			},
		}},
		ToolChoice: &model.ToolChoice{NamedToolChoice: &model.NamedToolChoice{
			Type: "function",
			Function: model.ToolFunction{
				Name: "get_weather",
			},
		}},
		ExtraBody: json.RawMessage(`{"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`),
	}

	outbound := &ResponseOutbound{}
	httpReq, err := outbound.TransformRequest(context.Background(), req, "https://example.com/v1", "key")
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}

	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing or invalid: %v", payload["tools"])
	}
	firstTool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] invalid: %T", tools[0])
	}
	if _, ok := firstTool["name"].(string); !ok {
		t.Fatalf("expected responses tool schema with top-level name, got: %v", firstTool)
	}
	if _, exists := firstTool["function"]; exists {
		t.Fatalf("unexpected chat tool schema leaked into responses request: %v", firstTool)
	}

	toolChoice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing or invalid: %v", payload["tool_choice"])
	}
	if _, ok := toolChoice["name"].(string); !ok {
		t.Fatalf("expected responses tool_choice schema with top-level name, got: %v", toolChoice)
	}
	if _, exists := toolChoice["function"]; exists {
		t.Fatalf("unexpected chat tool_choice schema leaked into responses request: %v", toolChoice)
	}
}

func TestResponseInbound_ConvertsInputAudioAndFile(t *testing.T) {
	raw := []byte(`{"model":"gpt-4.1","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_audio","input_audio":{"data":"YmFzZTY0LWF1ZGlv","format":"mp3"}},{"type":"input_file","file_id":"file-123","file_data":"ZmlsZS1kYXRh","file_url":"https://example.com/file.txt","filename":"note.txt"}]}]}`)

	inbound := &inboundopenai.ResponseInbound{}
	internalReq, err := inbound.TransformRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}

	if len(internalReq.Messages) != 1 {
		t.Fatalf("unexpected messages len: %d", len(internalReq.Messages))
	}
	parts := internalReq.Messages[0].Content.MultipleContent
	if len(parts) != 3 {
		t.Fatalf("unexpected content part len: %d", len(parts))
	}
	if parts[1].Type != "input_audio" || parts[1].Audio == nil {
		t.Fatalf("input_audio part missing: %#v", parts[1])
	}
	if parts[1].Audio.Format != "mp3" || parts[1].Audio.Data != "YmFzZTY0LWF1ZGlv" {
		t.Fatalf("unexpected audio payload: %#v", parts[1].Audio)
	}
	if parts[2].Type != "file" || parts[2].File == nil {
		t.Fatalf("file part missing: %#v", parts[2])
	}
	if parts[2].File.Filename != "note.txt" || parts[2].File.FileData != "ZmlsZS1kYXRh" {
		t.Fatalf("unexpected file payload: %#v", parts[2].File)
	}
	if parts[2].File.FileID == nil || *parts[2].File.FileID != "file-123" {
		t.Fatalf("unexpected file_id: %#v", parts[2].File.FileID)
	}
	if parts[2].File.FileURL == nil || *parts[2].File.FileURL != "https://example.com/file.txt" {
		t.Fatalf("unexpected file_url: %#v", parts[2].File.FileURL)
	}
}

func TestResponseOutbound_ConvertsInputAudioAndFile(t *testing.T) {
	stream := false
	textValue := "hello"
	fileID := "file-123"
	fileURL := "https://example.com/file.txt"

	req := &model.InternalLLMRequest{
		Model:  "gpt-4.1",
		Stream: &stream,
		Messages: []model.Message{{
			Role: "user",
			Content: model.MessageContent{MultipleContent: []model.MessageContentPart{
				{Type: "text", Text: &textValue},
				{Type: "input_audio", Audio: &model.Audio{Format: "wav", Data: "YmFzZTY0LWF1ZGlv"}},
				{Type: "file", File: &model.File{Filename: "note.txt", FileData: "ZmlsZS1kYXRh", FileID: &fileID, FileURL: &fileURL}},
			}},
		}},
	}

	outbound := &ResponseOutbound{}
	httpReq, err := outbound.TransformRequest(context.Background(), req, "https://example.com/v1", "key")
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}

	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}

	input, ok := payload["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input missing or invalid: %v", payload["input"])
	}
	msg, ok := input[0].(map[string]any)
	if !ok {
		t.Fatalf("input[0] invalid: %T", input[0])
	}
	content, ok := msg["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("content missing or invalid: %v", msg["content"])
	}

	audioItem, ok := content[1].(map[string]any)
	if !ok || audioItem["type"] != "input_audio" {
		t.Fatalf("audio item invalid: %v", content[1])
	}
	audioPayload, ok := audioItem["input_audio"].(map[string]any)
	if !ok {
		t.Fatalf("input_audio payload invalid: %v", audioItem)
	}
	if audioPayload["format"] != "wav" || audioPayload["data"] != "YmFzZTY0LWF1ZGlv" {
		t.Fatalf("unexpected input_audio payload: %v", audioPayload)
	}

	fileItem, ok := content[2].(map[string]any)
	if !ok || fileItem["type"] != "input_file" {
		t.Fatalf("file item invalid: %v", content[2])
	}
	if fileItem["filename"] != "note.txt" || fileItem["file_data"] != "ZmlsZS1kYXRh" {
		t.Fatalf("unexpected input_file payload: %v", fileItem)
	}
	if fileItem["file_id"] != "file-123" || fileItem["file_url"] != "https://example.com/file.txt" {
		t.Fatalf("unexpected input_file refs: %v", fileItem)
	}
}

func TestResponsesInboundToChatOutbound_PreservesFunctionToolShape(t *testing.T) {
	raw := []byte(`{"model":"gpt-4.1","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"function","name":"get_weather"}}`)

	inbound := &inboundopenai.ResponseInbound{}
	internalReq, err := inbound.TransformRequest(context.Background(), raw)
	if err != nil {
		t.Fatalf("inbound TransformRequest failed: %v", err)
	}

	outbound := &ChatOutbound{}
	httpReq, err := outbound.TransformRequest(context.Background(), internalReq, "https://example.com/v1", "key")
	if err != nil {
		t.Fatalf("outbound TransformRequest failed: %v", err)
	}

	if got := httpReq.URL.Path; got != "/v1/chat/completions" {
		t.Fatalf("unexpected outbound path: %s", got)
	}

	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing or invalid: %v", payload["tools"])
	}
	firstTool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] invalid: %T", tools[0])
	}
	fn, ok := firstTool["function"].(map[string]any)
	if !ok {
		t.Fatalf("expected chat tool schema with function object, got: %v", firstTool)
	}
	if fn["name"] != "get_weather" {
		t.Fatalf("unexpected function.name: %v", fn["name"])
	}
	if _, exists := firstTool["name"]; exists {
		t.Fatalf("unexpected responses-style tool name leaked into chat schema: %v", firstTool)
	}

	toolChoice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing or invalid: %v", payload["tool_choice"])
	}
	choiceFn, ok := toolChoice["function"].(map[string]any)
	if !ok {
		t.Fatalf("expected chat tool_choice schema with function object, got: %v", toolChoice)
	}
	if choiceFn["name"] != "get_weather" {
		t.Fatalf("unexpected tool_choice.function.name: %v", choiceFn["name"])
	}
	if _, exists := toolChoice["name"]; exists {
		t.Fatalf("unexpected responses-style tool_choice name leaked into chat schema: %v", toolChoice)
	}
}
