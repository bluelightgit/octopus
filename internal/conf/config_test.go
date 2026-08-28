package conf

import "testing"

func TestRelayBodyStorageWithDefaults(t *testing.T) {
	config := (RelayBodyStorage{}).WithDefaults()
	if config.Enabled {
		t.Fatal("WithDefaults must not silently enable a zero-value struct")
	}
	if config.Directory != DefaultRelayBodyStorageDirectory {
		t.Fatalf("unexpected body directory: %q", config.Directory)
	}
	if config.InlineMaxBytes != DefaultRelayBodyStorageInlineMaxBytes {
		t.Fatalf("unexpected inline limit: %d", config.InlineMaxBytes)
	}
	if config.PreviewMaxBytes != DefaultRelayBodyStoragePreviewMaxBytes {
		t.Fatalf("unexpected preview limit: %d", config.PreviewMaxBytes)
	}
	if config.Compression != DefaultRelayBodyStorageCompression {
		t.Fatalf("unexpected compression: %q", config.Compression)
	}
}

func TestRelayBodyStorageWithDefaultsClampsPreview(t *testing.T) {
	config := RelayBodyStorage{
		Enabled:         true,
		Directory:       "custom-bodies",
		InlineMaxBytes:  100,
		PreviewMaxBytes: 200,
		Compression:     "none",
	}.WithDefaults()
	if !config.Enabled {
		t.Fatal("explicitly enabled body storage must remain enabled")
	}
	if config.Directory != "custom-bodies" || config.InlineMaxBytes != 100 || config.PreviewMaxBytes != 100 || config.Compression != "none" {
		t.Fatalf("explicit body storage configuration was changed: %+v", config)
	}
}

func TestSQLiteMaintenanceWithDefaults(t *testing.T) {
	config := SQLiteMaintenance{Enabled: true}.WithDefaults()
	if !config.Enabled {
		t.Fatal("enabled configuration must remain enabled")
	}
	if config.IntervalSeconds != DefaultSQLiteMaintenanceIntervalSeconds {
		t.Fatalf("unexpected interval: %d", config.IntervalSeconds)
	}
	if config.IdleSeconds != DefaultSQLiteMaintenanceIdleSeconds {
		t.Fatalf("unexpected idle period: %d", config.IdleSeconds)
	}
	if config.MinDatabaseBytes != 0 {
		t.Fatalf("zero database threshold should disable the total-size gate: %d", config.MinDatabaseBytes)
	}
	if config.MinReclaimableBytes != DefaultSQLiteMaintenanceMinReclaimableBytes {
		t.Fatalf("unexpected reclaimable threshold: %d", config.MinReclaimableBytes)
	}
	if config.WALCheckpointThresholdBytes != DefaultSQLiteMaintenanceWALCheckpointThresholdBytes {
		t.Fatalf("unexpected WAL threshold: %d", config.WALCheckpointThresholdBytes)
	}
	if config.MaxPagesPerRun != DefaultSQLiteMaintenanceMaxPagesPerRun {
		t.Fatalf("unexpected page limit: %d", config.MaxPagesPerRun)
	}
	if config.MaxDurationSeconds != DefaultSQLiteMaintenanceMaxDurationSeconds {
		t.Fatalf("unexpected duration limit: %d", config.MaxDurationSeconds)
	}
}

func TestSQLiteMaintenanceWithDefaultsPreservesExplicitValues(t *testing.T) {
	config := SQLiteMaintenance{
		Enabled:                     false,
		IntervalSeconds:             30,
		IdleSeconds:                 10,
		MinDatabaseBytes:            1024,
		MinReclaimableBytes:         2048,
		WALCheckpointThresholdBytes: 4096,
		MaxPagesPerRun:              16,
		MaxDurationSeconds:          2,
	}.WithDefaults()

	if config.Enabled {
		t.Fatal("explicitly disabled maintenance must remain disabled")
	}
	if config.IntervalSeconds != 30 || config.IdleSeconds != 10 || config.MinDatabaseBytes != 1024 || config.MinReclaimableBytes != 2048 || config.WALCheckpointThresholdBytes != 4096 || config.MaxPagesPerRun != 16 || config.MaxDurationSeconds != 2 {
		t.Fatalf("explicit configuration was changed: %+v", config)
	}
}
