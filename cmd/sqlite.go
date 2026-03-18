package cmd

import (
	"context"

	"github.com/bestruirui/octopus/internal/conf"
	internaldb "github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/spf13/cobra"
)

var sqliteCmd = &cobra.Command{
	Use:   "sqlite",
	Short: "SQLite maintenance commands",
}

var sqliteCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check SQLite runtime pragmas and file status",
	PreRun: func(cmd *cobra.Command, args []string) {
		conf.Load(cfgFile)
		log.SetLevel(conf.AppConfig.Log.Level)
	},
	Run: func(cmd *cobra.Command, args []string) {
		if err := internaldb.InitDB(conf.AppConfig.Database.Type, conf.AppConfig.Database.Path, conf.IsDebug()); err != nil {
			log.Errorf("database init error: %v", err)
			return
		}
		defer func() { _ = internaldb.Close() }()

		status, err := internaldb.InspectSQLitePragmas(context.Background())
		if err != nil {
			log.Errorf("sqlite inspect error: %v", err)
			return
		}
		if status == nil {
			log.Infof("database is not sqlite")
			return
		}
		log.Infof("sqlite status: path=%s journal_mode=%s auto_vacuum=%s(%d) wal_autocheckpoint=%d page_count=%d freelist_count=%d wal_size_bytes=%d needs_repair=%t",
			status.DBPath, status.JournalMode, status.AutoVacuumMode, status.AutoVacuum, status.WALAutoCheckpoint, status.PageCount, status.FreelistCount, status.WALSizeBytes, status.AutoVacuumNeedsVacuum)
	},
}

var sqliteRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Repair SQLite auto_vacuum and journal_mode settings",
	Long:  "Repair SQLite by switching to incremental auto_vacuum and restoring WAL mode. Run this command while the service is stopped.",
	PreRun: func(cmd *cobra.Command, args []string) {
		conf.Load(cfgFile)
		log.SetLevel(conf.AppConfig.Log.Level)
	},
	Run: func(cmd *cobra.Command, args []string) {
		if err := internaldb.InitDB(conf.AppConfig.Database.Type, conf.AppConfig.Database.Path, conf.IsDebug()); err != nil {
			log.Errorf("database init error: %v", err)
			return
		}
		defer func() { _ = internaldb.Close() }()

		status, err := internaldb.RepairSQLiteAutoVacuum(context.Background())
		if err != nil {
			log.Errorf("sqlite repair error: %v", err)
			return
		}
		log.Infof("sqlite repair completed: path=%s journal_mode=%s auto_vacuum=%s(%d) wal_autocheckpoint=%d page_count=%d freelist_count=%d wal_size_bytes=%d",
			status.DBPath, status.JournalMode, status.AutoVacuumMode, status.AutoVacuum, status.WALAutoCheckpoint, status.PageCount, status.FreelistCount, status.WALSizeBytes)
	},
}

func init() {
	sqliteCmd.AddCommand(sqliteCheckCmd)
	sqliteCmd.AddCommand(sqliteRepairCmd)
	rootCmd.AddCommand(sqliteCmd)
}
