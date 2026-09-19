package dream

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const supersedesSnapshotConfidence = 0.7

// reconcileSupersedesState keeps the target lifecycle side-effect aligned with
// the current high-confidence supersedes rows for that target. The confidence
// column stores the weighted confidence used by ApplySupersedes; raw_confidence
// remains the separate query-side map gate.
func reconcileSupersedesState(ctx context.Context, tx pgx.Tx, targetID string) error {
	var lifecycle, scope string
	var supersededBy sql.NullString
	var archived bool
	if err := tx.QueryRow(ctx,
		`SELECT lifecycle_state, superseded_by::text, is_archived, scope
		 FROM context_blocks
		 WHERE id = $1::uuid
		 FOR UPDATE`, targetID,
	).Scan(&lifecycle, &supersededBy, &archived, &scope); err != nil {
		return fmt.Errorf("dream: reconcile supersedes target: %w", err)
	}
	if archived {
		return nil
	}

	rows, err := tx.Query(ctx,
		`SELECT dl.source_block_id::text
		 FROM context_dream_links dl
		 JOIN context_blocks src ON src.id = dl.source_block_id
		 WHERE dl.target_block_id = $1::uuid
		   AND dl.relationship = 'supersedes'
		   -- confidence is REAL on disk; cast the threshold to REAL so the
		   -- persisted gate uses the same representation as ApplySupersedes.
		   AND dl.confidence >= $2::real
		   AND NOT src.is_archived
		   AND src.scope = $3
		 ORDER BY dl.created_at, dl.source_block_id`,
		targetID, supersedesSnapshotConfidence, scope,
	)
	if err != nil {
		return fmt.Errorf("dream: reconcile supersedes candidates: %w", err)
	}
	defer rows.Close()

	var valid []string
	for rows.Next() {
		var sourceID string
		if err := rows.Scan(&sourceID); err != nil {
			return fmt.Errorf("dream: reconcile supersedes candidate: %w", err)
		}
		valid = append(valid, sourceID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dream: reconcile supersedes candidates: %w", err)
	}

	if len(valid) == 0 {
		if lifecycle == "snapshot" && supersededBy.Valid {
			if _, err := tx.Exec(ctx,
				`UPDATE context_blocks
				 SET lifecycle_state = 'knowledge', superseded_by = NULL
				 WHERE id = $1::uuid
				   AND lifecycle_state = 'snapshot'
				   AND superseded_by IS NOT NULL`, targetID,
			); err != nil {
				return fmt.Errorf("dream: reconcile supersedes restore: %w", err)
			}
		}
		return nil
	}

	chosen := valid[0]
	if supersededBy.Valid {
		for _, sourceID := range valid {
			if sourceID == supersededBy.String {
				chosen = sourceID
				break
			}
		}
	}

	if lifecycle != "snapshot" || !supersededBy.Valid || supersededBy.String != chosen {
		if _, err := tx.Exec(ctx,
			`UPDATE context_blocks
			 SET lifecycle_state = 'snapshot', superseded_by = $1::uuid
			 WHERE id = $2::uuid
			   AND NOT is_archived`, chosen, targetID,
		); err != nil {
			return fmt.Errorf("dream: reconcile supersedes snapshot: %w", err)
		}
	}
	return nil
}
