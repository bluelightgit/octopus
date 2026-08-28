package op

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/activity"
	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/snowflake"
)

const relayLogMaxSize = 20
const relayLogMaxSizeNoDB = 100 // 当不保存到数据库时，允许更大的缓存用于实时查询
const relayLogCleanupBatchSize = 2000

const (
	sqliteIncrementalVacuumMinFreePages = 1024
	sqliteIncrementalVacuumMinFreeRatio = 0.10
)

var relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
var relayLogCacheLock sync.Mutex

var relayLogFlushLock sync.Mutex

var sqliteMaintenanceLock sync.Mutex

var relayLogSubscribers = make(map[chan model.RelayLog]struct{})
var relayLogSubscribersLock sync.RWMutex

var relayLogStreamTokens = make(map[string]struct{})
var relayLogStreamTokensLock sync.RWMutex

func RelayLogStreamTokenCreate() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes)

	relayLogStreamTokensLock.Lock()
	relayLogStreamTokens[token] = struct{}{}
	relayLogStreamTokensLock.Unlock()

	return token, nil
}

func RelayLogStreamTokenVerify(token string) bool {
	relayLogStreamTokensLock.RLock()
	_, ok := relayLogStreamTokens[token]
	relayLogStreamTokensLock.RUnlock()
	return ok
}

func RelayLogStreamTokenRevoke(token string) {
	relayLogStreamTokensLock.Lock()
	delete(relayLogStreamTokens, token)
	relayLogStreamTokensLock.Unlock()
}

func RelayLogSubscribe() chan model.RelayLog {
	ch := make(chan model.RelayLog, 10)
	relayLogSubscribersLock.Lock()
	relayLogSubscribers[ch] = struct{}{}
	relayLogSubscribersLock.Unlock()
	return ch
}

func RelayLogUnsubscribe(ch chan model.RelayLog) {
	relayLogSubscribersLock.Lock()
	delete(relayLogSubscribers, ch)
	relayLogSubscribersLock.Unlock()
	close(ch)
}

func notifySubscribers(relayLog model.RelayLog) {
	relayLogSubscribersLock.RLock()
	defer relayLogSubscribersLock.RUnlock()

	for ch := range relayLogSubscribers {
		select {
		case ch <- relayLog:
		default:
		}
	}
}

func relayLogFlushToDB(ctx context.Context) error {
	relayLogFlushLock.Lock()
	defer relayLogFlushLock.Unlock()

	relayLogCacheLock.Lock()
	if len(relayLogCache) == 0 {
		relayLogCacheLock.Unlock()
		return nil
	}
	batch := make([]model.RelayLog, len(relayLogCache))
	copy(batch, relayLogCache)
	flushedUpto := len(batch)
	relayLogCacheLock.Unlock()

	result := db.GetDB().WithContext(ctx).Create(&batch)
	if result.Error != nil {
		return result.Error
	}

	relayLogCacheLock.Lock()
	if len(relayLogCache) >= flushedUpto {
		relayLogCache = relayLogCache[flushedUpto:]
	} else {
		relayLogCache = relayLogCache[:0]
	}
	if len(relayLogCache) == 0 {
		relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
	}
	relayLogCacheLock.Unlock()

	return nil
}

func RelayLogAdd(ctx context.Context, relayLog model.RelayLog) error {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return err
	}
	maxSize := relayLogMaxSize
	if !enabled {
		maxSize = relayLogMaxSizeNoDB
	}
	relayLog.ID = snowflake.GenerateID()
	go notifySubscribers(relayLog)

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, relayLog)
	if len(relayLogCache) >= maxSize {
		if enabled {
			relayLogCacheLock.Unlock()
			return relayLogFlushToDB(ctx)
		}
		// 如果未启用日志保存，移除最旧的日志，保留最新的日志用于实时查询
		keepSize := maxSize / 2
		if len(relayLogCache) > keepSize {
			relayLogCache = relayLogCache[len(relayLogCache)-keepSize:]
		}
	}
	relayLogCacheLock.Unlock()
	return nil
}

