package outbound

import (
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/xurl"
)

func ValidateRequestCompatibility(channelType OutboundType, req *model.InternalLLMRequest) error {
	if req == nil || req.RawOnly {
		return nil
	}

	switch channelType {
	case OutboundTypeAnthropic:
		return validateAnthropicCompatibility(req)
	case OutboundTypeGemini:
		return validateGeminiCompatibility(req)
	default:
		return nil
	}
}

func validateAnthropicCompatibility(req *model.InternalLLMRequest) error {
	if req.ResponseFormat != nil && req.ResponseFormat.Type != "" && req.ResponseFormat.Type != "text" {
		return incompatibleFeatureError("anthropic", "structured response_format")
	}
	if len(req.Modalities) > 0 || req.Audio != nil {
		return incompatibleFeatureError("anthropic", "output modalities/audio")
	}
	if unsupported := firstUnsupportedToolType(req, map[string]struct{}{"function": {}}); unsupported != "" {
		return incompatibleFeatureError("anthropic", "tool type "+unsupported)
	}
	for _, msg := range req.Messages {
		for _, part := range msg.Content.MultipleContent {
			switch part.Type {
			case "input_audio":
				return incompatibleFeatureError("anthropic", "input_audio content")
			case "file":
				return incompatibleFeatureError("anthropic", "input_file content")
			}
		}
	}
	return nil
}

func validateGeminiCompatibility(req *model.InternalLLMRequest) error {
	if unsupported := firstUnsupportedToolType(req, map[string]struct{}{"function": {}}); unsupported != "" {
		return incompatibleFeatureError("gemini", "tool type "+unsupported)
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_schema" && len(req.ResponseFormat.JSONSchema) > 0 && req.ResponseFormat.ExtractJSONSchemaObject() == nil {
		return incompatibleFeatureError("gemini", "invalid json_schema payload")
	}
	for _, msg := range req.Messages {
		for _, part := range msg.Content.MultipleContent {
			switch part.Type {
			case "image_url":
				if part.ImageURL != nil && part.ImageURL.URL != "" && xurl.ParseDataURL(part.ImageURL.URL) == nil {
					return incompatibleFeatureError("gemini", "remote image_url content")
				}
			case "file":
				if part.File == nil {
					continue
				}
				if part.File.FileData == "" {
					if part.File.FileID != nil && *part.File.FileID != "" {
						return incompatibleFeatureError("gemini", "input_file via file_id")
					}
					if part.File.FileURL != nil && *part.File.FileURL != "" {
						return incompatibleFeatureError("gemini", "input_file via file_url")
					}
					return incompatibleFeatureError("gemini", "empty input_file content")
				}
				if xurl.ParseDataURL(part.File.FileData) == nil {
					return incompatibleFeatureError("gemini", "input_file.file_data without data URL")
				}
			}
		}
	}
	return nil
}

func firstUnsupportedToolType(req *model.InternalLLMRequest, allowed map[string]struct{}) string {
	for _, tool := range req.Tools {
		if _, ok := allowed[strings.ToLower(tool.Type)]; ok {
			continue
		}
		if tool.Type == "" {
			return "<empty>"
		}
		return tool.Type
	}
	return ""
}

func incompatibleFeatureError(provider, feature string) error {
	return fmt.Errorf("request incompatible with %s channel: %s is not supported", provider, feature)
}
