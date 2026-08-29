package op

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/bluelightgit/octopus/internal/db"
	"github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/utils/cache"
	"github.com/bluelightgit/octopus/internal/utils/log"
)

var channelCache = cache.New[int, model.Channel](16)
var channelKeyCache = cache.New[int, model.ChannelKey](16)
var channelKeyCacheNeedUpdate = make(map[int]struct{})
var channelKeyCacheNeedUpdateLock sync.Mutex

func normalizeChannelSystemPromptRoleOverride(channel *model.Channel) {
	if channel == nil {
		return
	}
	channel.SystemPromptRoleOverride = channel.SystemPromptRoleOverride.Normalize()
}

func ChannelList(ctx context.Context) ([]model.Channel, error) {
	channels := make([]model.Channel, 0, channelCache.Len())
	for _, channel := range channelCache.GetAll() {
		normalizeChannelSystemPromptRoleOverride(&channel)
		channels = append(channels, channel)
	}
	return channels, nil
}

func ChannelCreate(channel *model.Channel, ctx context.Context) error {
	if channel != nil {
		channel.Model, channel.CustomModel = model.NormalizeChannelModelConfig(channel.Model, channel.CustomModel)
		channel.SystemPromptRoleOverride = channel.SystemPromptRoleOverride.Normalize()
		channel.ResponsesWebsocketMaxLifetimeSec = model.NormalizeResponsesWebsocketMaxLifetimeSec(channel.ResponsesWebsocketMaxLifetimeSec)
	}
	if err := db.GetDB().WithContext(ctx).Create(channel).Error; err != nil {
		return err
	}
	channelCache.Set(channel.ID, *channel)
	for _, k := range channel.Keys {
		if k.ID != 0 {
			channelKeyCache.Set(k.ID, k)
		}
	}
	return nil
}

// ChannelKeyUpdate 原子更新 ChannelKey 的运行状态和费用增量，并标记为需要持久化。
func ChannelKeyUpdate(key model.ChannelKey, costDelta float64) error {
	if key.ID == 0 || key.ChannelID == 0 {
		return fmt.Errorf("invalid channel key")
	}
	channelKeyCacheNeedUpdateLock.Lock()
	defer channelKeyCacheNeedUpdateLock.Unlock()
	current, ok := channelKeyCache.Get(key.ID)
	if !ok {
		return fmt.Errorf("channel key not found")
	}
	current.StatusCode = key.StatusCode
	current.LastUseTimeStamp = key.LastUseTimeStamp
	current.TotalCost += costDelta
	channelKeyCache.Set(key.ID, current)
	channelKeyCacheNeedUpdate[key.ID] = struct{}{}
	return nil
}
func ChannelBaseUrlUpdate(channelID int, baseUrl []model.BaseUrl) error {
	ch, ok := channelCache.Get(channelID)
	if !ok {
		return fmt.Errorf("channel not found")
	}
	// Copy to decouple callers from internal cache storage.
	if baseUrl == nil {
		ch.BaseUrls = nil
	} else {
		cp := make([]model.BaseUrl, len(baseUrl))
		copy(cp, baseUrl)
		ch.BaseUrls = cp
	}
	channelCache.Set(channelID, ch)
	return nil
}

// ChannelKeySaveDB 将运行时更新过的 ChannelKey 缓存写入数据库。
func ChannelKeySaveDB(ctx context.Context) error {
	channelKeyCacheNeedUpdateLock.Lock()
	keyIDs := make([]int, 0, len(channelKeyCacheNeedUpdate))
	for id := range channelKeyCacheNeedUpdate {
		keyIDs = append(keyIDs, id)
	}
	channelKeyCacheNeedUpdate = make(map[int]struct{})
	channelKeyCacheNeedUpdateLock.Unlock()

	if len(keyIDs) == 0 {
		return nil
	}

	dbConn := db.GetDB().WithContext(ctx)
	for _, id := range keyIDs {
		k, ok := channelKeyCache.Get(id)
		if !ok {
			continue
		}
		if err := dbConn.Save(&k).Error; err != nil {
			channelKeyCacheNeedUpdateLock.Lock()
			for _, keyID := range keyIDs {
				channelKeyCacheNeedUpdate[keyID] = struct{}{}
			}
			channelKeyCacheNeedUpdateLock.Unlock()
			return err
		}
	}
	return nil
}

