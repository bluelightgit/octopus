package task

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bluelightgit/octopus/internal/db"
	"github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/op"
	"github.com/bluelightgit/octopus/internal/transformer/outbound"
)

func TestNormalizeSyncedModels(t *testing.T) {
	got := normalizeSyncedModels([]string{" gpt-4o ", "", "gpt-4o", "custom", "claude-3"}, "custom,manual")
	want := []string{"gpt-4o", "claude-3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized synced models = %#v, want %#v", got, want)
	}
}

func TestSyncModelsTaskRejectsOverlappingRun(t *testing.T) {
	syncModelsMu.Lock()
	defer syncModelsMu.Unlock()

	err := SyncModelsTask()
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("SyncModelsTask() error = %v, want already-running error", err)
	}
}

func TestSyncModelsTaskUpdatesChannelAndRemovesStaleGroupItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]string{
				{"id": "new-model"},
				{"id": "custom-model"},
				{"id": " new-model "},
			},
		})
	}))
	defer server.Close()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "sync.db"), false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache failed: %v", err)
	}

	ctx := context.Background()
	channel := &model.Channel{
		Name:        "sync-channel",
		Type:        outbound.OutboundTypeOpenAIChat,
		Enabled:     true,
		AutoSync:    true,
		BaseUrls:    []model.BaseUrl{{URL: server.URL}},
		Model:       "old-model",
		CustomModel: "custom-model",
		Keys:        []model.ChannelKey{{Enabled: true, ChannelKey: "sync-key"}},
	}
	if err := op.ChannelCreate(channel, ctx); err != nil {
		t.Fatalf("ChannelCreate failed: %v", err)
	}
	group := &model.Group{Name: "sync-group", Mode: model.GroupModeFailover}
	if err := op.GroupCreate(group, ctx); err != nil {
		t.Fatalf("GroupCreate failed: %v", err)
	}
	for _, modelName := range []string{"old-model", "custom-model"} {
		if err := op.GroupItemAdd(&model.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: modelName, Priority: 1, Weight: 1}, ctx); err != nil {
			t.Fatalf("GroupItemAdd(%s) failed: %v", modelName, err)
		}
	}

	if err := SyncModelsTask(); err != nil {
		t.Fatalf("SyncModelsTask failed: %v", err)
	}
	updated, err := op.ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet failed: %v", err)
	}
	if updated.Model != "new-model" || updated.CustomModel != "custom-model" {
		t.Fatalf("synced model config = %q/%q", updated.Model, updated.CustomModel)
	}
	items, err := op.GroupItemList(group.ID, ctx)
	if err != nil {
		t.Fatalf("GroupItemList failed: %v", err)
	}
	if len(items) != 1 || items[0].ModelName != "custom-model" {
		t.Fatalf("group items after sync = %#v, want custom-model only", items)
	}
}
