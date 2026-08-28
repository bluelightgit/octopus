package conf

import "testing"

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
