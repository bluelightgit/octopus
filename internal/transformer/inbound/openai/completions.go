package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// CompletionsInbound implements an OpenAI Completions-compatible inbound adapter.
// It normalizes legacy /v1/completions requests into the internal chat request shape.
//
// Notes:
// - This adapter is intended for non-streaming requests in the first iteration.
// - Octopus only supports n=1 behavior, consistent with InternalLLMRequest design.
type CompletionsInbound struct {
	storedResponse *model.InternalLLMResponse
}

type openAICompletionsRequest struct {
	Model            string      `json:"model"`
	Prompt           any         `json:"prompt"`
	MaxTokens        *int64      `json:"max_tokens,omitempty"`
	Temperature      *float64    `json:"temperature,omitempty"`
	TopP             *float64    `json:"top_p,omitempty"`
	PresencePenalty  *float64    `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64    `json:"frequency_penalty,omitempty"`
	User             *string     `json:"user,omitempty"`
	Stream           *bool       `json:"stream,omitempty"`
	Stop             *model.Stop `json:"stop,omitempty"`
}

func (i *CompletionsInbound) TransformRequest(ctx context.Context, body []byte) (*model.InternalLLMRequest, error) {
	var req openAICompletionsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if req.Stream != nil && *req.Stream {
		return nil, errors.New("streaming is not supported for /v1/completions")
	}

	promptText := ""
	switch v := req.Prompt.(type) {
	case string:
		promptText = v
	case []any:
		// Best-effort: join array prompts with newlines.
		for idx := range v {
			if s, ok := v[idx].(string); ok {
				if promptText != "" {
					promptText += "\n"
				}
				promptText += s
			}
		}
	default:
		return nil, fmt.Errorf("unsupported prompt type")
	}
	if promptText == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	msg := model.Message{Role: "user"}
	msg.Content.Content = &promptText

	internalReq := &model.InternalLLMRequest{
		Model:            req.Model,
		Messages:         []model.Message{msg},
		MaxTokens:        req.MaxTokens,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		PresencePenalty:  req.PresencePenalty,
		FrequencyPenalty: req.FrequencyPenalty,
		Stop:             req.Stop,
		User:             req.User,
		RawAPIFormat:     model.APIFormatOpenAICompletions,
		RawRequest:       body,
	}
	return internalReq, nil
}

func (i *CompletionsInbound) TransformResponse(ctx context.Context, response *model.InternalLLMResponse) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("response is nil")
	}
	i.storedResponse = response

	text := ""
	finishReason := ""
	if len(response.Choices) > 0 {
		choice := response.Choices[0]
		if choice.Message != nil && choice.Message.Content.Content != nil {
			text = *choice.Message.Content.Content
		}
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	created := response.Created
	if created == 0 {
		created = time.Now().Unix()
	}

	out := map[string]any{
		"id":      response.ID,
		"object":  "text_completion",
		"created": created,
		"model":   response.Model,
		"choices": []map[string]any{{
			"index":         0,
			"text":          text,
			"finish_reason": finishReason,
		}},
	}
	if response.Usage != nil {
		out["usage"] = response.Usage
	}

	return json.Marshal(out)
}

func (i *CompletionsInbound) TransformStream(ctx context.Context, stream *model.InternalLLMResponse) ([]byte, error) {
	return nil, errors.New("streaming is not supported for /v1/completions")
}

func (i *CompletionsInbound) GetInternalResponse(ctx context.Context) (*model.InternalLLMResponse, error) {
	return i.storedResponse, nil
}