func ChannelUpdate(req *model.ChannelUpdateRequest, ctx context.Context) (*model.Channel, error) {
	oldChannel, ok := channelCache.Get(req.ID)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}

	var removedModels []model.GroupIDAndLLMName
	var nextModel, nextCustomModel string
	if req.Model != nil || req.CustomModel != nil {
		nextModel = oldChannel.Model
		nextCustomModel = oldChannel.CustomModel
		if req.Model != nil {
			nextModel = *req.Model
		}
		if req.CustomModel != nil {
			nextCustomModel = *req.CustomModel
		}
		nextModel, nextCustomModel = model.NormalizeChannelModelConfig(nextModel, nextCustomModel)
		oldModels := model.ChannelModelNames(oldChannel.Model, oldChannel.CustomModel)
		newModelSet := make(map[string]struct{})
		for _, modelName := range model.ChannelModelNames(nextModel, nextCustomModel) {
			newModelSet[modelName] = struct{}{}
		}
		for _, modelName := range oldModels {
			if _, ok := newModelSet[modelName]; !ok {
				removedModels = append(removedModels, model.GroupIDAndLLMName{ChannelID: req.ID, ModelName: modelName})
			}
		}
	}

	tx := db.GetDB().WithContext(ctx).Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var selectFields []string
	updates := model.Channel{ID: req.ID}

	if req.Name != nil {
		selectFields = append(selectFields, "name")
		updates.Name = *req.Name
	}
	if req.Type != nil {
		selectFields = append(selectFields, "type")
		updates.Type = *req.Type
	}
	if req.Enabled != nil {
		selectFields = append(selectFields, "enabled")
		updates.Enabled = *req.Enabled
	}
	if req.BaseUrls != nil {
		selectFields = append(selectFields, "base_urls")
		updates.BaseUrls = *req.BaseUrls
	}
	if req.Model != nil || req.CustomModel != nil {
		selectFields = append(selectFields, "model", "custom_model")
		updates.Model = nextModel
		updates.CustomModel = nextCustomModel
	}
	if req.Proxy != nil {
		selectFields = append(selectFields, "proxy")
		updates.Proxy = *req.Proxy
	}
	if req.AutoSync != nil {
		selectFields = append(selectFields, "auto_sync")
		updates.AutoSync = *req.AutoSync
	}
	if req.AutoGroup != nil {
		selectFields = append(selectFields, "auto_group")
		updates.AutoGroup = *req.AutoGroup
	}
	if req.CustomHeader != nil {
		selectFields = append(selectFields, "custom_header")
		updates.CustomHeader = *req.CustomHeader
	}
	if req.ChannelProxy != nil {
		selectFields = append(selectFields, "channel_proxy")
		updates.ChannelProxy = req.ChannelProxy
	}
	if req.ParamOverride != nil {
		selectFields = append(selectFields, "param_override")
		updates.ParamOverride = req.ParamOverride
	}
	if req.SystemPromptRoleOverride != nil {
		selectFields = append(selectFields, "system_prompt_role_override")
		updates.SystemPromptRoleOverride = req.SystemPromptRoleOverride.Normalize()
	}
	if req.ResponsesWebsocketMaxLifetimeSec != nil {
		selectFields = append(selectFields, "responses_websocket_max_lifetime_sec")
		updates.ResponsesWebsocketMaxLifetimeSec = model.NormalizeResponsesWebsocketMaxLifetimeSec(*req.ResponsesWebsocketMaxLifetimeSec)
	}
	if req.MatchRegex != nil {
		selectFields = append(selectFields, "match_regex")
		updates.MatchRegex = req.MatchRegex
	}

	// 只有当有字段需要更新时才执行 UPDATE
	if len(selectFields) > 0 {
		if err := tx.Model(&model.Channel{}).Where("id = ?", req.ID).Select(selectFields).Updates(&updates).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to update channel: %w", err)
		}
	}

	// 删除 keys
	if len(req.KeysToDelete) > 0 {
		if err := tx.Where("id IN ? AND channel_id = ?", req.KeysToDelete, req.ID).Delete(&model.ChannelKey{}).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to delete channel keys: %w", err)
		}
	}

	// 更新 keys（逐条，只更新提供的字段）
	if len(req.KeysToUpdate) > 0 {
		for _, ku := range req.KeysToUpdate {
			updates := map[string]interface{}{}
			if ku.Enabled != nil {
				updates["enabled"] = *ku.Enabled
			}
			if ku.ChannelKey != nil {
				updates["channel_key"] = *ku.ChannelKey
			}
			if ku.Remark != nil {
				updates["remark"] = *ku.Remark
			}
			if len(updates) == 0 {
				continue
			}
			if err := tx.Model(&model.ChannelKey{}).
				Where("id = ? AND channel_id = ?", ku.ID, req.ID).
				Updates(updates).Error; err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("failed to update channel key %d: %w", ku.ID, err)
			}
		}
	}

	// 新增 keys
	if len(req.KeysToAdd) > 0 {
		newKeys := make([]model.ChannelKey, 0, len(req.KeysToAdd))
		for _, ka := range req.KeysToAdd {
			newKeys = append(newKeys, model.ChannelKey{
				ChannelID:  req.ID,
				Enabled:    ka.Enabled,
				ChannelKey: ka.ChannelKey,
				Remark:     ka.Remark,
			})
		}
		if err := tx.Create(&newKeys).Error; err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to create channel keys: %w", err)
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}
	if err := GroupItemBatchDelByChannelAndModels(removedModels, ctx); err != nil {
		return nil, fmt.Errorf("failed to remove stale group items: %w", err)
	}

	// 刷新缓存并返回最新数据
	if err := channelRefreshCacheByID(req.ID, ctx); err != nil {
		return nil, err
	}

	channel, _ := channelCache.Get(req.ID)
	return &channel, nil
}

