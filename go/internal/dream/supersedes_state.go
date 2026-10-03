package dream

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"
)

const supersedesSnapshotConfidence = 0.7

// reconcileSupersedesTargets is the one entry point for the supersedes
// lifecycle side-effect inside a transaction: it runs AFTER every link write
// and stale-link delete of the transaction, so each reconcile sees the final
// link state of the transaction.
//
// Lock discipline (PR #44 hardening):
//   - All targets are locked in ONE statement, in ascending id order, BEFORE
//     the first UPDATE. Two transactions that touch overlapping target sets
//     therefore acquire them in the same order and cannot deadlock on each
//     other (the model emits links in arbitrary order; locking per link in
//     that order produced 40P01 between concurrent dream workers). Taking the
//     locks before any UPDATE also matters because every UPDATE on
//     context_blocks fires mark_guard_dirty, which locks the single
//     context_guard_state row until commit: a transaction that already holds
//     that row must not wait for a target lock afterwards.
//   - FOR NO KEY UPDATE, not FOR UPDATE: the reconcile never changes a key
//     column, and FOR UPDATE would also block the FOR KEY SHARE that every
//     concurrent INSERT into context_dream_links takes on its target for the
//     foreign-key check (55P03 under a lock_timeout).
//
// preferredSource is the writing source of a WriteLinks batch ("" for
// callers without one): a target that is not yet a snapshot and has that
// source among its valid superseders takes it as superseded_by — the
// ApplySupersedes semantics (the writer of the supersedes link becomes the
// pointer) that used to run as a separate per-link UPDATE.
func reconcileSupersedesTargets(ctx context.Context, tx pgx.Tx, targetIDs []string, preferredSource string) error {
	if len(targetIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id::text
		 FROM context_blocks
		 WHERE id = ANY($1::uuid[])
		 ORDER BY id
		 FOR NO KEY UPDATE`, targetIDs)
	if err != nil {
		return fmt.Errorf("dream: lock supersedes targets: %w", err)
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("dream: lock supersedes targets: %w", err)
	}
	for _, targetID := range locked {
		if err := reconcileSupersedesState(ctx, tx, targetID, preferredSource); err != nil {
			return err
		}
	}
	return nil
}

// supersedesCandidates returns the sources of supersedes links into targetID
// that can carry the snapshot side-effect — source not archived, source in the
// target's scope. remaining holds all of them, valid the subset at or above
// the weighted gate; both in (created_at, source id) order.
func supersedesCandidates(ctx context.Context, tx pgx.Tx, targetID, scope string) (remaining, valid []string, err error) {
	rows, err := tx.Query(ctx,
		`SELECT dl.source_block_id::text,
		        -- confidence is REAL on disk; cast the threshold to REAL so the
		        -- persisted gate uses the same representation as ApplySupersedes.
		        dl.confidence >= $2::real
		 FROM context_dream_links dl
		 JOIN context_blocks src ON src.id = dl.source_block_id
		 WHERE dl.target_block_id = $1::uuid
		   AND dl.relationship = 'supersedes'
		   AND NOT src.is_archived
		   AND src.scope = $3
		 ORDER BY dl.created_at, dl.source_block_id`,
		targetID, supersedesSnapshotConfidence, scope,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dream: reconcile supersedes candidates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sourceID string
		var isValid bool
		if err := rows.Scan(&sourceID, &isValid); err != nil {
			return nil, nil, fmt.Errorf("dream: reconcile supersedes candidate: %w", err)
		}
		remaining = append(remaining, sourceID)
		if isValid {
			valid = append(valid, sourceID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("dream: reconcile supersedes candidates: %w", err)
	}
	return remaining, valid, nil
}

// reconcileSupersedesState keeps the target lifecycle side-effect aligned with
// the current supersedes rows for that target. The confidence column stores
// the weighted confidence used by ApplySupersedes; raw_confidence remains the
// separate query-side map gate. Callers go through reconcileSupersedesTargets,
// which holds the row lock already; the lock clause here only re-asserts it.
//
// The state machine has an exit hysteresis (PR #44 review F3). The weighted
// confidence is raw × source quality × target quality, and qualities drift
// between dream cycles, so the same pair is re-written on both sides of the
// 0.7 gate; a plain "valid superseder ⇔ snapshot" rule makes the target
// flip-flop with every drift. Hence:
//   - enter: a target that is not a dream snapshot (lifecycle 'snapshot' with
//     superseded_by set) becomes one only when a VALID (≥ 0.7) superseder
//     exists;
//   - stay: a dream snapshot keeps state and pointer while its pointer's
//     source still has ANY supersedes link into it (confidence irrelevant);
//   - move: when the pointer's source no longer qualifies but others remain,
//     the pointer moves to the first valid one, else to the first remaining
//     one — the target stays a snapshot;
//   - leave: only when no supersedes link from a non-archived source of the
//     target's scope remains does the target return to knowledge/NULL.
func reconcileSupersedesState(ctx context.Context, tx pgx.Tx, targetID, preferredSource string) error {
	var lifecycle, scope string
	var supersededBy sql.NullString
	var archived bool
	if err := tx.QueryRow(ctx,
		`SELECT lifecycle_state, superseded_by::text, is_archived, scope
		 FROM context_blocks
		 WHERE id = $1::uuid
		 FOR NO KEY UPDATE`, targetID,
	).Scan(&lifecycle, &supersededBy, &archived, &scope); err != nil {
		return fmt.Errorf("dream: reconcile supersedes target: %w", err)
	}
	if archived {
		return nil
	}

	remaining, valid, err := supersedesCandidates(ctx, tx, targetID, scope)
	if err != nil {
		return err
	}

	if lifecycle == "snapshot" && supersededBy.Valid {
		pointer := supersededBy.String
		switch {
		case slices.Contains(remaining, pointer):
			return nil // stay
		case len(remaining) == 0: // leave
			if _, err := tx.Exec(ctx,
				`UPDATE context_blocks
				 SET lifecycle_state = 'knowledge', superseded_by = NULL
				 WHERE id = $1::uuid
				   AND lifecycle_state = 'snapshot'
				   AND superseded_by = $2::uuid`, targetID, pointer,
			); err != nil {
				return fmt.Errorf("dream: reconcile supersedes restore: %w", err)
			}
			return nil
		case len(valid) > 0: // move, a valid superseder first
			return markSupersededSnapshot(ctx, tx, targetID, valid[0])
		default: // move to a remaining sub-threshold superseder
			return markSupersededSnapshot(ctx, tx, targetID, remaining[0])
		}
	}

	if len(valid) == 0 {
		return nil
	}
	chosen := valid[0]
	switch {
	case lifecycle != "snapshot" && slices.Contains(valid, preferredSource):
		chosen = preferredSource
	case supersededBy.Valid && slices.Contains(valid, supersededBy.String):
		chosen = supersededBy.String
	}
	if err := markSupersededSnapshot(ctx, tx, targetID, chosen); err != nil {
		return err
	}
	if lifecycle != "snapshot" {
		slog.Info("dream: marked target block as snapshot",
			"target_block_id", targetID,
			"superseded_by_source", chosen,
		)
	}
	return nil
}

// markSupersededSnapshot sets the snapshot state with the given pointer.
func markSupersededSnapshot(ctx context.Context, tx pgx.Tx, targetID, sourceID string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE context_blocks
		 SET lifecycle_state = 'snapshot', superseded_by = $1::uuid
		 WHERE id = $2::uuid
		   AND NOT is_archived`, sourceID, targetID,
	); err != nil {
		return fmt.Errorf("dream: reconcile supersedes snapshot: %w", err)
	}
	return nil
}
