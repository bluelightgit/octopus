package cmd

import (
	"github.com/bestruirui/octopus/internal/client"
	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server"
	"github.com/bestruirui/octopus/internal/task"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/shutdown"
	"github.com/spf13/cobra"
)

var cfgFile string

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start " + conf.APP_NAME,
	PreRun: func(cmd *cobra.Command, args []string) {
		conf.PrintBanner()
		conf.Load(cfgFile)
		log.SetLevel(conf.AppConfig.Log.Level)
	},
	Run: func(cmd *cobra.Command, args []string) {
		shutdown.Init(log.Logger)
		defer shutdown.Listen()
		if err := db.InitDB(conf.AppConfig.Database.Type, conf.AppConfig.Database.Path, conf.IsDebug()); err != nil {
			log.Errorf("database init error: %v", err)
			return
		}
		shutdown.Register(db.Close)
		if status, err := db.EnsureSQLiteRuntimePragmas(cmd.Context()); err != nil {
			log.Errorf("sqlite runtime pragma check error: %v", err)
			return
		} else if status != nil {
			log.Infof("sqlite runtime status: journal_mode=%s auto_vacuum=%s(%d) wal_autocheckpoint=%d page_count=%d freelist_count=%d total_size_bytes=%d reclaimable_bytes=%d wal_size_bytes=%d",
				status.JournalMode, status.AutoVacuumMode, status.AutoVacuum, status.WALAutoCheckpoint, status.PageCount, status.FreelistCount, status.TotalSizeBytes, status.ReclaimableBytes, status.WALSizeBytes)
		}

		if err := op.InitCache(); err != nil {
			log.Errorf("cache init error: %v", err)
			return
		}
		if removed, err := op.RelayLogBodySweep(cmd.Context()); err != nil {
			log.Warnf("relay body storage sweep failed: %v", err)
		} else if removed > 0 {
			log.Infof("removed %d unreferenced relay body files at startup", removed)
		}
		client.ReloadRuntimeSettings()
		relay.ReloadRuntimeSettings()
		shutdown.Register(op.SaveCache)

		if err := op.UserInit(); err != nil {
			log.Errorf("user init error: %v", err)
			return
		}

		if err := server.Start(); err != nil {
			log.Errorf("server start error: %v", err)
			return
		}
		shutdown.Register(server.Close)

		task.Init()
		go task.RUN()
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
}
