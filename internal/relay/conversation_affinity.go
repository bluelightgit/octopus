package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

type conversationAffinityRequestContext struct {
	LookupKeys        []string
	SelectedLookupKey string
	PreferredRoute    *affinityRoute
}

type canonicalConversationEnvelope struct {
	Format         string                         `json:"format"`
	Messages       []canonicalConversationMessage `json:"messages"`
	Tools          []canonicalConversationTool    `json:"tools,omitempty"`
	ToolChoice     *canonicalToolChoice           `json:"tool_choice,omitempty"`
	ResponseFormat *canonicalResponseFormat       `json:"response_format,omitempty"`
}

type canonicalConversationMessage struct {
	Role               string                          `json:"role"`
	Name               string                          `json:"name,omitempty"`
	ToolCallID         string                          `json:"tool_call_id,omitempty"`
	Refusal            string                          `json:"refusal,omitempty"`
	Reasoning          string                          `json:"reasoning,omitempty"`
	ReasoningSignature string                          `json:"reasoning_signature,omitempty"`
	CacheControl       *canonicalCacheControl          `json:"cache_control,omitempty"`
	Content            []canonicalConversationContent  `json:"content,omitempty"`
	ToolCalls          []canonicalConversationToolCall `json:"tool_calls,omitempty"`
}

type canonicalConversationContent struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	ImageURL     string                 `json:"image_url,omitempty"`
	ImageDetail  string                 `json:"image_detail,omitempty"`
	AudioFormat  string                 `json:"audio_format,omitempty"`
	AudioData    string                 `json:"audio_data,omitempty"`
	FileName     string                 `json:"file_name,omitempty"`
	FileID       string                 `json:"file_id,omitempty"`
	FileURL      string                 `json:"file_url,omitempty"`
	FileData     string                 `json:"file_data,omitempty"`
	CacheControl *canonicalCacheControl `json:"cache_control,omitempty"`
}

type canonicalConversationTool struct {
	Type            string                 `json:"type"`
	Name            string                 `json:"name,omitempty"`
	Description     string                 `json:"description,omitempty"`
	Parameters      any                    `json:"parameters,omitempty"`
	ImageGeneration any                    `json:"image_generation,omitempty"`
	CacheControl    *canonicalCacheControl `json:"cache_control,omitempty"`
}

