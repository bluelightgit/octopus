package task

import (
	"context"
	"time"

	"github.com/bluelightgit/octopus/internal/conf"
	"github.com/bluelightgit/octopus/internal/db"
	"github.com/bluelightgit/octopus/internal/model"
	"github.com/bluelightgit/octopus/internal/op"
	"github.com/bluelightgit/octopus/internal/price"
	"github.com/bluelightgit/octopus/internal/utils/log"
)

const (
	TaskPriceUpdate       = "price_update"
	TaskStatsSave         = "stats_save"
	TaskRelayLogSave      = "relay_log_save"
	TaskSQLiteMaintenance = "sqlite_maintenance"
	TaskSyncLLM           = "sync_llm"
	TaskCleanLLM          = "clean_llm"
	TaskBaseUrlDelay      = "base_url_delay"
)

func Init() {
	registerRelayLogTask()

	priceUpdateIntervalHours, err := op.SettingGetInt(model.SettingKeyModelInfoUpdateInterval)
	if err != nil {
		log.Errorf("failed to get model info update interval: %v", err)
		return
	}
	priceUpdateInterval := time.Duration(priceUpdateIntervalHours) * time.Hour
	// 注册价格更新任务
	Register(string(model.SettingKeyModelInfoUpdateInterval), priceUpdateInterval, true, func() {
		if err := price.UpdateLLMPrice(context.Background()); err != nil {
			log.Warnf("failed to update price info: %v", err)
		}
	})

	// 注册基础URL延迟任务
	Register(TaskBaseUrlDelay, 1*time.Hour, true, ChannelBaseUrlDelayTask)

	// 注册LLM同步任务
	syncLLMIntervalHours, err := op.SettingGetInt(model.SettingKeySyncLLMInterval)
	if err != nil {
		log.Warnf("failed to get sync LLM interval: %v", err)
		return
	}
	syncLLMInterval := time.Duration(syncLLMIntervalHours) * time.Hour
	Register(string(model.SettingKeySyncLLMInterval), syncLLMInterval, true, SyncModelsTask)

	// 注册统计保存任务
	statsSaveIntervalMinutes, err := op.SettingGetInt(model.SettingKeyStatsSaveInterval)
	if err != nil {
		log.Warnf("failed to get stats save interval: %v", err)
		return
	}
	statsSaveInterval := time.Duration(statsSaveIntervalMinutes) * time.Minute
	Register(TaskStatsSave, statsSaveInterval, false, op.StatsSaveDBTask)
}

func registerRelayLogTask() {
	if db.IsSQLite() {
		config := conf.AppConfig.SQLiteMaintenance.WithDefaults()
		if config.Enabled {
			Register(TaskSQLiteMaintenance, time.Duration(config.IntervalSeconds)*time.Second, false, func() {
				if err := op.SQLiteMaintenanceTask(context.Background()); err != nil {
					log.Warnf("sqlite maintenance task failed: %v", err)
				}
			})
			return
		}
	}

	// Non-SQLite databases and explicitly disabled SQLite maintenance retain
	// the regular relay-log persistence task.
	Register(TaskRelayLogSave, 10*time.Minute, false, func() {
		if err := op.RelayLogSaveDBTask(context.Background()); err != nil {
			log.Warnf("relay log save db task failed: %v", err)
		}
	})
}
