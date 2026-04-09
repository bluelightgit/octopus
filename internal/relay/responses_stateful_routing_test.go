package relay

import (
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func TestParseResponsesRequestState(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-5.2",
		"previous_response_id": "resp_prev",
		"conversation": {"id": "conv_123"},
		"input": [{
			"type": "reasoning",
			"encrypted_content": "enc_a"
		}, {
			"type": "message",
			"content": [{"type": "input_text", "text": "hi"}],
			"metadata": {"encrypted_content": "enc_b"}
		}]
	}`)

	state := parseResponsesRequestState(raw)
	if state.PreviousResponseID != "resp_prev" {
		t.Fatalf("unexpected previous_response_id: %q", state.PreviousResponseID)
	}
	if state.ConversationID != "conv_123" {
		t.Fatalf("unexpected conversation id: %q", state.ConversationID)
	}
	if len(state.EncryptedContents) != 2 {
		t.Fatalf("expected 2 encrypted content items, got %#v", state.EncryptedContents)
	}
	if state.EncryptedContents[0] != "enc_a" || state.EncryptedContents[1] != "enc_b" {
		t.Fatalf("unexpected encrypted contents: %#v", state.EncryptedContents)
	}
}

func TestResponsesStatefulContextPinsExactRoute(t *testing.T) {
	group := dbmodel.Group{
		ID:                42,
		Name:              "gpt-5.2",
		RouteAffinityMode: dbmodel.GroupRouteAffinityModeAuto,
	}
	req := &transformerModel.InternalLLMRequest{
		RawAPIFormat: transformerModel.APIFormatOpenAIResponse,
		RawRequest:   []byte(`{"model":"gpt-5.2","previous_response_id":"resp_prev","input":"hi"}`),
	}

	ctx := buildResponsesStatefulRequestContext(group, 1001, "gpt-5.2", req)
	if ctx == nil {
		t.Fatal("expected responses stateful context")
	}
	if !ctx.HasContinuationState {
		t.Fatal("expected continuation state to be detected")
	}
	if ctx.HasPinnedRoute() {
		t.Fatal("expected no pinned route before memory is populated")
	}

	lookupKeys := ctx.AllLookupKeys()
	if len(lookupKeys) == 0 {
		t.Fatal("expected lookup keys")
	}
	rememberResponsesAffinityRoute(1001, lookupKeys, affinityRoute{
		ChannelID:    9,
		ChannelKeyID: 11,
		BaseURL:      "https://upstream-a.example/v1",
	})

	reloaded := buildResponsesStatefulRequestContext(group, 1001, "gpt-5.2", req)
	if reloaded == nil || !reloaded.HasPinnedRoute() {
		t.Fatal("expected pinned route after affinity memory is populated")
	}
	if reloaded.PinnedRoute.ChannelID != 9 || reloaded.PinnedRoute.ChannelKeyID != 11 {
		t.Fatalf("unexpected pinned route: %#v", reloaded.PinnedRoute)
	}
	if got := normalizeAffinityBaseURL(reloaded.PinnedRoute.BaseURL); got != "https://upstream-a.example/v1" {
		t.Fatalf("unexpected pinned base url: %q", got)
	}

	storeKey := responsesAffinityStoreKey(1001, lookupKeys[0])
	responsesAffinityStore.Delete(storeKey)
}

func TestResponsesAffinityTTLUsesAtLeastOneDay(t *testing.T) {
	group := dbmodel.Group{SessionKeepTime: int((2 * time.Hour) / time.Second)}
	if got := responsesAffinityTTL(group); got < 24*time.Hour {
		t.Fatalf("expected ttl >= 24h, got %s", got)
	}
}

func TestResponsesStatefulContextUsesUnifiedRouteAffinityMode(t *testing.T) {
	group := dbmodel.Group{ID: 42, Name: "gpt-5.2", RouteAffinityMode: dbmodel.GroupRouteAffinityModeOff}
	req := &transformerModel.InternalLLMRequest{
		RawAPIFormat: transformerModel.APIFormatOpenAIResponse,
		RawRequest:   []byte(`{"model":"gpt-5.2","previous_response_id":"resp_prev","input":"hi"}`),
	}
	if ctx := buildResponsesStatefulRequestContext(group, 1001, "gpt-5.2", req); ctx != nil {
		t.Fatal("expected route affinity off to disable responses stateful routing")
	}
}
