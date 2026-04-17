package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 6,
		Up:      backfillChannelResponsesWebsocketMaxLifetimeDefaults,
	})
}

func backfillChannelResponsesWebsocketMaxLifetimeDefaults(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.Exec(`UPDATE channels SET responses_websocket_max_lifetime_sec = 3600 WHERE responses_websocket_max_lifetime_sec IS NULL OR responses_websocket_max_lifetime_sec <= 0`).Error; err != nil {
		return fmt.Errorf("failed to backfill channels.responses_websocket_max_lifetime_sec: %w", err)
	}
	return nil
}
