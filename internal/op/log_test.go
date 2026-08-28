package op

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func TestSQLiteMaintenanceReclaimsBoundedPagesWhenIdle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance.db")
	if err := db.InitDB("sqlite", path, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows := make([]model.RelayLog, 0, 400)
	payload := strings.Repeat("payload", 5*1024)
	for i := 0; i < cap(rows); i++ {
		rows = append(rows, model.RelayLog{
			ID:             int64(i + 1),
			Time:           time.Now().Unix(),
			RequestContent: payload,
		})
	}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatalf("insert relay logs failed: %v", err)
	}
	if err := db.GetDB().Where("1 = 1").Delete(&model.RelayLog{}).Error; err != nil {
		t.Fatalf("delete relay logs failed: %v", err)
	}

	before, err := db.InspectSQLitePragmas(context.Background())
	if err != nil {
		t.Fatalf("inspect before maintenance failed: %v", err)
	}
	if before.FreelistCount < sqliteIncrementalVacuumMinFreePages {
		t.Fatalf("expected enough free pages for maintenance, got %d", before.FreelistCount)
	}

	config := conf.SQLiteMaintenance{
		Enabled:                     true,
		IdleSeconds:                 1,
		MinDatabaseBytes:            0,
		MinReclaimableBytes:         1,
		WALCheckpointThresholdBytes: 1,
		MaxPagesPerRun:              1,
		MaxDurationSeconds:          10,
	}
	if err := sqliteMaintenanceIfIdle(context.Background(), config); err != nil {
		t.Fatalf("sqlite maintenance failed: %v", err)
	}

	after, err := db.InspectSQLitePragmas(context.Background())
	if err != nil {
		t.Fatalf("inspect after maintenance failed: %v", err)
	}
	if after.PageCount >= before.PageCount {
		t.Fatalf("expected bounded vacuum to reduce page count, before=%d after=%d", before.PageCount, after.PageCount)
	}
	if before.PageCount-after.PageCount > config.MaxPagesPerRun {
		t.Fatalf("vacuum reclaimed too many pages, before=%d after=%d max=%d", before.PageCount, after.PageCount, config.MaxPagesPerRun)
	}
}

func TestRelayActivityTooRecent(t *testing.T) {
	if !relayActivityTooRecent(time.Now(), 10) {
		t.Fatal("expected recent activity to block maintenance")
	}
	if relayActivityTooRecent(time.Now().Add(-time.Minute), 10) {
		t.Fatal("expected old activity to allow maintenance")
	}
	if relayActivityTooRecent(time.Time{}, 10) {
		t.Fatal("zero activity time should not block maintenance")
	}
}

func TestIsSQLiteBusyError(t *testing.T) {
	if !isSQLiteBusyError(errors.New("database is locked")) {
		t.Fatal("expected database lock to be classified as sqlite busy")
	}
	if isSQLiteBusyError(context.Canceled) {
		t.Fatal("context cancellation must not be classified as sqlite busy")
	}
}
