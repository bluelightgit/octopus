package outbound

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestValidateRequestCompatibility_AnthropicRejectsStructuredOutputs(t *testing.T) {
	jsonSchema := json.RawMessage(`{"type":"object"}`)
	req := &model.InternalLLMRequest{
		Model:          "claude-3-5-sonnet-20241022",
		Messages:       []model.Message{{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}}},
		ResponseFormat: &model.ResponseFormat{Type: "json_schema", JSONSchema: jsonSchema},
	}

	err := ValidateRequestCompatibility(OutboundTypeAnthropic, req)
	if err == nil || !strings.Contains(err.Error(), "structured response_format") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRequestCompatibility_GeminiRejectsRemoteImageURL(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "gemini-2.5-flash",
		Messages: []model.Message{{
			Role: "user",
			Content: model.MessageContent{MultipleContent: []model.MessageContentPart{{
				Type:     "image_url",
				ImageURL: &model.ImageURL{URL: "https://example.com/image.png"},
			}}},
		}},
	}

	err := ValidateRequestCompatibility(OutboundTypeGemini, req)
	if err == nil || !strings.Contains(err.Error(), "remote image_url") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func stringPtr(v string) *string { return &v }
