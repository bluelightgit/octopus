package op

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bluelightgit/octopus/internal/db"
	"github.com/bluelightgit/octopus/internal/model"
	"gorm.io/gorm"
)

func setupLLMTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "llm.db"), false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	llmModelCache.Clear()
	channelCache.Clear()
	t.Cleanup(func() {
		llmModelCache.Clear()
		channelCache.Clear()
		_ = db.Close()
	})
	return context.Background()
}

func TestLLMBatchCreateNormalizesDeduplicatesAndLoadsDatabaseRow(t *testing.T) {
	ctx := setupLLMTestDB(t)

	if err := db.GetDB().Create(&model.LLMInfo{Name: "existing", LLMPrice: model.LLMPrice{Input: 7}}).Error; err != nil {
		t.Fatalf("seed existing model failed: %v", err)
	}
	if err := LLMBatchCreate([]model.LLMInfo{
		{Name: " Existing ", LLMPrice: model.LLMPrice{Input: 99}},
		{Name: " New-Model ", LLMPrice: model.LLMPrice{Input: 1, Output: 2}},
		{Name: "new-model", LLMPrice: model.LLMPrice{Input: 99}},
	}, ctx); err != nil {
		t.Fatalf("LLMBatchCreate failed: %v", err)
	}

	got, err := LLMGet(" EXISTING ")
	if err != nil {
		t.Fatalf("LLMGet(existing) failed: %v", err)
	}
	if got.Input != 7 {
		t.Fatalf("existing price = %+v, want database value", got)
	}
	got, err = LLMGet("new-model")
	if err != nil {
		t.Fatalf("LLMGet(new-model) failed: %v", err)
	}
	if got.Input != 1 || got.Output != 2 {
		t.Fatalf("new model price = %+v, want input=1 output=2", got)
	}

	var count int64
	if err := db.GetDB().Model(&model.LLMInfo{}).Where("name = ?", "new-model").Count(&count).Error; err != nil {
		t.Fatalf("count new model failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("new-model rows = %d, want 1", count)
	}
}

func TestLLMDeleteRejectsReferencedChannel(t *testing.T) {
	ctx := setupLLMTestDB(t)
	channel := &model.Channel{Name: "price-reference-channel", Model: "Referenced-Model"}
	if err := ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	if err := LLMCreate(model.LLMInfo{Name: "Referenced-Model", LLMPrice: model.LLMPrice{Input: 1}}, ctx); err != nil {
		t.Fatalf("LLMCreate failed: %v", err)
	}

	if err := LLMDelete(" REFERENCED-MODEL ", ctx); err == nil || err.Error() != "model is referenced by channel" {
		t.Fatalf("LLMDelete() error = %v, want referenced-model error", err)
	}
	if _, err := LLMGet("referenced-model"); err != nil {
		t.Fatalf("referenced price disappeared after rejected delete: %v", err)
	}
}

func TestLLMCleanupGhostsAndBatchSave(t *testing.T) {
	ctx := setupLLMTestDB(t)
	channel := &model.Channel{Name: "cleanup-reference-channel", Model: "kept-model"}
	if err := ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	if err := LLMCreate(model.LLMInfo{Name: "kept-model", LLMPrice: model.LLMPrice{Input: 1}}, ctx); err != nil {
		t.Fatalf("create kept model failed: %v", err)
	}
	if err := LLMCreate(model.LLMInfo{Name: "ghost-model", LLMPrice: model.LLMPrice{Input: 2}}, ctx); err != nil {
		t.Fatalf("create ghost model failed: %v", err)
	}

	if err := LLMCleanupGhosts(ctx); err != nil {
		t.Fatalf("LLMCleanupGhosts failed: %v", err)
	}
	if _, err := LLMGet("ghost-model"); err == nil {
		t.Fatal("ghost model remains in cache")
	}
	var ghost model.LLMInfo
	if err := db.GetDB().First(&ghost, "name = ?", "ghost-model").Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("ghost database row error = %v, want record not found", err)
	}

	if err := LLMBatchSave([]model.LLMInfo{{Name: " KEPT-MODEL ", LLMPrice: model.LLMPrice{Input: 9}}}, ctx); err != nil {
		t.Fatalf("LLMBatchSave failed: %v", err)
	}
	got, err := LLMGet("kept-model")
	if err != nil {
		t.Fatalf("LLMGet(kept-model) failed: %v", err)
	}
	if got.Input != 9 {
		t.Fatalf("kept model price = %+v, want input=9", got)
	}
}
