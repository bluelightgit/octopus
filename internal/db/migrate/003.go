package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 3,
		Up:      backfillGroupProtocolRoutingDefaults,
	})
}

func backfillGroupProtocolRoutingDefaults(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	if err := db.Exec(`UPDATE groups SET preferred_protocol_family = 'auto' WHERE preferred_protocol_family IS NULL OR TRIM(preferred_protocol_family) = ''`).Error; err != nil {
		return fmt.Errorf("failed to backfill groups.preferred_protocol_family: %w", err)
	}
	if err := db.Exec(`UPDATE groups SET protocol_routing_mode = 'prefer_same_protocol' WHERE protocol_routing_mode IS NULL OR TRIM(protocol_routing_mode) = ''`).Error; err != nil {
		return fmt.Errorf("failed to backfill groups.protocol_routing_mode: %w", err)
	}
	if err := db.Exec(`UPDATE groups SET responses_stateful_routing = 'auto' WHERE responses_stateful_routing IS NULL OR TRIM(responses_stateful_routing) = ''`).Error; err != nil {
		return fmt.Errorf("failed to backfill groups.responses_stateful_routing: %w", err)
	}
	return nil
}
