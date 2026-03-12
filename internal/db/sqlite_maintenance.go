package db

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/bestruirui/octopus/internal/utils/log"
	"gorm.io/gorm"
)

const (
	SQLiteAutoVacuumNone        = 0
	SQLiteAutoVacuumFull        = 1
	SQLiteAutoVacuumIncremental = 2

	sqliteDesiredJournalMode         = "wal"
	sqliteRepairJournalMode          = "delete"
	sqliteDesiredWALAutoCheckpoint   = 1000
	SQLiteWALCheckpointTriggerSize   = 64 << 20
	SQLiteMaintenanceWarnFreelistMin = 1024
)

type SQLiteCheckpointMode string

const (
	SQLiteCheckpointModePassive  SQLiteCheckpointMode = "PASSIVE"
	SQLiteCheckpointModeTruncate SQLiteCheckpointMode = "TRUNCATE"
)

type SQLitePragmaStatus struct {
	DBPath                string
	JournalMode           string
	AutoVacuum            int
	AutoVacuumMode        string
	WALAutoCheckpoint     int
	PageCount             int
	FreelistCount         int
	WALSizeBytes          int64
	AutoVacuumNeedsVacuum bool
}

type SQLiteCheckpointResult struct {
	BusyFrames         int
	LogFrames          int
	CheckpointedFrames int
}

func EnsureSQLiteRuntimePragmas(ctx context.Context) (*SQLitePragmaStatus, error) {
	if !IsSQLite() {
		return nil, nil
	}

	mode, err := setSQLiteJournalMode(ctx, sqliteDesiredJournalMode)
	if err != nil {
		return nil, err
	}
	if mode != sqliteDesiredJournalMode {
		return nil, fmt.Errorf("failed to enable sqlite WAL mode, current journal_mode=%s", mode)
	}

	if err := setSQLiteWALAutoCheckpoint(ctx, sqliteDesiredWALAutoCheckpoint); err != nil {
		return nil, err
	}

	status, err := InspectSQLitePragmas(ctx)
	if err != nil {
		return nil, err
	}
	if status != nil && status.AutoVacuumNeedsVacuum {
		log.Warnf("sqlite auto_vacuum is %s, incremental vacuum is inactive until the database is repaired with `octopus sqlite repair` while the service is stopped", status.AutoVacuumMode)
	}
	return status, nil
}

func InspectSQLitePragmas(ctx context.Context) (*SQLitePragmaStatus, error) {
	if !IsSQLite() {
		return nil, nil
	}

	dbConn := GetDB().WithContext(ctx)
	status := &SQLitePragmaStatus{DBPath: sqlitePath}

	journalMode, err := querySQLiteStringPragma(dbConn, "journal_mode")
	if err != nil {
		return nil, err
	}
	status.JournalMode = strings.ToLower(strings.TrimSpace(journalMode))

	autoVacuum, err := querySQLiteIntPragma(dbConn, "auto_vacuum")
	if err != nil {
		return nil, err
	}
	status.AutoVacuum = autoVacuum
	status.AutoVacuumMode = sqliteAutoVacuumModeName(autoVacuum)
	status.AutoVacuumNeedsVacuum = autoVacuum != SQLiteAutoVacuumIncremental

	if status.WALAutoCheckpoint, err = querySQLiteIntPragma(dbConn, "wal_autocheckpoint"); err != nil {
		return nil, err
	}
	if status.PageCount, err = querySQLiteIntPragma(dbConn, "page_count"); err != nil {
		return nil, err
	}
	if status.FreelistCount, err = querySQLiteIntPragma(dbConn, "freelist_count"); err != nil {
		return nil, err
	}

	if sqlitePath != "" {
		if info, statErr := os.Stat(sqlitePath + "-wal"); statErr == nil {
			status.WALSizeBytes = info.Size()
		} else if !os.IsNotExist(statErr) {
			return nil, statErr
		}
	}

	return status, nil
}

