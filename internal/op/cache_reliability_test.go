package op

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bluelightgit/octopus/internal/db"
	"github.com/bluelightgit/octopus/internal/model"
)

func clearChannelKeyTestState(keyID, channelID int) {
	channelKeyCache.Del(keyID)
	channelCache.Del(channelID)
	channelKeyCacheNeedUpdateLock.Lock()
	delete(channelKeyCacheNeedUpdate, keyID)
	channelKeyCacheNeedUpdateLock.Unlock()
}

func clearStatsTestState(channelID, modelID, apiKeyID int) {
	statsChannelCache.Del(channelID)
	statsModelCache.Del(modelID)
	statsAPIKeyCache.Del(apiKeyID)

	statsChannelCacheNeedUpdateLock.Lock()
	delete(statsChannelCacheNeedUpdate, channelID)
	statsChannelCacheNeedUpdateLock.Unlock()
	statsModelCacheNeedUpdateLock.Lock()
	delete(statsModelCacheNeedUpdate, modelID)
	statsModelCacheNeedUpdateLock.Unlock()
	statsAPIKeyCacheNeedUpdateLock.Lock()
	delete(statsAPIKeyCacheNeedUpdate, apiKeyID)
	statsAPIKeyCacheNeedUpdateLock.Unlock()
}

func TestChannelKeyUpdateAccumulatesConcurrentCost(t *testing.T) {
	const (
		channelID = 910001
		keyID     = 910002
	)
	clearChannelKeyTestState(keyID, channelID)
	t.Cleanup(func() { clearChannelKeyTestState(keyID, channelID) })

	channelKeyCache.Set(keyID, model.ChannelKey{
		ID:         keyID,
		ChannelID:  channelID,
		Enabled:    true,
		ChannelKey: "test-key",
	})

	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ChannelKeyUpdate(model.ChannelKey{
				ID:               keyID,
				ChannelID:        channelID,
				StatusCode:       200,
				LastUseTimeStamp: 123,
			}, 1.25); err != nil {
				t.Errorf("ChannelKeyUpdate failed: %v", err)
			}
		}()
	}
	wg.Wait()

	updated, ok := channelKeyCache.Get(keyID)
	if !ok {
		t.Fatal("updated channel key missing")
	}
	if updated.TotalCost != workers*1.25 {
		t.Fatalf("total cost = %v, want %v", updated.TotalCost, workers*1.25)
	}
	if updated.StatusCode != 200 || updated.LastUseTimeStamp != 123 {
		t.Fatalf("runtime status not updated: %#v", updated)
	}
	if updated.ChannelKey != "test-key" || !updated.Enabled {
		t.Fatalf("persistent key fields were overwritten: %#v", updated)
	}
}

func TestStatsUpdatesAccumulateConcurrently(t *testing.T) {
	const (
		channelID = 920001
		modelID   = 920002
		apiKeyID  = 920003
	)
	clearStatsTestState(channelID, modelID, apiKeyID)
	t.Cleanup(func() { clearStatsTestState(channelID, modelID, apiKeyID) })

	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			metrics := model.StatsMetrics{RequestSuccess: 1, InputToken: 2, OutputCost: 0.5}
			if err := StatsChannelUpdate(channelID, metrics); err != nil {
				t.Errorf("StatsChannelUpdate failed: %v", err)
			}
			if err := StatsModelUpdate(model.StatsModel{ID: modelID, StatsMetrics: metrics}); err != nil {
				t.Errorf("StatsModelUpdate failed: %v", err)
			}
			if err := StatsAPIKeyUpdate(apiKeyID, metrics); err != nil {
				t.Errorf("StatsAPIKeyUpdate failed: %v", err)
			}
		}()
	}
	wg.Wait()

	channelStats := StatsChannelGet(channelID)
	if channelStats.RequestSuccess != workers || channelStats.InputToken != workers*2 || channelStats.OutputCost != workers*0.5 {
		t.Fatalf("channel stats = %#v, want success=%d input=%d output_cost=%v", channelStats, workers, workers*2, workers*0.5)
	}
	modelStats := statsModelCacheValue(modelID)
	if modelStats.RequestSuccess != workers || modelStats.InputToken != workers*2 || modelStats.OutputCost != workers*0.5 {
		t.Fatalf("model stats = %#v, want success=%d input=%d output_cost=%v", modelStats, workers, workers*2, workers*0.5)
	}
	apiKeyStats := StatsAPIKeyGet(apiKeyID)
	if apiKeyStats.RequestSuccess != workers || apiKeyStats.InputToken != workers*2 || apiKeyStats.OutputCost != workers*0.5 {
		t.Fatalf("api key stats = %#v, want success=%d input=%d output_cost=%v", apiKeyStats, workers, workers*2, workers*0.5)
	}
}

func statsModelCacheValue(id int) model.StatsModel {
	value, ok := statsModelCache.Get(id)
	if !ok {
		return model.StatsModel{}
	}
	return value
}

func TestStatsSaveDBRestoresDirtyIDsAfterPersistenceFailure(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "stats-dirty-retry.db")
	if err := db.InitDB("sqlite", databasePath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		channelID = 930001
		modelID   = 930002
		apiKeyID  = 930003
	)
	clearStatsTestState(channelID, modelID, apiKeyID)
	t.Cleanup(func() { clearStatsTestState(channelID, modelID, apiKeyID) })

	metrics := model.StatsMetrics{RequestSuccess: 1}
	if err := StatsChannelUpdate(channelID, metrics); err != nil {
		t.Fatalf("StatsChannelUpdate failed: %v", err)
	}
	if err := StatsModelUpdate(model.StatsModel{ID: modelID, StatsMetrics: metrics}); err != nil {
		t.Fatalf("StatsModelUpdate failed: %v", err)
	}
	if err := StatsAPIKeyUpdate(apiKeyID, metrics); err != nil {
		t.Fatalf("StatsAPIKeyUpdate failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := StatsSaveDB(ctx); err == nil {
		t.Fatal("expected canceled stats save to fail")
	}

	statsChannelCacheNeedUpdateLock.Lock()
	_, channelDirty := statsChannelCacheNeedUpdate[channelID]
	statsChannelCacheNeedUpdateLock.Unlock()
	statsModelCacheNeedUpdateLock.Lock()
	_, modelDirty := statsModelCacheNeedUpdate[modelID]
	statsModelCacheNeedUpdateLock.Unlock()
	statsAPIKeyCacheNeedUpdateLock.Lock()
	_, apiKeyDirty := statsAPIKeyCacheNeedUpdate[apiKeyID]
	statsAPIKeyCacheNeedUpdateLock.Unlock()

	if !channelDirty || !modelDirty || !apiKeyDirty {
		t.Fatalf("dirty IDs were not restored: channel=%t model=%t api_key=%t", channelDirty, modelDirty, apiKeyDirty)
	}
}