type canonicalConversationToolCall struct {
	ID           string                 `json:"id,omitempty"`
	Type         string                 `json:"type,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Arguments    string                 `json:"arguments,omitempty"`
	CacheControl *canonicalCacheControl `json:"cache_control,omitempty"`
}

type canonicalCacheControl struct {
	Type string `json:"type,omitempty"`
	TTL  string `json:"ttl,omitempty"`
}

type canonicalToolChoice struct {
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
}

type canonicalResponseFormat struct {
	Type       string `json:"type,omitempty"`
	JSONSchema any    `json:"json_schema,omitempty"`
}

var conversationAffinityStore sync.Map

func buildConversationAffinityRequestContext(group dbmodel.Group, apiKeyID int, requestModel string, req *transformerModel.InternalLLMRequest) *conversationAffinityRequestContext {
	if req == nil || req.RawOnly || group.SessionKeepTime <= 0 {
		return nil
	}
	if !supportsConversationPrefixAffinity(req.RawAPIFormat) {
		return nil
	}

	lookupKeys := buildConversationAffinityKeys(group.ID, requestModel, req)
	if len(lookupKeys) == 0 {
		return nil
	}

	ttl := time.Duration(group.SessionKeepTime) * time.Second
	preferredRoute, selectedLookupKey := getConversationAffinityRoute(apiKeyID, lookupKeys, ttl)
	return &conversationAffinityRequestContext{
		LookupKeys:        lookupKeys,
		SelectedLookupKey: selectedLookupKey,
		PreferredRoute:    preferredRoute,
	}
}

func supportsConversationPrefixAffinity(format transformerModel.APIFormat) bool {
	switch format {
	case transformerModel.APIFormatOpenAIChatCompletion, transformerModel.APIFormatAnthropicMessage:
		return true
	default:
		return false
	}
}

func buildConversationAffinityKeys(groupID int, requestModel string, req *transformerModel.InternalLLMRequest) []string {
	envelope, prefixes := buildConversationAffinityEnvelope(req)
	if len(prefixes) == 0 {
		return nil
	}

	substantiveCounts := make([]int, len(prefixes))
	count := 0
	for i, message := range prefixes {
		if isSubstantiveConversationMessage(message) {
			count++
		}
		substantiveCounts[i] = count
	}
	if count < 2 {
		return nil
	}

	lookupKeys := make([]string, 0, len(prefixes))
	for i := len(prefixes); i >= 1; i-- {
		if substantiveCounts[i-1] < 2 {
			continue
		}
		envelope.Messages = prefixes[:i]
		payload, err := json.Marshal(envelope)
		if err != nil || len(payload) == 0 {
			continue
		}
		digest := sha256.Sum256(payload)
		lookupKeys = append(lookupKeys, fmt.Sprintf(
			"group=%d|model=%s|format=%s|prefix=%d|sha=%s",
			groupID,
			requestModel,
			req.RawAPIFormat,
			i,
			hex.EncodeToString(digest[:]),
		))
	}
	return compactStrings(lookupKeys)
}

func buildConversationAffinityEnvelope(req *transformerModel.InternalLLMRequest) (canonicalConversationEnvelope, []canonicalConversationMessage) {
	envelope := canonicalConversationEnvelope{
		Format:         string(req.RawAPIFormat),
		Tools:          canonicalizeConversationTools(req.Tools),
		ToolChoice:     canonicalizeConversationToolChoice(req.ToolChoice),
		ResponseFormat: canonicalizeConversationResponseFormat(req.ResponseFormat),
	}
	messages := canonicalizeConversationMessages(req.Messages)
	return envelope, messages
}

func canonicalizeConversationMessages(messages []transformerModel.Message) []canonicalConversationMessage {
	result := make([]canonicalConversationMessage, 0, len(messages))
	hasConversationSignal := false
	for _, message := range messages {
		canonical := canonicalConversationMessage{
			Role:               strings.TrimSpace(strings.ToLower(message.Role)),
			Name:               sanitizeAffinityString(ptrStringValue(message.Name)),
			ToolCallID:         sanitizeAffinityString(ptrStringValue(message.ToolCallID)),
			Refusal:            sanitizeAffinityString(message.Refusal),
			Reasoning:          sanitizeAffinityString(message.GetReasoningContent()),
			ReasoningSignature: sanitizeAffinityString(ptrStringValue(message.ReasoningSignature)),
			CacheControl:       canonicalizeCacheControl(message.CacheControl),
			Content:            canonicalizeConversationContent(message.Content),
			ToolCalls:          canonicalizeConversationToolCalls(message.ToolCalls),
		}
		if canonical.Role != "system" && isSubstantiveConversationMessage(canonical) {
			hasConversationSignal = true
		}
		result = append(result, canonical)
	}
	if !hasConversationSignal {
		return nil
	}
	return result
}

func isSubstantiveConversationMessage(message canonicalConversationMessage) bool {
	return message.ToolCallID != "" || message.Refusal != "" || message.Reasoning != "" || message.ReasoningSignature != "" || len(message.Content) > 0 || len(message.ToolCalls) > 0
}

func canonicalizeConversationContent(content transformerModel.MessageContent) []canonicalConversationContent {
	parts := make([]canonicalConversationContent, 0, len(content.MultipleContent)+1)
	if content.Content != nil {
		parts = append(parts, canonicalConversationContent{
			Type: "text",
			Text: sanitizeAffinityString(*content.Content),
		})
	}
	for _, part := range content.MultipleContent {
		canonical := canonicalConversationContent{
			Type:         strings.TrimSpace(strings.ToLower(part.Type)),
			CacheControl: canonicalizeCacheControl(part.CacheControl),
		}
		if part.Text != nil {
			canonical.Text = sanitizeAffinityString(*part.Text)
		}
		if part.ImageURL != nil {
			canonical.ImageURL = sanitizeAffinityString(part.ImageURL.URL)
			canonical.ImageDetail = sanitizeAffinityString(ptrStringValue(part.ImageURL.Detail))
		}
		if part.Audio != nil {
			canonical.AudioFormat = sanitizeAffinityString(part.Audio.Format)
			canonical.AudioData = sanitizeAffinityString(part.Audio.Data)
		}
		if part.File != nil {
			canonical.FileName = sanitizeAffinityString(part.File.Filename)
			canonical.FileData = sanitizeAffinityString(part.File.FileData)
			canonical.FileID = sanitizeAffinityString(ptrStringValue(part.File.FileID))
			canonical.FileURL = sanitizeAffinityString(ptrStringValue(part.File.FileURL))
		}
		parts = append(parts, canonical)
	}
	return compactConversationContent(parts)
}

func compactConversationContent(parts []canonicalConversationContent) []canonicalConversationContent {
	result := make([]canonicalConversationContent, 0, len(parts))
	for _, part := range parts {
		if part.Type == "" {
			continue
		}
		result = append(result, part)
	}
	return result
}

func canonicalizeConversationTools(tools []transformerModel.Tool) []canonicalConversationTool {
	result := make([]canonicalConversationTool, 0, len(tools))
	for _, tool := range tools {
		canonical := canonicalConversationTool{
			Type:         strings.TrimSpace(strings.ToLower(tool.Type)),
			Name:         sanitizeAffinityString(tool.Function.Name),
			Description:  sanitizeAffinityString(tool.Function.Description),
			Parameters:   normalizeStructuredValue(tool.Function.Parameters),
			CacheControl: canonicalizeCacheControl(tool.CacheControl),
		}
		if tool.ImageGeneration != nil {
			canonical.ImageGeneration = normalizeStructuredValue(tool.ImageGeneration)
		}
		result = append(result, canonical)
	}
	return result
}

func canonicalizeConversationToolCalls(toolCalls []transformerModel.ToolCall) []canonicalConversationToolCall {
	result := make([]canonicalConversationToolCall, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		result = append(result, canonicalConversationToolCall{
			ID:           sanitizeAffinityString(toolCall.ID),
			Type:         sanitizeAffinityString(toolCall.Type),
			Name:         sanitizeAffinityString(toolCall.Function.Name),
			Arguments:    sanitizeAffinityString(toolCall.Function.Arguments),
			CacheControl: canonicalizeCacheControl(toolCall.CacheControl),
		})
	}
	return result
}

func canonicalizeConversationToolChoice(toolChoice *transformerModel.ToolChoice) *canonicalToolChoice {
	if toolChoice == nil {
		return nil
	}
	if toolChoice.ToolChoice != nil {
		return &canonicalToolChoice{Type: sanitizeAffinityString(*toolChoice.ToolChoice)}
	}
	if toolChoice.NamedToolChoice != nil {
		return &canonicalToolChoice{
			Type: sanitizeAffinityString(toolChoice.NamedToolChoice.Type),
			Name: sanitizeAffinityString(toolChoice.NamedToolChoice.Function.Name),
		}
	}
	return nil
}

func canonicalizeConversationResponseFormat(format *transformerModel.ResponseFormat) *canonicalResponseFormat {
	if format == nil {
		return nil
	}
	return &canonicalResponseFormat{
		Type:       sanitizeAffinityString(format.Type),
		JSONSchema: normalizeStructuredValue(format.JSONSchema),
	}
}

func canonicalizeCacheControl(cacheControl *transformerModel.CacheControl) *canonicalCacheControl {
	if cacheControl == nil {
		return nil
	}
	return &canonicalCacheControl{
		Type: sanitizeAffinityString(cacheControl.Type),
		TTL:  sanitizeAffinityString(cacheControl.TTL),
	}
}

func normalizeStructuredValue(value any) any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 {
		return sanitizeAffinityString(fmt.Sprint(value))
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return sanitizeAffinityString(string(raw))
	}
	return normalized
}

func ptrStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func sanitizeAffinityString(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > 2048 || strings.HasPrefix(trimmed, "data:") {
		return "sha256:" + hashAffinityString(trimmed)
	}
	return trimmed
}

func hashAffinityString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func conversationAffinityStoreKey(apiKeyID int, affinityKey string) string {
	return fmt.Sprintf("api_key=%d|%s", apiKeyID, strings.TrimSpace(affinityKey))
}

func getConversationAffinityRoute(apiKeyID int, lookupKeys []string, ttl time.Duration) (*affinityRoute, string) {
	for _, lookupKey := range lookupKeys {
		if strings.TrimSpace(lookupKey) == "" {
			continue
		}
		storeKey := conversationAffinityStoreKey(apiKeyID, lookupKey)
		stored, ok := conversationAffinityStore.Load(storeKey)
		if !ok {
			continue
		}
		route, ok := stored.(*affinityRoute)
		if !ok || route == nil {
			conversationAffinityStore.Delete(storeKey)
			continue
		}
		if ttl > 0 && time.Since(route.Timestamp) > ttl {
			conversationAffinityStore.Delete(storeKey)
			continue
		}
		copyRoute := *route
		return &copyRoute, lookupKey
	}
	return nil, ""
}

func rememberConversationAffinityRoute(apiKeyID int, lookupKeys []string, route affinityRoute) {
	if apiKeyID <= 0 {
		return
	}
	route.BaseURL = normalizeAffinityBaseURL(route.BaseURL)
	route.Timestamp = time.Now()
	for _, lookupKey := range compactStrings(lookupKeys) {
		conversationAffinityStore.Store(conversationAffinityStoreKey(apiKeyID, lookupKey), &affinityRoute{
			ChannelID:    route.ChannelID,
			ChannelKeyID: route.ChannelKeyID,
			BaseURL:      route.BaseURL,
			Timestamp:    route.Timestamp,
		})
	}
}
