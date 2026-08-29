package db

import (
	"path/filepath"
	"testing"

	"github.com/bluelightgit/octopus/internal/model"
)

func TestChannelSystemPromptRoleOverrideDefaultsToAuto(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channel-role-default.db")
	if err := InitDB("sqlite", path, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = Close() })

	channel := &model.Channel{Name: "default-role-channel"}
	if err := GetDB().Create(channel).Error; err != nil {
		t.Fatalf("create channel failed: %v", err)
	}

	var stored model.Channel
	if err := GetDB().First(&stored, channel.ID).Error; err != nil {
		t.Fatalf("load channel failed: %v", err)
	}
	if stored.SystemPromptRoleOverride != "auto" {
		t.Fatalf("system prompt role override = %q, want auto", stored.SystemPromptRoleOverride)
	}
}