func RelayLogSaveDBTask(ctx context.Context) error {
	log.Debugf("relay log save db task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("relay log save db task finished, save time: %s", time.Since(startTime))
	}()
	return relayLogSaveDB(ctx)
}

func relayLogSaveDB(ctx context.Context) error {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return err
	}

	if enabled {
		if err := relayLogFlushToDB(ctx); err != nil {
			return err
		}
		if err := relayLogCleanup(ctx); err != nil {
			return err
		}
		return nil
	}

	// 如果未启用日志保存，检查缓存大小，如果超过限制则清理旧日志
	relayLogCacheLock.Lock()
	if len(relayLogCache) > relayLogMaxSizeNoDB {
		keepSize := relayLogMaxSizeNoDB / 2
		relayLogCache = relayLogCache[len(relayLogCache)-keepSize:]
	}
	relayLogCacheLock.Unlock()
	if removed, sweepErr := RelayLogBodySweep(ctx); sweepErr != nil {
		log.Warnf("failed to sweep relay body storage with log persistence disabled: %v", sweepErr)
	} else if removed > 0 {
		log.Infof("removed %d unreferenced relay body files", removed)
	}

	return nil
}

// SQLiteMaintenanceTask combines the regular relay-log flush/retention pass
// with the SQLite-only compaction pass. Keeping this separate from the normal
// log task means non-SQLite deployments retain their existing behavior, while
// SQLite can use a configurable interval and idle-aware maintenance policy.
func SQLiteMaintenanceTask(ctx context.Context) error {
	if !db.IsSQLite() {
		return nil
	}

	config := conf.AppConfig.SQLiteMaintenance.WithDefaults()
	if !config.Enabled {
		return relayLogSaveDB(ctx)
	}

	if err := relayLogSaveDB(ctx); err != nil {
		return err
	}

	maintenanceCtx, cancel := context.WithTimeout(ctx, time.Duration(config.MaxDurationSeconds)*time.Second)
	defer cancel()
	return sqliteMaintenanceIfIdle(maintenanceCtx, config)
}

