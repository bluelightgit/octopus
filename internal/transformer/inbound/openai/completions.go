package openai

import (
	"context"
	"encoding/json"
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
	streamID       string
	streamModel    string
	createdAt      int64
	streamChunks   []*model.InternalLLMResponse
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
	// Streaming is supported and will be converted into legacy Completions SSE.

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
		Stream:           req.Stream,
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
	if stream == nil {
		return nil, fmt.Errorf("stream is nil")
	}
	if stream.Object == "[DONE]" {
		return []byte("data: [DONE]\n\n"), nil
	}

	// Store the chunk for aggregation.
	i.streamChunks = append(i.streamChunks, stream)
	if i.streamID == "" && stream.ID != "" {
		i.streamID = stream.ID
	}
	if i.streamModel == "" && stream.Model != "" {
		i.streamModel = stream.Model
	}
	if i.createdAt == 0 && stream.Created != 0 {
		i.createdAt = stream.Created
	}

	deltaText := ""
	finishReason := any(nil)
	if len(stream.Choices) > 0 {
		choice := stream.Choices[0]
		if choice.Delta != nil {
			if choice.Delta.Content.Content != nil {
				deltaText = *choice.Delta.Content.Content
			}
			// Best-effort: map refusal to text for legacy clients.
			if deltaText == "" && choice.Delta.Refusal != "" {
				deltaText = choice.Delta.Refusal
			}
		}
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	// Filter out empty chunks unless they carry finish_reason or usage.
	if deltaText == "" && finishReason == nil && stream.Usage == nil {
		return nil, nil
	}

	created := i.createdAt
	if created == 0 {
		created = time.Now().Unix()
	}

	chunk := map[string]any{
		"id":      firstNonEmpty(i.streamID, stream.ID),
		"object":  "text_completion",
		"created": created,
		"model":   firstNonEmpty(i.streamModel, stream.Model),
		"choices": []map[string]any{{
			"index":         0,
			"text":          deltaText,
			"finish_reason": finishReason,
		}},
	}
	if stream.Usage != nil {
		chunk["usage"] = stream.Usage
	}

	b, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return []byte("data: " + string(b) + "\n\n"), nil
}

func (i *CompletionsInbound) EmitStreamFailure(ctx context.Context, cause error) ([]byte, error) {
	finishReason := "error"
	chunk, err := i.TransformStream(ctx, &model.InternalLLMResponse{
		Object: "chat.completion.chunk",
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	})
	if err != nil {
		return nil, err
	}
	done, err := i.TransformStream(ctx, &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		return nil, err
	}
	return append(chunk, done...), nil
}

func (i *CompletionsInbound) GetInternalResponse(ctx context.Context) (*model.InternalLLMResponse, error) {
	if i.storedResponse != nil {
		return i.storedResponse, nil
	}
	if len(i.streamChunks) == 0 {
		return nil, nil
	}

	// Aggregate stream chunks into a single InternalLLMResponse for metrics/logging.
	result := &model.InternalLLMResponse{Object: "chat.completion"}
	choicesMap := make(map[int]*model.Choice)
	for _, chunk := range i.streamChunks {
		if chunk == nil {
			continue
		}
		if chunk.ID != "" {
			result.ID = chunk.ID
		}
		if chunk.Model != "" {
			result.Model = chunk.Model
		}
		if chunk.Created != 0 {
			result.Created = chunk.Created
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			existingChoice, exists := choicesMap[choice.Index]
			if !exists {
				existingChoice = &model.Choice{Index: choice.Index, Message: &model.Message{}}
				choicesMap[choice.Index] = existingChoice
			}
			if choice.FinishReason != nil {
				existingChoice.FinishReason = choice.FinishReason
			}
			if choice.Delta != nil {
				delta := choice.Delta
				if delta.Role != "" {
					existingChoice.Message.Role = delta.Role
				}
				if delta.Content.Content != nil {
					if existingChoice.Message.Content.Content == nil {
						existingChoice.Message.Content.Content = new(string)
					}
					*existingChoice.Message.Content.Content += *delta.Content.Content
				}
			}
		}
	}

	result.Choices = make([]model.Choice, 0, len(choicesMap))
	for idx := 0; idx < len(choicesMap); idx++ {
		if choice, exists := choicesMap[idx]; exists {
			result.Choices = append(result.Choices, *choice)
		}
	}

	// Clear stored chunks after aggregation.
	i.streamChunks = nil
	i.storedResponse = result
	return result, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
