-- =============================================================================
-- 154_supersedes_snapshot_reconcile.sql — one-time supersedes reconcile over
-- the existing corpus
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- A dream supersedes link "A supersedes B" retires its TARGET B as
-- lifecycle_state='snapshot' with superseded_by=A. Until PR #44 the reverse
-- only ran when the link was physically deleted by the dream replace sweep and
-- only while the pointer named the deleted source: a pair RE-CLASSIFIED to
-- topical/recurrent by the upsert, a pointer whose source got archived and
-- swept, a second superseder taking over — none of these reached the target,
-- and the block stayed a snapshot with nothing superseding it. Snapshots are
-- excluded from dream linking and from the guard, and the query path serves
-- their superseder instead — an orphan is a block the system hides for good.
--
-- The runtime reconcile (store.ReconcileSupersedesTargets, PR #44 + review
-- hardening) now keeps the state consistent for every target a dream batch, a
-- cleanup sweep or a dream-link-resolve TOUCHES. A target nobody touches again
-- stays an orphan. This migration applies the same rules once to every live
-- dream snapshot (lifecycle_state='snapshot', superseded_by set, not archived):
--
--   superseder = a 'supersedes' link into the target from a source that is not
--                archived and lives in the target's scope (confidence does NOT
--                matter here — exit hysteresis, review F3);
--   (a) move:    the pointer's source is no superseder, others are → point at
--                the first one with weighted confidence >= 0.7, else at the
--                first remaining one, ordered by (created_at, source id) —
--                the runtime's choice; the target stays a snapshot;
--   (b) restore: no superseder at all → lifecycle_state='knowledge',
--                superseded_by=NULL.
--
-- Out of scope on purpose: a knowledge block that HAS a valid superseder is
-- left alone (this repair heals snapshots, it never creates one — the runtime
-- marks such a target on its next touch); archived blocks; snapshots with a
-- NULL pointer (not produced by dream). The prior lifecycle of a restored
-- block (canonical/synthesis before it became a snapshot) is not recorded
-- anywhere and cannot be recovered — restore means 'knowledge', as at runtime.
--
-- Audit: every touched row carries metadata.supersedes_repair =
-- {"migration": 154, "superseded_by_before": "<old pointer>"} — the only
-- machine-readable way back:
--   SELECT id, metadata->'supersedes_repair' FROM context_blocks
--    WHERE metadata ? 'supersedes_repair';
-- updated_at is NOT bumped: the runtime reconcile does not bump it either,
-- and the lifecycle flip is not a content change.
--
-- Idempotent: after one pass every live dream snapshot points at a
-- superseder, so (a) and (b) match zero rows on a second run.
--
-- Tx-Hinweis (Konvention 058/059/061/091/116/119/120/121): der Runner
-- (store/migrations.go) wickelt jede Migration in eine eigene Transaktion,
-- SET LOCAL ist damit selbst-revertierend.
--
-- Forward-only. Kein Schema-Objekt → test.sh table count UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '60s';

DO $$
DECLARE
    v_moved    INT;
    v_restored INT;
BEGIN
    -- (a) move: the pointer's source no longer supersedes the target, another
    --     source does. DISTINCT ON picks the runtime's candidate: valid first,
    --     then (created_at, source id).
    WITH next_pointer AS (
        SELECT DISTINCT ON (t.id)
               t.id AS target_id,
               dl.source_block_id AS source_id
          FROM context_blocks t
          JOIN context_dream_links dl
            ON dl.target_block_id = t.id
           AND dl.relationship = 'supersedes'
          JOIN context_blocks src ON src.id = dl.source_block_id
         WHERE t.lifecycle_state = 'snapshot'
           AND NOT t.is_archived
           AND t.superseded_by IS NOT NULL
           AND NOT src.is_archived
           AND src.scope = t.scope
         ORDER BY t.id, (dl.confidence >= 0.7::real) DESC, dl.created_at, dl.source_block_id
    )
    UPDATE context_blocks b
       SET superseded_by = np.source_id,
           metadata = COALESCE(b.metadata, '{}'::jsonb)
                      || jsonb_build_object('supersedes_repair',
                             jsonb_build_object('migration', 154,
                                                'superseded_by_before', b.superseded_by::text))
      FROM next_pointer np
     WHERE b.id = np.target_id
       AND NOT EXISTS (
             SELECT 1
               FROM context_dream_links dl
               JOIN context_blocks src ON src.id = dl.source_block_id
              WHERE dl.target_block_id = b.id
                AND dl.source_block_id = b.superseded_by
                AND dl.relationship = 'supersedes'
                AND NOT src.is_archived
                AND src.scope = b.scope);
    GET DIAGNOSTICS v_moved = ROW_COUNT;

    -- (b) restore: no superseder left at all.
    UPDATE context_blocks b
       SET lifecycle_state = 'knowledge',
           superseded_by = NULL,
           metadata = COALESCE(b.metadata, '{}'::jsonb)
                      || jsonb_build_object('supersedes_repair',
                             jsonb_build_object('migration', 154,
                                                'superseded_by_before', b.superseded_by::text))
     WHERE b.lifecycle_state = 'snapshot'
       AND NOT b.is_archived
       AND b.superseded_by IS NOT NULL
       AND NOT EXISTS (
             SELECT 1
               FROM context_dream_links dl
               JOIN context_blocks src ON src.id = dl.source_block_id
              WHERE dl.target_block_id = b.id
                AND dl.relationship = 'supersedes'
                AND NOT src.is_archived
                AND src.scope = b.scope);
    GET DIAGNOSTICS v_restored = ROW_COUNT;

    IF v_moved + v_restored > 0 THEN
        RAISE NOTICE 'migration 154: supersedes reconcile — % snapshot block(s) without any remaining superseder restored to knowledge, % snapshot pointer(s) moved to a remaining superseder.', v_restored, v_moved;
        RAISE NOTICE 'migration 154: the previous pointers survive in metadata — SELECT id, metadata->''supersedes_repair'' FROM context_blocks WHERE metadata ? ''supersedes_repair'';';
    END IF;
END $$;
