package authropic

import (
	"encoding/json"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestConvertToAnthropicRequest_PreservesDeveloperAndNamedToolChoice(t *testing.T) {
	developerText := "developer instruction"
	systemText := "system instruction"
	userText := "hi"
	parallelToolCalls := false

	req := &model.InternalLLMRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []model.Message{
			{Role: "developer", Content: model.MessageContent{Content: &developerText}},
			{Role: "system", Content: model.MessageContent{Content: &systemText}},
			{Role: "user", Content: model.MessageContent{Content: &userText}},
		},
		Tools: []model.Tool{{
			Type: "function",
			Function: model.Function{
				Name:       "get_weather",
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
		ToolChoice: &model.ToolChoice{NamedToolChoice: &model.NamedToolChoice{
			Type:     "function",
			Function: model.ToolFunction{Name: "get_weather"},
		}},
		ParallelToolCalls: &parallelToolCalls,
	}

	got := convertToAnthropicRequest(req)
	if got.System == nil || len(got.System.MultiplePrompts) != 2 {
		t.Fatalf("unexpected system prompts: %#v", got.System)
	}
	if got.System.MultiplePrompts[0].Text != developerText {
		t.Fatalf("developer prompt not preserved: %#v", got.System.MultiplePrompts)
	}
	if got.System.MultiplePrompts[1].Text != systemText {
		t.Fatalf("system prompt not preserved: %#v", got.System.MultiplePrompts)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("unexpected converted messages: %#v", got.Messages)
	}
	if got.ToolChoice == nil {
		t.Fatalf("tool choice missing")
	}
	if got.ToolChoice.Type != "tool" {
		t.Fatalf("unexpected tool choice type: %#v", got.ToolChoice)
	}
	if got.ToolChoice.Name == nil || *got.ToolChoice.Name != "get_weather" {
		t.Fatalf("unexpected tool choice name: %#v", got.ToolChoice)
	}
	if got.ToolChoice.DisableParallelToolUse == nil || *got.ToolChoice.DisableParallelToolUse != true {
		t.Fatalf("unexpected parallel tool flag: %#v", got.ToolChoice)
	}
}

func TestConvertToAnthropicRequest_RequiredToolChoiceMapsToAny(t *testing.T) {
	userText := "hi"
	required := "required"
	req := &model.InternalLLMRequest{
		Model:    "claude-3-5-sonnet-20241022",
		Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &userText}}},
		Tools: []model.Tool{{
			Type:     "function",
			Function: model.Function{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		ToolChoice: &model.ToolChoice{ToolChoice: &required},
	}

	got := convertToAnthropicRequest(req)
	if got.ToolChoice == nil || got.ToolChoice.Type != "any" {
		t.Fatalf("required tool choice not mapped to any: %#v", got.ToolChoice)
	}
}
