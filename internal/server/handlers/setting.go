package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/client"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/internal/task"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/setting").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(getSettingList),
		).
		AddRoute(
			router.NewRoute("/set", http.MethodPost).
				Use(middleware.RequireJSON()).
				Handle(setSetting),
		).
		AddRoute(
			router.NewRoute("/sqlite-status", http.MethodGet).
				Handle(getSQLiteStatus),
		).
		AddRoute(
			router.NewRoute("/sqlite-checkpoint", http.MethodPost).
				Handle(runSQLiteCheckpoint),
		).
		AddRoute(
			router.NewRoute("/export", http.MethodGet).
				Handle(exportDB),
		).
		AddRoute(
			router.NewRoute("/import", http.MethodPost).
				Handle(importDB),
		)
}

type sqliteStatusResponse struct {
	IsSQLite              bool   `json:"is_sqlite"`
	DBPath                string `json:"db_path,omitempty"`
	JournalMode           string `json:"journal_mode,omitempty"`
	AutoVacuum            int    `json:"auto_vacuum,omitempty"`
	AutoVacuumMode        string `json:"auto_vacuum_mode,omitempty"`
	WALAutoCheckpoint     int    `json:"wal_auto_checkpoint,omitempty"`
	PageCount             int    `json:"page_count,omitempty"`
	FreelistCount         int    `json:"freelist_count,omitempty"`
	WALSizeBytes          int64  `json:"wal_size_bytes,omitempty"`
	AutoVacuumNeedsVacuum bool   `json:"auto_vacuum_needs_vacuum,omitempty"`
}

type sqliteCheckpointResponse struct {
	IsSQLite            bool   `json:"is_sqlite"`
	Mode                string `json:"mode,omitempty"`
	BusyFrames          int    `json:"busy_frames,omitempty"`
	LogFrames           int    `json:"log_frames,omitempty"`
	CheckpointedFrames  int    `json:"checkpointed_frames,omitempty"`
	WALSizeBytesAfter   int64  `json:"wal_size_bytes_after,omitempty"`
	JournalModeAfter    string `json:"journal_mode_after,omitempty"`
	AutoVacuumModeAfter string `json:"auto_vacuum_mode_after,omitempty"`
}

func getSettingList(c *gin.Context) {
	settings, err := op.SettingList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, settings)
}

func setSetting(c *gin.Context) {
	var setting model.Setting
	if err := c.ShouldBindJSON(&setting); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := setting.Validate(); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.SettingSetString(setting.Key, setting.Value); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	client.ReloadRuntimeSettings()
	relay.ReloadRuntimeSettings()
	switch setting.Key {
	case model.SettingKeyModelInfoUpdateInterval:
		hours, err := strconv.Atoi(setting.Value)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		task.Update(string(setting.Key), time.Duration(hours)*time.Hour)
	case model.SettingKeySyncLLMInterval:
		hours, err := strconv.Atoi(setting.Value)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		task.Update(string(setting.Key), time.Duration(hours)*time.Hour)
	}
	resp.Success(c, setting)
}

func getSQLiteStatus(c *gin.Context) {
	if !db.IsSQLite() {
		resp.Success(c, sqliteStatusResponse{IsSQLite: false})
		return
	}

	status, err := db.InspectSQLitePragmas(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to inspect sqlite runtime status: "+err.Error())
		return
	}
	if status == nil {
		resp.Success(c, sqliteStatusResponse{IsSQLite: false})
		return
	}

	resp.Success(c, sqliteStatusResponse{
		IsSQLite:              true,
		DBPath:                status.DBPath,
		JournalMode:           status.JournalMode,
		AutoVacuum:            status.AutoVacuum,
		AutoVacuumMode:        status.AutoVacuumMode,
		WALAutoCheckpoint:     status.WALAutoCheckpoint,
		PageCount:             status.PageCount,
		FreelistCount:         status.FreelistCount,
		WALSizeBytes:          status.WALSizeBytes,
		AutoVacuumNeedsVacuum: status.AutoVacuumNeedsVacuum,
	})
}

func runSQLiteCheckpoint(c *gin.Context) {
	if !db.IsSQLite() {
		resp.Success(c, sqliteCheckpointResponse{IsSQLite: false})
		return
	}

	result, err := db.SQLiteWALCheckpoint(c.Request.Context(), db.SQLiteCheckpointModeTruncate)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to run sqlite wal checkpoint: "+err.Error())
		return
	}

	status, err := db.InspectSQLitePragmas(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to inspect sqlite runtime status after checkpoint: "+err.Error())
		return
	}
	if result == nil || status == nil {
		resp.Success(c, sqliteCheckpointResponse{IsSQLite: false})
		return
	}

	resp.Success(c, sqliteCheckpointResponse{
		IsSQLite:            true,
		Mode:                string(db.SQLiteCheckpointModeTruncate),
		BusyFrames:          result.BusyFrames,
		LogFrames:           result.LogFrames,
		CheckpointedFrames:  result.CheckpointedFrames,
		WALSizeBytesAfter:   status.WALSizeBytes,
		JournalModeAfter:    status.JournalMode,
		AutoVacuumModeAfter: status.AutoVacuumMode,
	})
}

func exportDB(c *gin.Context) {
	includeLogs, _ := strconv.ParseBool(c.DefaultQuery("include_logs", "false"))
	includeStats, _ := strconv.ParseBool(c.DefaultQuery("include_stats", "false"))

	dump, err := op.DBExportAll(c.Request.Context(), includeLogs, includeStats)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.Header("Content-Type", "application/json")
	c.Header("Content-Disposition", "attachment; filename=\"octopus-export-"+time.Now().Format("20060102150405")+".json\"")
	c.JSON(http.StatusOK, dump)
}

func importDB(c *gin.Context) {
	var dump model.DBDump

	contentType := c.GetHeader("Content-Type")
	if strings.Contains(contentType, "multipart/form-data") {
		fh, err := c.FormFile("file")
		if err != nil {
			resp.Error(c, http.StatusBadRequest, "missing upload file field 'file'")
			return
		}
		f, err := fh.Open()
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		defer f.Close()
		body, err := io.ReadAll(f)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := decodeDBDump(body, &dump); err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := decodeDBDump(body, &dump); err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
	}

	result, err := op.DBImportIncremental(c.Request.Context(), &dump)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	_ = op.InitCache()
	client.ReloadRuntimeSettings()
	relay.ReloadRuntimeSettings()

	resp.Success(c, result)
}

func decodeDBDump(body []byte, dump *model.DBDump) error {
	if dump == nil {
		return json.Unmarshal(body, &struct{}{})
	}

	if err := json.Unmarshal(body, dump); err != nil {
		return err
	}

	if dump.Version == 0 &&
		len(dump.Channels) == 0 &&
		len(dump.Groups) == 0 &&
		len(dump.GroupItems) == 0 &&
		len(dump.Settings) == 0 &&
		len(dump.APIKeys) == 0 &&
		len(dump.LLMInfos) == 0 &&
		len(dump.RelayLogs) == 0 &&
		len(dump.StatsDaily) == 0 &&
		len(dump.StatsHourly) == 0 &&
		len(dump.StatsTotal) == 0 &&
		len(dump.StatsChannel) == 0 &&
		len(dump.StatsModel) == 0 &&
		len(dump.StatsAPIKey) == 0 {
		var wrapper struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Data) > 0 {
			return json.Unmarshal(wrapper.Data, dump)
		}
	}

	return nil
}
