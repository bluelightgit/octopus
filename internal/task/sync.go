package task

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bluelightgit/octopus/internal/helper"
	"github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/op"
	"github.com/bluelightgit/octopus/internal/utils/diff"
	"github.com/bluelightgit/octopus/internal/utils/log"
)

var (
	syncModelsMu         sync.Mutex
	lastSyncModelsTimeMu sync.RWMutex
	lastSyncModelsTime   = time.Now()
)

// SyncModelsTask synchronizes auto-managed channel models. A second trigger
// while one sync is running returns an error instead of issuing overlapping
// upstream requests and racing channel/group updates.
func SyncModelsTask() error {
	if !syncModelsMu.TryLock() {
		return fmt.Errorf("model sync already running")
	}
	defer syncModelsMu.Unlock()

	log.Debugf("sync models task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("sync models task finished, sync time: %s", time.Since(startTime))
	}()
	defer func() {
		lastSyncModelsTimeMu.Lock()
		lastSyncModelsTime = time.Now()
		lastSyncModelsTimeMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	channels, err := op.ChannelList(ctx)
	if err != nil {
		log.Errorf("failed to list channels: %v", err)
		return err
	}
	var syncErr error
	for _, channel := range channels {
		if !channel.AutoSync {
			continue
		}
		fetchModels, err := helper.FetchModels(ctx, channel)
		if err != nil {
			log.Warnf("failed to sync models for channel %s: %v", channel.Name, err)
			if syncErr == nil {
				syncErr = fmt.Errorf("failed to fetch models for channel %s: %w", channel.Name, err)
			}
			continue
		}
		newModels := normalizeSyncedModels(fetchModels, channel.CustomModel)
		oldModels := strings.Split(model.NormalizeChannelModelList(channel.Model), ",")
		if channel.Model == "" {
			oldModels = nil
		}
		deletedModels, addedModels := diff.Diff(oldModels, newModels)
		if len(deletedModels) > 0 || len(addedModels) > 0 {
			fetchModelStr := strings.Join(newModels, ",")
			updatedChannel, updateErr := op.ChannelUpdate(&model.ChannelUpdateRequest{
				ID:    channel.ID,
				Model: &fetchModelStr,
			}, ctx)
			if updateErr != nil {
				log.Warnf("failed to update channel %s: %v", channel.Name, updateErr)
				if syncErr == nil {
					syncErr = fmt.Errorf("failed to update channel %s models: %w", channel.Name, updateErr)
				}
				continue
			}
			channel = *updatedChannel
		}
		if len(addedModels) > 0 {
			if err := helper.LLMPriceAddToDB(addedModels, ctx); err != nil {
				log.Warnf("failed to save model prices for channel %s: %v", channel.Name, err)
				if syncErr == nil {
					syncErr = fmt.Errorf("failed to save model prices for channel %s: %w", channel.Name, err)
				}
			}
		}
		if len(deletedModels) > 0 {
			log.Infof("deleted channel %s models: %v", channel.Name, deletedModels)
		}
		if len(newModels) > 0 {
			helper.ChannelAutoGroup(&channel, ctx)
		}
	}
	if err := op.LLMCleanupGhosts(ctx); err != nil {
		log.Warnf("failed to clean ghost model prices: %v", err)
		if syncErr == nil {
			syncErr = fmt.Errorf("failed to clean ghost model prices: %w", err)
		}
	}
	return syncErr
}

func normalizeSyncedModels(fetchModels []string, customModelList string) []string {
	customModels := strings.Split(model.NormalizeChannelModelList(customModelList), ",")
	customModelSet := make(map[string]struct{}, len(customModels))
	for _, modelName := range customModels {
		if modelName != "" {
			customModelSet[modelName] = struct{}{}
		}
	}
	newModels := make([]string, 0, len(fetchModels))
	seenModels := make(map[string]struct{}, len(fetchModels))
	for _, modelName := range fetchModels {
		modelName = strings.TrimSpace(modelName)
		if modelName == "" {
			continue
		}
		if _, isCustom := customModelSet[modelName]; isCustom {
			continue
		}
		if _, seen := seenModels[modelName]; seen {
			continue
		}
		seenModels[modelName] = struct{}{}
		newModels = append(newModels, modelName)
	}
	return newModels
}

func GetLastSyncModelsTime() time.Time {
	lastSyncModelsTimeMu.RLock()
	defer lastSyncModelsTimeMu.RUnlock()
	return lastSyncModelsTime
}
