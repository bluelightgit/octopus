package op

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bluelightgit/octopus/internal/db"
	"github.com/bluelightgit/octopus/internal/model"
	transformerModel "github.com/bluelightgit/octopus/internal/transformer/model"
)

func setupChannelModelTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "channel-model.db"), false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	channelCache.Clear()
	channelKeyCache.Clear()
	groupCache.Clear()
	groupMap.Clear()
	t.Cleanup(func() {
		channelCache.Clear()
		channelKeyCache.Clear()
		groupCache.Clear()
		groupMap.Clear()
		_ = db.Close()
	})
	return context.Background()
}

func TestChannelModelConfigNormalizesAndRemovesStaleGroupItems(t *testing.T) {
	ctx := setupChannelModelTestDB(t)
	channel := &model.Channel{
		Name:        "model-normalization-channel",
		Model:       "auto-a, auto-b, auto-a",
		CustomModel: "auto-b, custom-c",
	}
	if err := ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	if channel.Model != "auto-a" || channel.CustomModel != "auto-b,custom-c" {
		t.Fatalf("stored model config = %q/%q", channel.Model, channel.CustomModel)
	}

	group := &model.Group{Name: "model-normalization-group", Mode: model.GroupModeFailover}
	if err := GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	for _, name := range []string{"auto-a", "auto-b", "custom-c"} {
		if err := GroupItemAdd(&model.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: name, Priority: 1, Weight: 1}, ctx); err != nil {
			t.Fatalf("GroupItemAdd(%s) failed: %v", name, err)
		}
	}

	nextAutoModels := " auto-a, auto-d, auto-d "
	nextCustomModels := "custom-c"
	updated, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:          channel.ID,
		Model:       &nextAutoModels,
		CustomModel: &nextCustomModels,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate failed: %v", err)
	}
	if updated.Model != "auto-a,auto-d" || updated.CustomModel != "custom-c" {
		t.Fatalf("updated model config = %q/%q", updated.Model, updated.CustomModel)
	}

	items, err := GroupItemList(group.ID, ctx)
	if err != nil {
		t.Fatalf("GroupItemList failed: %v", err)
	}
	gotModels := make(map[string]bool, len(items))
	for _, item := range items {
		gotModels[item.ModelName] = true
	}
	if !gotModels["auto-a"] || !gotModels["custom-c"] || gotModels["auto-b"] {
		t.Fatalf("group items after model update = %#v, want auto-a and custom-c", gotModels)
	}
}

func TestChannelSystemPromptRoleOverrideEmptyUpdateDefaultsToAuto(t *testing.T) {
	ctx := setupChannelModelTestDB(t)
	channel := &model.Channel{Name: "empty-role-override-channel"}
	if err := ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	empty := transformerModel.SystemPromptRoleOverride("")
	updated, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:                       channel.ID,
		SystemPromptRoleOverride: &empty,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate failed: %v", err)
	}
	if updated.SystemPromptRoleOverride != transformerModel.SystemPromptRoleOverrideAuto {
		t.Fatalf("updated role override = %q, want auto", updated.SystemPromptRoleOverride)
	}

	var stored model.Channel
	if err := db.GetDB().First(&stored, channel.ID).Error; err != nil {
		t.Fatalf("load updated channel failed: %v", err)
	}
	if stored.SystemPromptRoleOverride != transformerModel.SystemPromptRoleOverrideAuto {
		t.Fatalf("stored role override = %q, want auto", stored.SystemPromptRoleOverride)
	}
}

func TestChannelReadsNormalizeLegacyEmptySystemPromptRoleOverride(t *testing.T) {
	ctx := setupChannelModelTestDB(t)
	channel := &model.Channel{Name: "legacy-empty-role-channel"}
	if err := ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}

	legacy := *channel
	legacy.SystemPromptRoleOverride = ""
	channelCache.Set(channel.ID, legacy)

	got, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet failed: %v", err)
	}
	if got.SystemPromptRoleOverride != transformerModel.SystemPromptRoleOverrideAuto {
		t.Fatalf("ChannelGet role override = %q, want auto", got.SystemPromptRoleOverride)
	}

	channels, err := ChannelList(ctx)
	if err != nil {
		t.Fatalf("ChannelList failed: %v", err)
	}
	if len(channels) != 1 || channels[0].SystemPromptRoleOverride != transformerModel.SystemPromptRoleOverrideAuto {
		t.Fatalf("ChannelList role override = %#v, want one auto channel", channels)
	}
}
