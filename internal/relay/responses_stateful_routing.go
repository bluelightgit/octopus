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

const minResponsesAffinityTTL = 24 * time.Hour

type responsesStatefulRequestContext struct {
	Mode                 dbmodel.GroupResponsesStatefulRoutingMode
	LookupKeys           []string
	ObservedLookupKeys   []string
	SelectedLookupKey    string
	PinnedRoute          *affinityRoute
	HasContinuationState bool
	mu                   sync.Mutex
}

type responsesRequestState struct {
	PreviousResponseID string
	ConversationID     string
	EncryptedContents  []string
}

var responsesAffinityStore sync.Map

func buildResponsesStatefulRequestContext(group dbmodel.Group, apiKeyID int, requestModel string, req *transformerModel.InternalLLMRequest) *responsesStatefulRequestContext {
	if req == nil || req.RawAPIFormat != transformerModel.APIFormatOpenAIResponse {
		return nil
	}

	mode := group.GetResponsesStatefulRoutingMode()
	if mode == dbmodel.GroupResponsesStatefulRoutingModeOff {
		return nil
	}

	state := parseResponsesRequestState(req.RawRequest)
	lookupKeys := make([]string, 0, 6)
	if state.PreviousResponseID != "" {
		lookupKeys = append(lookupKeys, responsesAffinityKeyForResponseID(group.ID, requestModel, state.PreviousResponseID))
	}
	if state.ConversationID != "" {
		lookupKeys = append(lookupKeys, responsesAffinityKeyForConversation(group.ID, requestModel, state.ConversationID))
	}
	for _, encryptedContent := range state.EncryptedContents {
		lookupKeys = append(lookupKeys, responsesAffinityKeyForEncryptedContent(group.ID, requestModel, encryptedContent))
	}
	lookupKeys = compactStrings(lookupKeys)

	hasContinuationState := len(lookupKeys) > 0
	if mode == dbmodel.GroupResponsesStatefulRoutingModeStrict && len(lookupKeys) == 0 {
		lookupKeys = append(lookupKeys, responsesAffinityKeyForStrictFallback(group.ID, requestModel))
	}

	ttl := responsesAffinityTTL(group)
	pinnedRoute, selectedLookupKey := getResponsesAffinityRoute(apiKeyID, lookupKeys, ttl)

	return &responsesStatefulRequestContext{
		Mode:                 mode,
		LookupKeys:           lookupKeys,
		SelectedLookupKey:    selectedLookupKey,
		PinnedRoute:          pinnedRoute,
		HasContinuationState: hasContinuationState,
	}
}

func (c *responsesStatefulRequestContext) Enabled() bool {
	return c != nil
}

func (c *responsesStatefulRequestContext) HasPinnedRoute() bool {
	return c != nil && c.PinnedRoute != nil
}

func (c *responsesStatefulRequestContext) BlocksCrossRouteFailover() bool {
	if c == nil {
		return false
	}
	return c.Mode == dbmodel.GroupResponsesStatefulRoutingModeStrict || c.HasContinuationState
}

func (c *responsesStatefulRequestContext) ProtectionReason() string {
	if c == nil {
		return "disabled"
	}
	if c.HasPinnedRoute() {
		return "exact_route"
	}
	if c.Mode == dbmodel.GroupResponsesStatefulRoutingModeStrict {
		return "strict_mode"
	}
	if c.HasContinuationState {
		return "continuation_state"
	}
	return "disabled"
}

func (c *responsesStatefulRequestContext) ObserveLookupKey(key string) {
	if c == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.ObservedLookupKeys {
		if existing == key {
			return
		}
	}
	c.ObservedLookupKeys = append(c.ObservedLookupKeys, key)
}

func (c *responsesStatefulRequestContext) AllLookupKeys() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	merged := make([]string, 0, len(c.LookupKeys)+len(c.ObservedLookupKeys))
	merged = append(merged, c.LookupKeys...)
	merged = append(merged, c.ObservedLookupKeys...)
	return compactStrings(merged)
}

func observeResponsesAffinityFromPayload(groupID int, requestModel string, ctx *responsesStatefulRequestContext, payload []byte) {
	if ctx == nil || len(payload) == 0 {
		return
	}

	var data any
	if err := json.Unmarshal(payload, &data); err != nil {
		return
	}

	obj, ok := data.(map[string]any)
	if !ok {
		return
	}

	observeResponsesAffinityFromObject(groupID, requestModel, ctx, obj)
}

func observeResponsesAffinityFromInternalResponse(groupID int, requestModel string, ctx *responsesStatefulRequestContext, resp *transformerModel.InternalLLMResponse) {
	if ctx == nil || resp == nil {
		return
	}
	if strings.TrimSpace(resp.ID) != "" {
		ctx.ObserveLookupKey(responsesAffinityKeyForResponseID(groupID, requestModel, resp.ID))
	}
	for _, choice := range resp.Choices {
		if choice.Message != nil && choice.Message.ReasoningSignature != nil && strings.TrimSpace(*choice.Message.ReasoningSignature) != "" {
			ctx.ObserveLookupKey(responsesAffinityKeyForEncryptedContent(groupID, requestModel, *choice.Message.ReasoningSignature))
		}
		if choice.Delta != nil && choice.Delta.ReasoningSignature != nil && strings.TrimSpace(*choice.Delta.ReasoningSignature) != "" {
			ctx.ObserveLookupKey(responsesAffinityKeyForEncryptedContent(groupID, requestModel, *choice.Delta.ReasoningSignature))
		}
	}
}

