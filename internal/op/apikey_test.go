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

func setupAPIKeyTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "apikey.db"), false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	apiKeyCache.Clear()
	apiKeyIDMap.Clear()
	statsAPIKeyCache.Clear()
	statsAPIKeyCacheNeedUpdateLock.Lock()
	statsAPIKeyCacheNeedUpdate = make(map[int]struct{})
	statsAPIKeyCacheNeedUpdateLock.Unlock()
	t.Cleanup(func() {
		apiKeyCache.Clear()
		apiKeyIDMap.Clear()
		statsAPIKeyCache.Clear()
		_ = db.Close()
	})
	return context.Background()
}

func TestAPIKeyUpdateAllowsCustomValueAndPreservesItWhenOmitted(t *testing.T) {
	ctx := setupAPIKeyTestDB(t)
	key := &model.APIKey{Name: "custom-key", APIKey: "sk-octopus-original", Enabled: true}
	if err := APIKeyCreate(key, ctx); err != nil {
		t.Fatalf("APIKeyCreate failed: %v", err)
	}

	key.APIKey = "sk-octopus-custom"
	key.Name = "renamed-key"
	if err := APIKeyUpdate(key, ctx); err != nil {
		t.Fatalf("APIKeyUpdate(custom) failed: %v", err)
	}
	if _, err := APIKeyGetByAPIKey("sk-octopus-original", ctx); err == nil {
		t.Fatal("old API key still resolves after update")
	}
	got, err := APIKeyGetByAPIKey("sk-octopus-custom", ctx)
	if err != nil {
		t.Fatalf("new API key lookup failed: %v", err)
	}
	if got.Name != "renamed-key" {
		t.Fatalf("updated key name = %q, want renamed-key", got.Name)
	}

	key.APIKey = ""
	key.Name = "renamed-again"
	if err := APIKeyUpdate(key, ctx); err != nil {
		t.Fatalf("APIKeyUpdate(omitted value) failed: %v", err)
	}
	got, err = APIKeyGetByAPIKey("sk-octopus-custom", ctx)
	if err != nil {
		t.Fatalf("custom API key was not preserved: %v", err)
	}
	if got.Name != "renamed-again" {
		t.Fatalf("preserved key name = %q, want renamed-again", got.Name)
	}
}

func TestAPIKeyDeleteRemovesCustomValueFromLookupMap(t *testing.T) {
	ctx := setupAPIKeyTestDB(t)
	key := &model.APIKey{Name: "delete-key", APIKey: "sk-octopus-delete", Enabled: true}
	if err := APIKeyCreate(key, ctx); err != nil {
		t.Fatalf("APIKeyCreate failed: %v", err)
	}
	if err := APIKeyDelete(key.ID, ctx); err != nil {
		t.Fatalf("APIKeyDelete failed: %v", err)
	}
	if _, err := APIKeyGetByAPIKey(key.APIKey, ctx); err == nil {
		t.Fatal("deleted API key still resolves")
	}
	var deleted model.APIKey
	if err := db.GetDB().First(&deleted, key.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("deleted API key database error = %v, want record not found", err)
	}
}
