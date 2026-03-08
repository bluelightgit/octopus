package openai

import (
	"encoding/json"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func requiresOpenAISameProtocolPassthrough(raw map[string]json.RawMessage, format model.APIFormat) bool {
	if len(raw) == 0 {
		return false
	}
	if requiresRawOnlyForOpenAITools(raw["tools"], format) {
		return true
	}
	if requiresRawOnlyForOpenAIToolChoice(raw["tool_choice"], format) {
		return true
	}
	return false
}

func requiresRawOnlyForOpenAITools(data json.RawMessage, format model.APIFormat) bool {
	if len(data) == 0 {
		return false
	}

	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(data, &tools); err != nil {
		return true
	}

	for _, tool := range tools {
		toolType := strings.ToLower(jsonString(tool["type"]))
		switch format {
		case model.APIFormatOpenAIChatCompletion:
			if toolType != "function" {
				return true
			}
			if len(tool["function"]) == 0 {
				return true
			}
		case model.APIFormatOpenAIResponse:
			switch toolType {
			case "function":
				if len(tool["name"]) == 0 {
					return true
				}
			case "image_generation":
			default:
				return true
			}
		default:
			return true
		}
	}
	return false
}

func requiresRawOnlyForOpenAIToolChoice(data json.RawMessage, format model.APIFormat) bool {
	if len(data) == 0 {
		return false
	}

	var mode string
	if err := json.Unmarshal(data, &mode); err == nil {
		switch strings.ToLower(mode) {
		case "auto", "none", "required":
			return false
		default:
			return true
		}
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return true
	}

	switch format {
	case model.APIFormatOpenAIChatCompletion:
		if strings.ToLower(jsonString(obj["type"])) != "function" {
			return true
		}
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(obj["function"], &fn); err != nil {
			return true
		}
		return jsonString(fn["name"]) == ""
	case model.APIFormatOpenAIResponse:
		if strings.ToLower(jsonString(obj["type"])) != "function" {
			return true
		}
		return jsonString(obj["name"]) == ""
	default:
		return true
	}
}

func jsonString(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s
}