func sqliteMaintenanceIfIdle(ctx context.Context, config conf.SQLiteMaintenance) error {
	active, lastActivity := activity.Snapshot()
	if active > 0 || relayActivityTooRecent(lastActivity, config.IdleSeconds) {
		log.Debugf("sqlite maintenance skipped: active_relay_requests=%d last_activity=%s", active, formatActivityTime(lastActivity))
		return nil
	}

	release, ok := activity.TryBeginMaintenance()
	if !ok {
		log.Debugf("sqlite maintenance skipped: relay activity changed before maintenance started")
		return nil
	}
	defer release()

	// Recheck after taking the exclusive request-entry gate. This closes the
	// race where a new relay request arrives between Snapshot and TryLock.
	active, lastActivity = activity.Snapshot()
	if active > 0 || relayActivityTooRecent(lastActivity, config.IdleSeconds) {
		log.Debugf("sqlite maintenance skipped after recheck: active_relay_requests=%d last_activity=%s", active, formatActivityTime(lastActivity))
		return nil
	}

	if !sqliteMaintenanceLock.TryLock() {
		log.Debugf("sqlite maintenance skipped: another sqlite maintenance pass is running")
		return nil
	}
	defer sqliteMaintenanceLock.Unlock()

	if _, err := db.SQLiteWALCheckpointIfNeeded(ctx, config.WALCheckpointThresholdBytes); err != nil {
		if isSQLiteBusyError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.Debugf("sqlite maintenance skipped during wal checkpoint: %v", err)
			return nil
		}
		return err
	}

	status, err := db.InspectSQLitePragmas(ctx)
	if err != nil {
		if isSQLiteBusyError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.Debugf("sqlite maintenance skipped while inspecting database: %v", err)
			return nil
		}
		return err
	}
	if status == nil || status.AutoVacuumNeedsVacuum {
		if status != nil {
			log.Warnf("sqlite maintenance skipped: auto_vacuum=%s requires one-time `octopus sqlite repair` while the service is stopped", status.AutoVacuumMode)
		}
		return nil
	}
	if config.MinDatabaseBytes > 0 && status.TotalSizeBytes < config.MinDatabaseBytes {
		return nil
	}
	if status.FreelistCount <= 0 || status.ReclaimableBytes < config.MinReclaimableBytes {
		return nil
	}

	beforeSize := status.TotalSizeBytes
	beforeReclaimable := status.ReclaimableBytes
	log.Debugf("sqlite incremental vacuum started: total_size_bytes=%d reclaimable_bytes=%d page_count=%d freelist_count=%d max_pages=%d", beforeSize, beforeReclaimable, status.PageCount, status.FreelistCount, config.MaxPagesPerRun)
	if err := db.SQLiteIncrementalVacuum(ctx, config.MaxPagesPerRun); err != nil {
		if isSQLiteBusyError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.Debugf("sqlite incremental vacuum skipped: %v", err)
			return nil
		}
		return err
	}

	// The vacuum itself writes pages and can create a new WAL segment. Try to
	// truncate it immediately so the reported on-disk size reflects the pass.
	if result, checkpointErr := db.SQLiteWALCheckpoint(ctx, db.SQLiteCheckpointModeTruncate); checkpointErr != nil {
		if !isSQLiteBusyError(checkpointErr) && !errors.Is(checkpointErr, context.DeadlineExceeded) && !errors.Is(checkpointErr, context.Canceled) {
			log.Warnf("sqlite maintenance post-vacuum checkpoint failed: %v", checkpointErr)
		}
	} else if result != nil && result.BusyFrames > 0 {
		log.Debugf("sqlite maintenance post-vacuum checkpoint incomplete: busy=%d log=%d checkpointed=%d", result.BusyFrames, result.LogFrames, result.CheckpointedFrames)
	}

	after, inspectErr := db.InspectSQLitePragmas(ctx)
	if inspectErr != nil {
		if isSQLiteBusyError(inspectErr) || errors.Is(inspectErr, context.DeadlineExceeded) || errors.Is(inspectErr, context.Canceled) {
			return nil
		}
		return inspectErr
	}
	if after != nil {
		log.Infof("sqlite incremental vacuum finished: reclaimed_bytes=%d reclaimable_bytes=%d->%d total_size_bytes=%d->%d", maxInt64(0, beforeSize-after.TotalSizeBytes), beforeReclaimable, after.ReclaimableBytes, beforeSize, after.TotalSizeBytes)
	}
	return nil
}

func relayActivityTooRecent(lastActivity time.Time, idleSeconds int) bool {
	if lastActivity.IsZero() || idleSeconds <= 0 {
		return false
	}
	return time.Since(lastActivity) < time.Duration(idleSeconds)*time.Second
}

func formatActivityTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") || strings.Contains(message, "sqlite_busy")
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func relayLogCleanup(ctx context.Context) error {
	keepPeriod, err := SettingGetInt(model.SettingKeyRelayLogKeepPeriod)
	if err != nil {
		return err
	}

	dbConn := db.GetDB().WithContext(ctx)
	if keepPeriod > 0 {
		cutoffTime := time.Now().Add(-time.Duration(keepPeriod) * 24 * time.Hour).Unix()
		for {
			result := dbConn.Exec(`DELETE FROM relay_logs WHERE id IN (
				SELECT id FROM relay_logs WHERE time < ? ORDER BY time ASC LIMIT ?
			)`, cutoffTime, relayLogCleanupBatchSize)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected < relayLogCleanupBatchSize {
				break
			}
		}
	}
	if db.IsSQLite() {
		if err := dbConn.Exec("PRAGMA optimize;").Error; err != nil {
			return err
		}
	}
	if removed, sweepErr := RelayLogBodySweep(ctx); sweepErr != nil {
		log.Warnf("failed to sweep relay body storage after log cleanup: %v", sweepErr)
	} else if removed > 0 {
		log.Infof("removed %d unreferenced relay body files", removed)
	}
	return nil
}

