package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func createLegacySQLiteDB(t *testing.T, path string) {
	t.Helper()

	legacyDB, err := gorm.Open(sqlite.Open(path+"?_journal_mode=DELETE&_synchronous=NORMAL&_foreign_keys=ON"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open legacy sqlite failed: %v", err)
	}
	sqlDB, err := legacyDB.DB()
	if err != nil {
		t.Fatalf("legacy sqlite db handle failed: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := legacyDB.Exec("PRAGMA auto_vacuum=NONE;").Error; err != nil {
		t.Fatalf("set legacy auto_vacuum failed: %v", err)
	}
	if err := legacyDB.Exec("CREATE TABLE IF NOT EXISTS legacy_probe (id INTEGER PRIMARY KEY, value TEXT);").Error; err != nil {
		t.Fatalf("create legacy table failed: %v", err)
	}
	if err := legacyDB.Exec("INSERT INTO legacy_probe(value) VALUES ('legacy');").Error; err != nil {
		t.Fatalf("insert legacy row failed: %v", err)
	}
}

func TestEnsureSQLiteRuntimePragmas_DetectsLegacyAutoVacuum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacySQLiteDB(t, path)

	if err := InitDB("sqlite", path, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = Close() })

	status, err := EnsureSQLiteRuntimePragmas(context.Background())
	if err != nil {
		t.Fatalf("EnsureSQLiteRuntimePragmas failed: %v", err)
	}
	if status == nil {
		t.Fatal("expected sqlite pragma status")
	}
	if status.JournalMode != sqliteDesiredJournalMode {
		t.Fatalf("expected journal_mode=%s, got %s", sqliteDesiredJournalMode, status.JournalMode)
	}
	if status.AutoVacuum != SQLiteAutoVacuumNone {
		t.Fatalf("expected legacy auto_vacuum=%d, got %d", SQLiteAutoVacuumNone, status.AutoVacuum)
	}
	if !status.AutoVacuumNeedsVacuum {
		t.Fatal("expected legacy database to require vacuum repair")
	}
}

func TestRepairSQLiteAutoVacuum_RepairsLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-repair.db")
	createLegacySQLiteDB(t, path)

	if err := InitDB("sqlite", path, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = Close() })

	status, err := RepairSQLiteAutoVacuum(context.Background())
	if err != nil {
		t.Fatalf("RepairSQLiteAutoVacuum failed: %v", err)
	}
	if status == nil {
		t.Fatal("expected sqlite pragma status after repair")
	}
	if status.AutoVacuum != SQLiteAutoVacuumIncremental {
		t.Fatalf("expected auto_vacuum=%d, got %d", SQLiteAutoVacuumIncremental, status.AutoVacuum)
	}
	if status.AutoVacuumNeedsVacuum {
		t.Fatal("expected repaired database to no longer require vacuum repair")
	}
	if status.JournalMode != sqliteDesiredJournalMode {
		t.Fatalf("expected repaired journal_mode=%s, got %s", sqliteDesiredJournalMode, status.JournalMode)
	}
}
