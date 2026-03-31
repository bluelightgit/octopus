package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 4,
		Up:      backfillGroupResponsesStatefulRoutingDefaults,
	})
}

func backfillGroupResponsesStatefulRoutingDefaults(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.Exec(`UPDATE groups SET responses_stateful_routing = 'auto' WHERE responses_stateful_routing IS NULL OR TRIM(responses_stateful_routing) = ''`).Error; err != nil {
		return fmt.Errorf("failed to backfill groups.responses_stateful_routing: %w", err)
	}
	return nil
}
