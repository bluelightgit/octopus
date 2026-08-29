package op

import (
	"context"
	"fmt"
	"strings"

	"github.com/bluelightgit/octopus/internal/db"
	"github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/utils/cache"
	"gorm.io/gorm/clause"
)

var llmModelCache = cache.New[string, model.LLMPrice](16)

func LLMList(ctx context.Context) ([]model.LLMInfo, error) {
	models := make([]model.LLMInfo, 0, llmModelCache.Len())
	for m, cost := range llmModelCache.GetAll() {
		models = append(models, model.LLMInfo{
			Name:     m,
			LLMPrice: cost,
		})
	}
	return models, nil
}

func LLMUpdate(model model.LLMInfo, ctx context.Context) error {
	model.Name = strings.ToLower(strings.TrimSpace(model.Name))
	if model.Name == "" {
		return fmt.Errorf("model name is empty")
	}
	_, ok := llmModelCache.Get(model.Name)
	if !ok {
		return fmt.Errorf("model not found")
	}
	if err := db.GetDB().WithContext(ctx).Save(model).Error; err != nil {
		return err
	}
	llmModelCache.Set(model.Name, model.LLMPrice)
	return nil
}

func LLMDelete(modelName string, ctx context.Context) error {
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	if modelName == "" {
		return fmt.Errorf("model name is empty")
	}
	_, ok := llmModelCache.Get(modelName)
	if !ok {
		return fmt.Errorf("model not found")
	}
	channelModels, err := ChannelLLMList(ctx)
	if err != nil {
		return err
	}
	for _, channelModel := range channelModels {
		if strings.EqualFold(strings.TrimSpace(channelModel.Name), modelName) {
			return fmt.Errorf("model is referenced by channel")
		}
	}
	if err := db.GetDB().WithContext(ctx).Delete(&model.LLMInfo{Name: modelName}).Error; err != nil {
		return err
	}
	llmModelCache.Del(modelName)
	return nil
}
func LLMBatchDelete(modelNames []string, ctx context.Context) error {
	normalizedNames := normalizeLLMNames(modelNames)
	if len(normalizedNames) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).Where("name IN ?", normalizedNames).Delete(&model.LLMInfo{}).Error; err != nil {
		return err
	}
	llmModelCache.Del(normalizedNames...)
	return nil
}
func LLMCreate(model model.LLMInfo, ctx context.Context) error {
	model.Name = strings.ToLower(strings.TrimSpace(model.Name))
	if model.Name == "" {
		return fmt.Errorf("model name is empty")
	}
	_, ok := llmModelCache.Get(model.Name)
	if ok {
		return fmt.Errorf("model already exists")
	}
	if err := db.GetDB().WithContext(ctx).Create(&model).Error; err != nil {
		return err
	}
	llmModelCache.Set(model.Name, model.LLMPrice)
	return nil
}
func LLMBatchCreate(llmInfos []model.LLMInfo, ctx context.Context) error {
	if len(llmInfos) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(llmInfos))
	newLLMInfos := make([]model.LLMInfo, 0, len(llmInfos))
	for _, llmInfo := range llmInfos {
		llmInfo.Name = strings.ToLower(strings.TrimSpace(llmInfo.Name))
		if llmInfo.Name == "" {
			continue
		}
		if _, ok := seen[llmInfo.Name]; ok {
			continue
		}
		if _, ok := llmModelCache.Get(llmInfo.Name); ok {
			continue
		}
		seen[llmInfo.Name] = struct{}{}
		newLLMInfos = append(newLLMInfos, llmInfo)
	}
	if len(newLLMInfos) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&newLLMInfos).Error; err != nil {
		return err
	}
	names := make([]string, len(newLLMInfos))
	for i, llmInfo := range newLLMInfos {
		names[i] = llmInfo.Name
	}
	var savedLLMInfos []model.LLMInfo
	if err := db.GetReadDB().WithContext(ctx).Where("name IN ?", names).Find(&savedLLMInfos).Error; err != nil {
		return err
	}
	for _, llmInfo := range savedLLMInfos {
		llmModelCache.Set(llmInfo.Name, llmInfo.LLMPrice)
	}
	return nil
}

// LLMBatchSave upserts model prices and refreshes the cache only after the
// database write succeeds. It is used by the explicit price rebuild action.
func LLMBatchSave(llmInfos []model.LLMInfo, ctx context.Context) error {
	if len(llmInfos) == 0 {
		return nil
	}
	normalized := make([]model.LLMInfo, 0, len(llmInfos))
	seen := make(map[string]struct{}, len(llmInfos))
	for _, llmInfo := range llmInfos {
		llmInfo.Name = strings.ToLower(strings.TrimSpace(llmInfo.Name))
		if llmInfo.Name == "" {
			continue
		}
		if _, ok := seen[llmInfo.Name]; ok {
			continue
		}
		seen[llmInfo.Name] = struct{}{}
		normalized = append(normalized, llmInfo)
	}
	if len(normalized) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).
		Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&normalized).Error; err != nil {
		return err
	}
	for _, llmInfo := range normalized {
		llmModelCache.Set(llmInfo.Name, llmInfo.LLMPrice)
	}
	return nil
}

// LLMCleanupGhosts removes price rows that are no longer referenced by any
// channel. It is intentionally explicit so a user can keep an unreferenced
// custom price until choosing to rebuild/clean the price table.
func LLMCleanupGhosts(ctx context.Context) error {
	channelModels, err := ChannelLLMList(ctx)
	if err != nil {
		return err
	}
	referenced := make(map[string]struct{}, len(channelModels))
	for _, channelModel := range channelModels {
		name := strings.ToLower(strings.TrimSpace(channelModel.Name))
		if name != "" {
			referenced[name] = struct{}{}
		}
	}

	ghosts := make([]string, 0)
	for name := range llmModelCache.GetAll() {
		if _, ok := referenced[strings.ToLower(name)]; !ok {
			ghosts = append(ghosts, name)
		}
	}
	if len(ghosts) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).Where("name IN ?", ghosts).Delete(&model.LLMInfo{}).Error; err != nil {
		return err
	}
	llmModelCache.Del(ghosts...)
	return nil
}
func LLMGet(name string) (model.LLMPrice, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	price, ok := llmModelCache.Get(name)
	if !ok {
		return model.LLMPrice{}, fmt.Errorf("model not found")
	}
	return price, nil
}

func llmRefreshCache(ctx context.Context) error {
	models := []model.LLMInfo{}
	if err := db.GetReadDB().WithContext(ctx).Find(&models).Error; err != nil {
		return err
	}
	llmModelCache.Clear()
	for _, model := range models {
		name := strings.ToLower(strings.TrimSpace(model.Name))
		if name != "" {
			llmModelCache.Set(name, model.LLMPrice)
		}
	}
	return nil
}

func normalizeLLMNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized
}
