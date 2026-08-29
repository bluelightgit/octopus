package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 7,
		Up:      backfillChannelSystemPromptRoleOverride,
	})
}

// 007: normalize the per-channel system prompt role override for existing
// databases. The field is additive; auto is the backwards-compatible default.
func backfillChannelSystemPromptRoleOverride(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	if err := db.Exec(`
UPDATE channels
SET system_prompt_role_override = 'auto'
WHERE system_prompt_role_override IS NULL
   OR TRIM(system_prompt_role_override) = ''
   OR system_prompt_role_override NOT IN ('auto', 'system', 'developer')
`).Error; err != nil {
		return fmt.Errorf("failed to backfill channels.system_prompt_role_override: %w", err)
	}
	return nil
}
