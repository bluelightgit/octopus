package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 5,
		Up:      backfillGroupResponsesWebsocketDefaults,
	})
}

func backfillGroupResponsesWebsocketDefaults(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.Exec(`UPDATE groups SET responses_websocket_enabled = false WHERE responses_websocket_enabled IS NULL OR preferred_protocol_family IS NULL OR TRIM(preferred_protocol_family) = '' OR preferred_protocol_family != 'openai_responses'`).Error; err != nil {
		return fmt.Errorf("failed to backfill groups.responses_websocket_enabled: %w", err)
	}
	return nil
}
