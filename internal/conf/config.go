package conf

import (
	"fmt"
	"os"
	"strings"

	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/spf13/viper"
)

type Server struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

type Log struct {
	Level string `mapstructure:"level"`
}

type Database struct {
	Type string `mapstructure:"type"`
	Path string `mapstructure:"path"`
}

// SQLiteMaintenance controls the optional background maintenance pass used by
// SQLite deployments. The values are deliberately expressed in primitive
// units so they can be configured from both config.json and environment
// variables (for example, OCTOPUS_SQLITE_MAINTENANCE_MIN_DATABASE_BYTES).
//
// The maintenance pass is conservative: it only runs when there are no active
// relay requests and only reclaims a bounded number of pages per pass.
type SQLiteMaintenance struct {
	Enabled                     bool  `mapstructure:"enabled"`
	IntervalSeconds             int   `mapstructure:"interval_seconds"`
	IdleSeconds                 int   `mapstructure:"idle_seconds"`
	MinDatabaseBytes            int64 `mapstructure:"min_database_bytes"`
	MinReclaimableBytes         int64 `mapstructure:"min_reclaimable_bytes"`
	WALCheckpointThresholdBytes int64 `mapstructure:"wal_checkpoint_threshold_bytes"`
	MaxPagesPerRun              int   `mapstructure:"max_pages_per_run"`
	MaxDurationSeconds          int   `mapstructure:"max_duration_seconds"`
}

const (
	DefaultSQLiteMaintenanceIntervalSeconds             = 10 * 60
	DefaultSQLiteMaintenanceIdleSeconds                 = 5 * 60
	DefaultSQLiteMaintenanceMinDatabaseBytes            = 512 << 20
	DefaultSQLiteMaintenanceMinReclaimableBytes         = 64 << 20
	DefaultSQLiteMaintenanceWALCheckpointThresholdBytes = 64 << 20
	DefaultSQLiteMaintenanceMaxPagesPerRun              = 4096
	DefaultSQLiteMaintenanceMaxDurationSeconds          = 5
)

// WithDefaults keeps a malformed or partially populated configuration from
// disabling safety limits accidentally. Enabled=false remains the explicit
// way to disable maintenance.
func (s SQLiteMaintenance) WithDefaults() SQLiteMaintenance {
	if s.IntervalSeconds <= 0 {
		s.IntervalSeconds = DefaultSQLiteMaintenanceIntervalSeconds
	}
	if s.IdleSeconds <= 0 {
		s.IdleSeconds = DefaultSQLiteMaintenanceIdleSeconds
	}
	if s.MinDatabaseBytes < 0 {
		s.MinDatabaseBytes = DefaultSQLiteMaintenanceMinDatabaseBytes
	}
	if s.MinReclaimableBytes <= 0 {
		s.MinReclaimableBytes = DefaultSQLiteMaintenanceMinReclaimableBytes
	}
	if s.WALCheckpointThresholdBytes <= 0 {
		s.WALCheckpointThresholdBytes = DefaultSQLiteMaintenanceWALCheckpointThresholdBytes
	}
	if s.MaxPagesPerRun <= 0 {
		s.MaxPagesPerRun = DefaultSQLiteMaintenanceMaxPagesPerRun
	}
	if s.MaxDurationSeconds <= 0 {
		s.MaxDurationSeconds = DefaultSQLiteMaintenanceMaxDurationSeconds
	}
	return s
}

type Config struct {
	Server            Server            `mapstructure:"server"`
	Log               Log               `mapstructure:"log"`
	Database          Database          `mapstructure:"database"`
	SQLiteMaintenance SQLiteMaintenance `mapstructure:"sqlite_maintenance"`
}

var AppConfig Config

func Load(path string) error {
	if path != "" {
		viper.SetConfigFile(path)
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("json")
		viper.AddConfigPath("data")
	}

	viper.AutomaticEnv()
	viper.SetEnvPrefix(APP_NAME)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	setDefaults()

	if err := viper.ReadInConfig(); err == nil {
		log.Infof("Using config file: %s", viper.ConfigFileUsed())
	} else {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Infof("Config file not found, creating default config")
			if err := os.MkdirAll("data", 0755); err != nil {
				log.Errorf("Failed to create data directory: %v", err)
			}
			if err := viper.SafeWriteConfigAs("data/config.json"); err != nil {
				log.Errorf("Failed to create default config: %v", err)
			}
		} else {
			return fmt.Errorf("error reading config file: %w", err)
		}
	}

	if err := viper.Unmarshal(&AppConfig); err != nil {
		return fmt.Errorf("unable to decode config into struct: %w", err)
	}
	return nil
}

func setDefaults() {
	viper.SetDefault("server.host", "0.0.0.0")
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("database.type", "sqlite")
	viper.SetDefault("database.path", "data/data.db")
	viper.SetDefault("log.level", "info")
	viper.SetDefault("sqlite_maintenance.enabled", true)
	viper.SetDefault("sqlite_maintenance.interval_seconds", DefaultSQLiteMaintenanceIntervalSeconds)
	viper.SetDefault("sqlite_maintenance.idle_seconds", DefaultSQLiteMaintenanceIdleSeconds)
	viper.SetDefault("sqlite_maintenance.min_database_bytes", DefaultSQLiteMaintenanceMinDatabaseBytes)
	viper.SetDefault("sqlite_maintenance.min_reclaimable_bytes", DefaultSQLiteMaintenanceMinReclaimableBytes)
	viper.SetDefault("sqlite_maintenance.wal_checkpoint_threshold_bytes", DefaultSQLiteMaintenanceWALCheckpointThresholdBytes)
	viper.SetDefault("sqlite_maintenance.max_pages_per_run", DefaultSQLiteMaintenanceMaxPagesPerRun)
	viper.SetDefault("sqlite_maintenance.max_duration_seconds", DefaultSQLiteMaintenanceMaxDurationSeconds)
}