func sqliteIncrementalVacuumIfNeeded(ctx context.Context) error {
	if !db.IsSQLite() {
		return nil
	}

	dbConn := db.GetDB().WithContext(ctx)
	if _, err := db.SQLiteWALCheckpoint(ctx, db.SQLiteCheckpointModePassive); err != nil {
		return err
	}

	var pageCount int
	if err := dbConn.Raw("PRAGMA page_count;").Row().Scan(&pageCount); err != nil {
		return err
	}

	var freelistCount int
	if err := dbConn.Raw("PRAGMA freelist_count;").Row().Scan(&freelistCount); err != nil {
		return err
	}

	if pageCount <= 0 || freelistCount < sqliteIncrementalVacuumMinFreePages {
		return nil
	}

	freeRatio := float64(freelistCount) / float64(pageCount)
	if freeRatio < sqliteIncrementalVacuumMinFreeRatio {
		return nil
	}

	log.Debugf("sqlite incremental vacuum triggered, page_count=%d, freelist_count=%d, free_ratio=%.2f", pageCount, freelistCount, freeRatio)
	if err := dbConn.Exec("PRAGMA incremental_vacuum;").Error; err != nil {
		return err
	}
	return nil
}

// RelayLogList 查询日志列表，支持可选的时间范围过滤
// startTime 和 endTime 为 nil 时表示不限制时间范围
func RelayLogList(ctx context.Context, startTime, endTime *int, page, pageSize int) ([]model.RelayLog, error) {
	enabled, err := SettingGetBool(model.SettingKeyRelayLogKeepEnabled)
	if err != nil {
		return nil, err
	}
	hasTimeFilter := startTime != nil && endTime != nil

	// 获取缓存中符合条件的日志
	relayLogCacheLock.Lock()
	var cachedLogs []model.RelayLog
	for _, log := range relayLogCache {
		if hasTimeFilter {
			if log.Time >= int64(*startTime) && log.Time <= int64(*endTime) {
				cachedLogs = append(cachedLogs, log)
			}
		} else {
			cachedLogs = append(cachedLogs, log)
		}
	}
	relayLogCacheLock.Unlock()

	// 反转缓存日志顺序（原本新的在末尾，反转后新的在前面，方便分页）
	for i, j := 0, len(cachedLogs)-1; i < j; i, j = i+1, j-1 {
		cachedLogs[i], cachedLogs[j] = cachedLogs[j], cachedLogs[i]
	}

	cacheCount := len(cachedLogs)
	offset := (page - 1) * pageSize

	var result []model.RelayLog

	// 先从缓存中取（缓存是最新的日志）
	if offset < cacheCount {
		cacheEnd := offset + pageSize
		if cacheEnd > cacheCount {
			cacheEnd = cacheCount
		}
		result = append(result, cachedLogs[offset:cacheEnd]...)
	}

	// 如果启用了日志保存，缓存不够时从数据库补充
	if enabled {
		remaining := pageSize - len(result)
		if remaining > 0 {
			dbOffset := 0
			if offset > cacheCount {
				dbOffset = offset - cacheCount
			}

			query := db.GetReadDB().WithContext(ctx)
			if hasTimeFilter {
				query = query.Where("time >= ? AND time <= ?", *startTime, *endTime)
			}

			var dbLogs []model.RelayLog
			if err := query.Order("id DESC").Offset(dbOffset).Limit(remaining).Find(&dbLogs).Error; err != nil {
				return nil, err
			}
			result = append(result, dbLogs...)
		}
	}

	return result, nil
}

func RelayLogClear(ctx context.Context) error {
	relayLogFlushLock.Lock()
	defer relayLogFlushLock.Unlock()

	relayLogCacheLock.Lock()
	relayLogCache = make([]model.RelayLog, 0, relayLogMaxSize)
	relayLogCacheLock.Unlock()
	if err := db.GetDB().WithContext(ctx).Where("1 = 1").Delete(&model.RelayLog{}).Error; err != nil {
		return err
	}
	if _, err := db.SQLiteWALCheckpoint(ctx, db.SQLiteCheckpointModeTruncate); err != nil {
		return err
	}
	if err := sqliteIncrementalVacuumIfNeeded(ctx); err != nil {
		return err
	}
	if removed, err := RelayLogBodySweep(ctx); err != nil {
		log.Warnf("failed to sweep relay body storage after clearing logs: %v", err)
	} else if removed > 0 {
		log.Infof("removed %d relay body files after clearing logs", removed)
	}
	return nil
}
