package handlers

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

func TestNormalizeResponsesWebsocketEnabled_ClearsNonResponsesGroup(t *testing.T) {
	group := &model.Group{
		PreferredProtocolFamily:   model.GroupProtocolFamilyOpenAIChat,
		ResponsesWebsocketEnabled: true,
	}

	normalizeResponsesWebsocketEnabled(group)

	if group.ResponsesWebsocketEnabled {
		t.Fatal("expected websocket toggle to be cleared for non-responses group")
	}
}

func TestNormalizeResponsesWebsocketEnabled_PreservesResponsesGroup(t *testing.T) {
	group := &model.Group{
		PreferredProtocolFamily:   model.GroupProtocolFamilyOpenAIResponses,
		ResponsesWebsocketEnabled: true,
	}

	normalizeResponsesWebsocketEnabled(group)

	if !group.ResponsesWebsocketEnabled {
		t.Fatal("expected websocket toggle to be preserved for responses group")
	}
}

func TestNormalizeResponsesWebsocketEnabledUpdate_ClearsWhenSwitchingProtocol(t *testing.T) {
	enabled := true
	req := &model.GroupUpdateRequest{
		PreferredProtocolFamily:   ptrProtocolFamily(model.GroupProtocolFamilyOpenAIChat),
		ResponsesWebsocketEnabled: &enabled,
	}
	current := model.Group{
		PreferredProtocolFamily:   model.GroupProtocolFamilyOpenAIResponses,
		ResponsesWebsocketEnabled: true,
	}

	normalizeResponsesWebsocketEnabledUpdate(req, current)

	if req.ResponsesWebsocketEnabled == nil || *req.ResponsesWebsocketEnabled {
		t.Fatal("expected websocket toggle update to be forced off for non-responses protocol")
	}
}

func TestNormalizeResponsesWebsocketEnabledUpdate_PreservesForResponsesGroup(t *testing.T) {
	enabled := true
	req := &model.GroupUpdateRequest{ResponsesWebsocketEnabled: &enabled}
	current := model.Group{PreferredProtocolFamily: model.GroupProtocolFamilyOpenAIResponses}

	normalizeResponsesWebsocketEnabledUpdate(req, current)

	if req.ResponsesWebsocketEnabled == nil || !*req.ResponsesWebsocketEnabled {
		t.Fatal("expected websocket toggle update to remain enabled for responses group")
	}
}

func ptrProtocolFamily(v model.GroupProtocolFamily) *model.GroupProtocolFamily {
	return &v
}