func SQLiteWALCheckpoint(ctx context.Context, mode SQLiteCheckpointMode) (*SQLiteCheckpointResult, error) {
	if !IsSQLite() {
		return nil, nil
	}

	dbConn := GetDB().WithContext(ctx)
	row := dbConn.Raw(fmt.Sprintf("PRAGMA wal_checkpoint(%s);", strings.ToUpper(string(mode)))).Row()
	result := &SQLiteCheckpointResult{}
	if err := row.Scan(&result.BusyFrames, &result.LogFrames, &result.CheckpointedFrames); err != nil {
		return nil, err
	}
	return result, nil
}

func SQLiteWALCheckpointIfNeeded(ctx context.Context, thresholdBytes int64) (*SQLiteCheckpointResult, error) {
	if !IsSQLite() || sqlitePath == "" {
		return nil, nil
	}

	info, err := os.Stat(sqlitePath + "-wal")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if thresholdBytes > 0 && info.Size() < thresholdBytes {
		return nil, nil
	}

	result, err := SQLiteWALCheckpoint(ctx, SQLiteCheckpointModeTruncate)
	if err != nil {
		return nil, err
	}
	if result != nil && result.BusyFrames > 0 {
		log.Warnf("sqlite wal checkpoint truncate incomplete: busy=%d log=%d checkpointed=%d", result.BusyFrames, result.LogFrames, result.CheckpointedFrames)
	}
	return result, nil
}

func RepairSQLiteAutoVacuum(ctx context.Context) (*SQLitePragmaStatus, error) {
	if !IsSQLite() {
		return nil, fmt.Errorf("database is not sqlite")
	}

	if _, err := SQLiteWALCheckpoint(ctx, SQLiteCheckpointModeTruncate); err != nil {
		return nil, err
	}
	mode, err := setSQLiteJournalMode(ctx, sqliteRepairJournalMode)
	if err != nil {
		return nil, err
	}
	if mode != sqliteRepairJournalMode {
		return nil, fmt.Errorf("failed to switch sqlite journal_mode to %s before vacuum, current=%s", sqliteRepairJournalMode, mode)
	}

	if err := GetDB().WithContext(ctx).Exec("PRAGMA auto_vacuum=INCREMENTAL;").Error; err != nil {
		return nil, err
	}
	if err := GetDB().WithContext(ctx).Exec("VACUUM;").Error; err != nil {
		return nil, err
	}

	status, err := EnsureSQLiteRuntimePragmas(ctx)
	if err != nil {
		return nil, err
	}
	if status == nil || status.AutoVacuum != SQLiteAutoVacuumIncremental {
		return nil, fmt.Errorf("sqlite auto_vacuum repair did not take effect")
	}
	return status, nil
}

func sqliteAutoVacuumModeName(mode int) string {
	switch mode {
	case SQLiteAutoVacuumNone:
		return "none"
	case SQLiteAutoVacuumFull:
		return "full"
	case SQLiteAutoVacuumIncremental:
		return "incremental"
	default:
		return fmt.Sprintf("unknown(%d)", mode)
	}
}

func setSQLiteJournalMode(ctx context.Context, mode string) (string, error) {
	dbConn := GetDB().WithContext(ctx)
	row := dbConn.Raw(fmt.Sprintf("PRAGMA journal_mode=%s;", strings.ToUpper(mode))).Row()
	var current string
	if err := row.Scan(&current); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(current)), nil
}

func setSQLiteWALAutoCheckpoint(ctx context.Context, pages int) error {
	if !IsSQLite() {
		return nil
	}
	return GetDB().WithContext(ctx).Exec(fmt.Sprintf("PRAGMA wal_autocheckpoint=%d;", pages)).Error
}

func querySQLiteIntPragma(dbConn *gorm.DB, name string) (int, error) {
	row := dbConn.Raw(fmt.Sprintf("PRAGMA %s;", name)).Row()
	var value int
	if err := row.Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}

func querySQLiteStringPragma(dbConn *gorm.DB, name string) (string, error) {
	row := dbConn.Raw(fmt.Sprintf("PRAGMA %s;", name)).Row()
	var value string
	if err := row.Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}
