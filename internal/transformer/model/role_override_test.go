package model

import "testing"

func TestSystemPromptRoleOverrideNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   SystemPromptRoleOverride
		want SystemPromptRoleOverride
	}{
		{name: "empty defaults to auto", in: "", want: SystemPromptRoleOverrideAuto},
		{name: "auto", in: SystemPromptRoleOverrideAuto, want: SystemPromptRoleOverrideAuto},
		{name: "system", in: SystemPromptRoleOverrideSystem, want: SystemPromptRoleOverrideSystem},
		{name: "developer", in: SystemPromptRoleOverrideDeveloper, want: SystemPromptRoleOverrideDeveloper},
		{name: "invalid defaults to auto", in: "invalid", want: SystemPromptRoleOverrideAuto},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Normalize(); got != tt.want {
				t.Fatalf("Normalize() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCloneWithSystemPromptRoleOverrideDoesNotMutateOriginal(t *testing.T) {
	systemText := "system prompt"
	developerText := "developer prompt"
	userText := "user prompt"
	original := &InternalLLMRequest{
		Messages: []Message{
			{Role: "system", Content: MessageContent{Content: &systemText}},
			{Role: "developer", Content: MessageContent{Content: &developerText}},
			{Role: "user", Content: MessageContent{Content: &userText}},
		},
		RawRequest: []byte(`{"messages":[]}`),
	}

	clone := original.CloneWithSystemPromptRoleOverride(SystemPromptRoleOverrideSystem)
	if clone == nil {
		t.Fatal("expected clone")
	}
	if got := []string{clone.Messages[0].Role, clone.Messages[1].Role, clone.Messages[2].Role}; got[0] != "system" || got[1] != "system" || got[2] != "user" {
		t.Fatalf("unexpected clone roles: %#v", got)
	}
	if original.Messages[0].Role != "system" || original.Messages[1].Role != "developer" || original.Messages[2].Role != "user" {
		t.Fatalf("original roles were mutated: %#v", original.Messages)
	}
	if clone.TransformOptions.SystemPromptRoleOverride != SystemPromptRoleOverrideSystem {
		t.Fatalf("clone override = %q", clone.TransformOptions.SystemPromptRoleOverride)
	}
	if string(clone.RawRequest) != string(original.RawRequest) {
		t.Fatal("raw request changed in outbound copy")
	}
}

func TestCloneWithSystemPromptRoleOverrideAutoPreservesRoles(t *testing.T) {
	request := &InternalLLMRequest{
		Messages: []Message{{Role: "developer"}},
	}

	clone := request.CloneWithSystemPromptRoleOverride(SystemPromptRoleOverrideAuto)
	if clone == request {
		t.Fatal("expected an outbound copy")
	}
	if clone.Messages[0].Role != "developer" {
		t.Fatalf("role = %q, want developer", clone.Messages[0].Role)
	}
	if request.Messages[0].Role != "developer" {
		t.Fatalf("original role = %q, want developer", request.Messages[0].Role)
	}
}
