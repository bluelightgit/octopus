package relay

import (
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func TestBuildConversationAffinityContextReusesHistoricalPrefix(t *testing.T) {
	group := dbmodel.Group{ID: 42, SessionKeepTime: int((30 * time.Minute) / time.Second), RouteAffinityMode: dbmodel.GroupRouteAffinityModeAuto}
	apiKeyID := 1001
	requestModel := "claude-sonnet-4"

	baseReq := &transformerModel.InternalLLMRequest{
		RawAPIFormat: transformerModel.APIFormatAnthropicMessage,
		Messages: []transformerModel.Message{
			{Role: "system", Content: transformerModel.MessageContent{Content: strPtr("You are a test assistant.")}},
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("hello")}},
			{Role: "assistant", Content: transformerModel.MessageContent{Content: strPtr("hi")}},
		},
	}

	ctx := buildConversationAffinityRequestContext(group, apiKeyID, requestModel, baseReq)
	if ctx == nil {
		t.Fatal("expected conversation affinity context")
	}
	if len(ctx.LookupKeys) < 2 {
		t.Fatalf("expected multiple lookup keys, got %d", len(ctx.LookupKeys))
	}

	rememberConversationAffinityRoute(apiKeyID, ctx.LookupKeys, affinityRoute{
		ChannelID:    7,
		ChannelKeyID: 11,
		BaseURL:      "https://upstream-a.example/v1",
	})

	nextTurnReq := &transformerModel.InternalLLMRequest{
		RawAPIFormat: transformerModel.APIFormatAnthropicMessage,
		Messages: []transformerModel.Message{
			{Role: "system", Content: transformerModel.MessageContent{Content: strPtr("You are a test assistant.")}},
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("hello")}},
			{Role: "assistant", Content: transformerModel.MessageContent{Content: strPtr("hi")}},
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("how are you")}},
		},
	}

	reloaded := buildConversationAffinityRequestContext(group, apiKeyID, requestModel, nextTurnReq)
	if reloaded == nil || reloaded.PreferredRoute == nil {
		t.Fatal("expected preferred route from historical prefix")
	}
	if reloaded.PreferredRoute.ChannelID != 7 || reloaded.PreferredRoute.ChannelKeyID != 11 {
		t.Fatalf("unexpected preferred route: %#v", reloaded.PreferredRoute)
	}
	if got := normalizeAffinityBaseURL(reloaded.PreferredRoute.BaseURL); got != "https://upstream-a.example/v1" {
		t.Fatalf("unexpected preferred base url: %q", got)
	}
}

func TestBuildConversationAffinityContextSkipsResponsesAndRawOnly(t *testing.T) {
	group := dbmodel.Group{ID: 42, SessionKeepTime: int((10 * time.Minute) / time.Second), RouteAffinityMode: dbmodel.GroupRouteAffinityModeAuto}

	responsesReq := &transformerModel.InternalLLMRequest{
		RawAPIFormat: transformerModel.APIFormatOpenAIResponse,
		Messages:     []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("hello")}}},
	}
	if ctx := buildConversationAffinityRequestContext(group, 1, "gpt-5.2", responsesReq); ctx != nil {
		t.Fatal("expected responses request to skip conversation affinity")
	}

	rawOnlyReq := &transformerModel.InternalLLMRequest{
		RawAPIFormat: transformerModel.APIFormatAnthropicMessage,
		RawOnly:      true,
		Messages:     []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("hello")}}},
	}
	if ctx := buildConversationAffinityRequestContext(group, 1, "claude-sonnet-4", rawOnlyReq); ctx != nil {
		t.Fatal("expected raw-only request to skip conversation affinity")
	}
}

func strPtr(s string) *string { return &s }

func TestBuildConversationAffinityContextDisabledWhenRouteAffinityOff(t *testing.T) {
	group := dbmodel.Group{ID: 42, SessionKeepTime: int((10 * time.Minute) / time.Second), RouteAffinityMode: dbmodel.GroupRouteAffinityModeOff}
	req := &transformerModel.InternalLLMRequest{
		RawAPIFormat: transformerModel.APIFormatAnthropicMessage,
		Messages:     []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("hello")}}},
	}
	if ctx := buildConversationAffinityRequestContext(group, 1, "claude-sonnet-4", req); ctx != nil {
		t.Fatal("expected route affinity off to disable conversation affinity")
	}
}

func TestConversationAffinityStrictBlocksCrossRouteFailover(t *testing.T) {
	ctx := &conversationAffinityRequestContext{Mode: dbmodel.GroupRouteAffinityModeStrict}
	if ctx.BlocksCrossRouteFailover() {
		t.Fatal("strict mode should not block before a preferred route is known")
	}
	ctx.PreferredRoute = &affinityRoute{ChannelID: 7}
	if !ctx.BlocksCrossRouteFailover() {
		t.Fatal("strict mode should block after a preferred route is known")
	}
}