func observeResponsesAffinityFromObject(groupID int, requestModel string, ctx *responsesStatefulRequestContext, obj map[string]any) {
	if obj == nil || ctx == nil {
		return
	}

	if objectType, _ := obj["object"].(string); strings.EqualFold(strings.TrimSpace(objectType), "response") {
		if responseID := jsonStringValue(obj["id"]); responseID != "" {
			ctx.ObserveLookupKey(responsesAffinityKeyForResponseID(groupID, requestModel, responseID))
		}
		if conversationID := jsonStringValue(obj["conversation"]); conversationID != "" {
			ctx.ObserveLookupKey(responsesAffinityKeyForConversation(groupID, requestModel, conversationID))
		}
		for _, encryptedContent := range extractEncryptedContents(obj) {
			ctx.ObserveLookupKey(responsesAffinityKeyForEncryptedContent(groupID, requestModel, encryptedContent))
		}
	}

	if responseObj, ok := obj["response"].(map[string]any); ok {
		observeResponsesAffinityFromObject(groupID, requestModel, ctx, responseObj)
	}
	if itemObj, ok := obj["item"].(map[string]any); ok {
		for _, encryptedContent := range extractEncryptedContents(itemObj) {
			ctx.ObserveLookupKey(responsesAffinityKeyForEncryptedContent(groupID, requestModel, encryptedContent))
		}
	}
}

func parseResponsesRequestState(raw []byte) responsesRequestState {
	state := responsesRequestState{}
	if len(raw) == 0 {
		return state
	}

	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return state
	}

	obj, ok := payload.(map[string]any)
	if !ok {
		return state
	}

	state.PreviousResponseID = jsonStringValue(obj["previous_response_id"])
	state.ConversationID = jsonStringValue(obj["conversation"])
	if state.ConversationID == "" {
		if conversationObj, ok := obj["conversation"].(map[string]any); ok {
			state.ConversationID = jsonStringValue(conversationObj["id"])
		}
	}
	state.EncryptedContents = extractEncryptedContents(obj)
	return state
}

func extractEncryptedContents(node any) []string {
	values := make([]string, 0, 2)
	collectEncryptedContents(node, &values)
	return compactStrings(values)
}

func collectEncryptedContents(node any, out *[]string) {
	switch v := node.(type) {
	case map[string]any:
		for key, value := range v {
			if strings.EqualFold(key, "encrypted_content") {
				if s := jsonStringValue(value); s != "" {
					*out = append(*out, s)
				}
				continue
			}
			collectEncryptedContents(value, out)
		}
	case []any:
		for _, item := range v {
			collectEncryptedContents(item, out)
		}
	}
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func jsonStringValue(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func hashEncryptedContent(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	h := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(h[:])
}

func responsesAffinityKeyForResponseID(groupID int, requestModel, responseID string) string {
	return fmt.Sprintf("group=%d|model=%s|response=%s", groupID, requestModel, strings.TrimSpace(responseID))
}

func responsesAffinityKeyForConversation(groupID int, requestModel, conversationID string) string {
	return fmt.Sprintf("group=%d|model=%s|conversation=%s", groupID, requestModel, strings.TrimSpace(conversationID))
}

func responsesAffinityKeyForEncryptedContent(groupID int, requestModel, encryptedContent string) string {
	return fmt.Sprintf("group=%d|model=%s|enc=%s", groupID, requestModel, hashEncryptedContent(encryptedContent))
}

func responsesAffinityKeyForStrictFallback(groupID int, requestModel string) string {
	return fmt.Sprintf("group=%d|model=%s|strict", groupID, requestModel)
}

func responsesAffinityTTL(group dbmodel.Group) time.Duration {
	ttl := minResponsesAffinityTTL
	if group.SessionKeepTime > 0 {
		candidate := time.Duration(group.SessionKeepTime) * time.Second
		if candidate > ttl {
			ttl = candidate
		}
	}
	return ttl
}

func responsesAffinityStoreKey(apiKeyID int, affinityKey string) string {
	return fmt.Sprintf("api_key=%d|%s", apiKeyID, strings.TrimSpace(affinityKey))
}

func getResponsesAffinityRoute(apiKeyID int, lookupKeys []string, ttl time.Duration) (*affinityRoute, string) {
	for _, lookupKey := range lookupKeys {
		if strings.TrimSpace(lookupKey) == "" {
			continue
		}
		storeKey := responsesAffinityStoreKey(apiKeyID, lookupKey)
		v, ok := responsesAffinityStore.Load(storeKey)
		if !ok {
			continue
		}
		route, ok := v.(*affinityRoute)
		if !ok || route == nil {
			responsesAffinityStore.Delete(storeKey)
			continue
		}
		if ttl > 0 && time.Since(route.Timestamp) > ttl {
			responsesAffinityStore.Delete(storeKey)
			continue
		}
		copyRoute := *route
		return &copyRoute, lookupKey
	}
	return nil, ""
}

func rememberResponsesAffinityRoute(apiKeyID int, lookupKeys []string, route affinityRoute) {
	if apiKeyID <= 0 {
		return
	}
	route.BaseURL = normalizeAffinityBaseURL(route.BaseURL)
	route.Timestamp = time.Now()
	for _, lookupKey := range compactStrings(lookupKeys) {
		responsesAffinityStore.Store(responsesAffinityStoreKey(apiKeyID, lookupKey), &affinityRoute{
			ChannelID:    route.ChannelID,
			ChannelKeyID: route.ChannelKeyID,
			BaseURL:      route.BaseURL,
			Timestamp:    route.Timestamp,
		})
	}
}
