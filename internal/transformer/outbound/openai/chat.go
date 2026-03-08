package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

type ChatOutbound struct{}

func (o *ChatOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseUrl, key string) (*http.Request, error) {
	if request == nil {
		return nil, fmt.Errorf("request is nil")
	}
	// Same-protocol passthrough: preserve raw Chat Completions request fields.
	var body []byte
	var err error
	if request != nil && request.RawAPIFormat == model.APIFormatOpenAIChatCompletion && len(request.RawRequest) > 0 {
		body, err = rewriteChatCompletionsRequestBody(request.RawRequest, request.Model, request.Stream)
		if err != nil {
			return nil, fmt.Errorf("failed to rewrite chat completions request body: %w", err)
		}
	} else {
		// Avoid mutating the shared request (used for retries/metrics).
		copyReq := *request
		copyReq.Messages = append([]model.Message(nil), request.Messages...)
		if request.StreamOptions != nil {
			so := *request.StreamOptions
			copyReq.StreamOptions = &so
		}
		copyReq.ClearHelpFields()

		if copyReq.Stream != nil && *copyReq.Stream {
			if copyReq.StreamOptions == nil {
				copyReq.StreamOptions = &model.StreamOptions{IncludeUsage: true}
			} else if !copyReq.StreamOptions.IncludeUsage {
				copyReq.StreamOptions.IncludeUsage = true
			}
		}

		body, err = json.Marshal(&copyReq)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}

		// Preserve raw tool metadata from newer schemas when converting from
		// Responses API into Chat Completions.
		if request.RawAPIFormat == model.APIFormatOpenAIResponse {
			if merged, mergeErr := mergeExtraBodyIntoJSON(body, request.ExtraBody); mergeErr == nil {
				body = merged
			} else {
				return nil, fmt.Errorf("failed to merge extra body: %w", mergeErr)
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	accept := "application/json"
	if request.Stream != nil && *request.Stream {
		accept = "text/event-stream"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+key)

	parsedUrl, err := url.Parse(strings.TrimSuffix(baseUrl, "/"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse base url: %w", err)
	}
	parsedUrl.Path = parsedUrl.Path + "/chat/completions"
	req.URL = parsedUrl
	req.Method = http.MethodPost
	return req, nil
}

func rewriteChatCompletionsRequestBody(raw []byte, modelName string, stream *bool) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}

	if modelName != "" {
		obj["model"] = modelName
	}
	if stream != nil {
		obj["stream"] = *stream
	}

	// Preserve historical behavior: request usage in streaming mode so the relay
	// can compute costs from upstream usage fields.
	if stream != nil && *stream {
		so, ok := obj["stream_options"].(map[string]any)
		if !ok || so == nil {
			so = map[string]any{}
		}
		so["include_usage"] = true
		obj["stream_options"] = so
	}

	return json.Marshal(obj)
}

func (o *ChatOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if len(body) == 0 {
		return nil, fmt.Errorf("response body is empty")
	}

	var resp model.InternalLLMResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return &resp, nil
}

func (o *ChatOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
	if bytes.HasPrefix(eventData, []byte("[DONE]")) {
		return &model.InternalLLMResponse{
			Object: "[DONE]",
		}, nil
	}

	var errCheck struct {
		Error *model.ErrorDetail `json:"error"`
	}
	if err := json.Unmarshal(eventData, &errCheck); err == nil && errCheck.Error != nil {
		return nil, &model.ResponseError{
			Detail: *errCheck.Error,
		}
	}

	var resp model.InternalLLMResponse
	if err := json.Unmarshal(eventData, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stream chunk: %w", err)
	}
	return &resp, nil
}
