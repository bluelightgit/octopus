package handlers

import (
	"testing"

	transformerModel "github.com/bluelightgit/octopus/internal/transformer/model"
)

func TestNormalizeSystemPromptRoleOverrideEmptyDefaultsToAuto(t *testing.T) {
	value := transformerModel.SystemPromptRoleOverride("")
	if err := normalizeSystemPromptRoleOverride(&value); err != nil {
		t.Fatalf("normalize empty role override failed: %v", err)
	}
	if value != transformerModel.SystemPromptRoleOverrideAuto {
		t.Fatalf("normalized role override = %q, want auto", value)
	}
}

func TestNormalizeSystemPromptRoleOverrideRejectsUnknownValue(t *testing.T) {
	value := transformerModel.SystemPromptRoleOverride("unsupported")
	if err := normalizeSystemPromptRoleOverride(&value); err == nil {
		t.Fatal("expected unknown role override to be rejected")
	}
	if value != "unsupported" {
		t.Fatalf("invalid role override was changed to %q", value)
	}
}