func ChannelEnabled(id int, enabled bool, ctx context.Context) error {
	oldChannel, ok := channelCache.Get(id)
	if !ok {
		return fmt.Errorf("channel not found")
	}
	if err := db.GetDB().WithContext(ctx).Model(&model.Channel{}).Where("id = ?", id).Update("enabled", enabled).Error; err != nil {
		return err
	}
	oldChannel.Enabled = enabled
	channelCache.Set(id, oldChannel)
	return nil
}

func ChannelDel(id int, ctx context.Context) error {
	ch, ok := channelCache.Get(id)
	if !ok {
		return fmt.Errorf("channel not found")
	}

	// 开启事务
	tx := db.GetDB().WithContext(ctx).Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 获取所有受影响的 GroupID，用于刷新缓存
	var affectedGroupIDs []int
	if err := tx.Model(&model.GroupItem{}).
		Where("channel_id = ?", id).
		Pluck("group_id", &affectedGroupIDs).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to get affected groups: %w", err)
	}

	// 删除所有引用该渠道的 GroupItem
	if err := tx.Where("channel_id = ?", id).Delete(&model.GroupItem{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete group items: %w", err)
	}

	// 删除渠道 keys
	if err := tx.Where("channel_id = ?", id).Delete(&model.ChannelKey{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel keys: %w", err)
	}

	// 删除统计数据
	if err := tx.Where("channel_id = ?", id).Delete(&model.StatsChannel{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel stats: %w", err)
	}

	// 删除渠道
	if err := tx.Delete(&model.Channel{}, id).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("failed to delete channel: %w", err)
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// 删除缓存
	channelCache.Del(id)
	channelKeyCacheNeedUpdateLock.Lock()
	for _, k := range ch.Keys {
		if k.ID != 0 {
			channelKeyCache.Del(k.ID)
			delete(channelKeyCacheNeedUpdate, k.ID)
		}
	}
	channelKeyCacheNeedUpdateLock.Unlock()
	StatsChannelDel(id)

	// 刷新受影响的分组缓存
	for _, groupID := range affectedGroupIDs {
		if err := groupRefreshCacheByID(groupID, ctx); err != nil {
			log.Warnf("failed to refresh group cache for group %d: %v", groupID, err)
		}
	}

	return nil
}

func ChannelLLMList(ctx context.Context) ([]model.LLMChannel, error) {
	models := []model.LLMChannel{}
	for _, channel := range channelCache.GetAll() {
		modelNames := model.ChannelModelNames(channel.Model, channel.CustomModel)
		for _, modelName := range modelNames {
			if modelName == "" {
				continue
			}
			models = append(models, model.LLMChannel{
				Name:        modelName,
				Enabled:     channel.Enabled,
				ChannelID:   channel.ID,
				ChannelName: channel.Name,
			})
		}
	}
	return models, nil
}

func ChannelGet(id int, ctx context.Context) (*model.Channel, error) {
	channel, ok := channelCache.Get(id)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}
	normalizeChannelSystemPromptRoleOverride(&channel)
	channel.Keys = slices.Clone(channel.Keys)
	for i, key := range channel.Keys {
		if current, ok := channelKeyCache.Get(key.ID); ok {
			channel.Keys[i] = current
		}
	}
	return &channel, nil
}

func channelRefreshCache(ctx context.Context) error {
	channels := []model.Channel{}
	if err := db.GetReadDB().WithContext(ctx).
		Preload("Keys").
		Preload("Stats").
		Find(&channels).Error; err != nil {
		log.Warnf("failed to get channels: %v", err)
		return err
	}
	channelKeyCache.Clear()
	channelKeyCacheNeedUpdateLock.Lock()
	channelKeyCacheNeedUpdate = make(map[int]struct{})
	channelKeyCacheNeedUpdateLock.Unlock()
	for _, channel := range channels {
		normalizeChannelSystemPromptRoleOverride(&channel)
		channelCache.Set(channel.ID, channel)
		for _, k := range channel.Keys {
			if k.ID != 0 {
				channelKeyCache.Set(k.ID, k)
			}
		}
	}
	return nil
}

func channelRefreshCacheByID(id int, ctx context.Context) error {
	var channel model.Channel
	if err := db.GetReadDB().WithContext(ctx).
		Preload("Keys").
		Preload("Stats").
		First(&channel, id).Error; err != nil {
		return err
	}
	normalizeChannelSystemPromptRoleOverride(&channel)
	channelKeyCacheNeedUpdateLock.Lock()
	if old, ok := channelCache.Get(id); ok {
		for _, key := range old.Keys {
			if !slices.ContainsFunc(channel.Keys, func(current model.ChannelKey) bool {
				return current.ID == key.ID
			}) {
				channelKeyCache.Del(key.ID)
				delete(channelKeyCacheNeedUpdate, key.ID)
			}
		}
	}
	for i, key := range channel.Keys {
		if current, ok := channelKeyCache.Get(key.ID); ok {
			key.StatusCode = current.StatusCode
			key.LastUseTimeStamp = current.LastUseTimeStamp
			key.TotalCost = current.TotalCost
			channel.Keys[i] = key
		}
		channelKeyCache.Set(key.ID, channel.Keys[i])
	}
	channelKeyCacheNeedUpdateLock.Unlock()
	channelCache.Set(channel.ID, channel)
	return nil
}
