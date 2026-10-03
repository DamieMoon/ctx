//go:build integration

// Migration 154 gates (PR #44 review F4): the one-time supersedes reconcile
// over the existing corpus. The runtime reconcile (ReconcileSupersedesTargets)
// only runs for a target a dream batch, a cleanup sweep or a resolve
// touches — snapshot markings that lost their supersedes link before it
// existed stay snapshots forever without this repair.
//
// One database, every pre-154 case seeded side by side, 154 applied once:
// the statements only prove their predicates in each other's presence (a
// restore that ignored the scope or archive guard would also take the move
// cases with it).
//
//	go test -tags=integration -p 1 ./internal/store/ -run TestMigration154 -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m154File = "154_supersedes_snapshot_reconcile.sql"

// m154Block inserts one block; ptr != "" marks it a snapshot superseded by ptr.
func m154Block(t *testing.T, pool *pgxpool.Pool, title, scope string, archived bool, ptr string) string {
	t.Helper()
	lifecycle := "knowledge"
	var superseded any
	if ptr != "" {
		lifecycle, superseded = "snapshot", ptr
	}
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, is_archived, lifecycle_state, superseded_by)
		 VALUES ('decisions', $1, 'content of '||$1, $2, $3, $4, $5::uuid)
		 RETURNING id::text`,
		title, scope, archived, lifecycle, superseded).Scan(&id); err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
	return id
}

func m154Link(t *testing.T, pool *pgxpool.Pool, src, tgt, rel string, confidence float64, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links
		   (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, created_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $4, 'private', $5)`,
		src, tgt, rel, confidence, createdAt); err != nil {
		t.Fatalf("seed link %s->%s: %v", src, tgt, err)
	}
}

type m154State struct {
	lifecycle string
	ptr       string // "" = NULL
	before    string // metadata->'supersedes_repair'->>'superseded_by_before', "" = absent
}

func m154Read(t *testing.T, pool *pgxpool.Pool, id string) m154State {
	t.Helper()
	var s m154State
	if err := pool.QueryRow(context.Background(),
		`SELECT lifecycle_state, COALESCE(superseded_by::text, ''),
		        COALESCE(metadata->'supersedes_repair'->>'superseded_by_before', '')
		   FROM context_blocks WHERE id = $1::uuid`, id).Scan(&s.lifecycle, &s.ptr, &s.before); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return s
}

// m154Snapshot captures every column the migration may write, for the
// idempotency and fixpoint probes.
func m154Snapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, lifecycle_state || '|' || COALESCE(superseded_by::text, '<null>') || '|' ||
		        COALESCE(metadata::text, '<null>') || '|' || updated_at::text
		   FROM context_blocks ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer rows.Close()
	snap := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatalf("snapshot row: %v", err)
		}
		snap[id] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return snap
}

func m154ApplyAgain(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := migrations.FS.ReadFile(m154File)
	if err != nil {
		t.Fatalf("read embedded %s: %v", m154File, err)
	}
	ctx := context.Background()
	if err := pgxdb.Write(ctx, pool, pgxdb.At("m154 re-apply"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, string(sql))
		return err
	}); err != nil {
		t.Fatalf("re-apply %s: %v", m154File, err)
	}
}

func TestMigration154_SupersedesSnapshotReconcile(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 153)
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	type fixture struct {
		name, target, ptrBefore string
		want                    m154State
	}
	var fx []fixture
	add := func(name, target, ptrBefore string, want m154State) {
		fx = append(fx, fixture{name, target, ptrBefore, want})
	}

	// (1) pair re-classified away from supersedes (live: 19 of 27 orphans).
	s1 := m154Block(t, pool, "m154 s1", "private", false, "")
	t1 := m154Block(t, pool, "m154 t1", "private", false, s1)
	m154Link(t, pool, s1, t1, "topical", 0.9, early)
	add("re-classified pair", t1, s1, m154State{"knowledge", "", s1})

	// (2) no link at all between pointer and target (live: 8 of 27).
	s2 := m154Block(t, pool, "m154 s2", "private", false, "")
	t2 := m154Block(t, pool, "m154 t2", "private", false, s2)
	add("no pair", t2, s2, m154State{"knowledge", "", s2})

	// (3) pointer archived, its link not yet swept, nothing else.
	s3 := m154Block(t, pool, "m154 s3", "private", true, "")
	t3 := m154Block(t, pool, "m154 t3", "private", false, s3)
	m154Link(t, pool, s3, t3, "supersedes", 0.9, early)
	add("archived pointer, sole superseder", t3, s3, m154State{"knowledge", "", s3})

	// (4) pointer archived, a valid superseder survives → move.
	s4 := m154Block(t, pool, "m154 s4", "private", true, "")
	o4 := m154Block(t, pool, "m154 o4", "private", false, "")
	t4 := m154Block(t, pool, "m154 t4", "private", false, s4)
	m154Link(t, pool, s4, t4, "supersedes", 0.9, early)
	m154Link(t, pool, o4, t4, "supersedes", 0.8, later)
	add("archived pointer, valid survivor", t4, s4, m154State{"snapshot", o4, s4})

	// (5) pointer without link, only a sub-threshold superseder left → the
	// exit hysteresis keeps the snapshot and moves the pointer.
	s5 := m154Block(t, pool, "m154 s5", "private", false, "")
	o5 := m154Block(t, pool, "m154 o5", "private", false, "")
	t5 := m154Block(t, pool, "m154 t5", "private", false, s5)
	m154Link(t, pool, o5, t5, "supersedes", 0.5, early)
	add("sub-threshold survivor", t5, s5, m154State{"snapshot", o5, s5})

	// (6) older sub-threshold vs newer valid survivor → the valid one.
	s6 := m154Block(t, pool, "m154 s6", "private", false, "")
	p6 := m154Block(t, pool, "m154 p6", "private", false, "")
	q6 := m154Block(t, pool, "m154 q6", "private", false, "")
	t6 := m154Block(t, pool, "m154 t6", "private", false, s6)
	m154Link(t, pool, p6, t6, "supersedes", 0.5, early)
	m154Link(t, pool, q6, t6, "supersedes", 0.9, later)
	add("valid survivor preferred", t6, s6, m154State{"snapshot", q6, s6})

	// (7) healthy: the pointer's own link is valid → untouched.
	s7 := m154Block(t, pool, "m154 s7", "private", false, "")
	t7 := m154Block(t, pool, "m154 t7", "private", false, s7)
	m154Link(t, pool, s7, t7, "supersedes", 0.9, early)
	add("healthy", t7, s7, m154State{"snapshot", s7, ""})

	// (8) healthy under the hysteresis: the pointer's link is below 0.7.
	s8 := m154Block(t, pool, "m154 s8", "private", false, "")
	t8 := m154Block(t, pool, "m154 t8", "private", false, s8)
	m154Link(t, pool, s8, t8, "supersedes", 0.5, early)
	add("healthy, sub-threshold pointer", t8, s8, m154State{"snapshot", s8, ""})

	// (9) pointer in another scope (legacy divergent row) → no superseder.
	s9 := m154Block(t, pool, "m154 s9", "hth", false, "")
	t9 := m154Block(t, pool, "m154 t9", "private", false, s9)
	m154Link(t, pool, s9, t9, "supersedes", 0.9, early)
	add("foreign-scope pointer", t9, s9, m154State{"knowledge", "", s9})

	// (10) knowledge target with a valid superseder: outside the repair's
	// domain (it heals snapshots, it never creates one) — the runtime
	// reconcile marks it on its next touch.
	s10 := m154Block(t, pool, "m154 s10", "private", false, "")
	t10 := m154Block(t, pool, "m154 t10", "private", false, "")
	m154Link(t, pool, s10, t10, "supersedes", 0.9, early)
	add("knowledge with superseder", t10, "", m154State{"knowledge", "", ""})

	// (11) archived snapshot target: archived blocks are out of every
	// reconcile, here too.
	s11 := m154Block(t, pool, "m154 s11", "private", false, "")
	t11 := m154Block(t, pool, "m154 t11", "private", true, s11)
	add("archived target", t11, s11, m154State{"snapshot", s11, ""})

	if err := store.RunMigrationsUpTo(ctx, pool, 154); err != nil {
		t.Fatalf("apply migration 154: %v", err)
	}
	for _, f := range fx {
		if got := m154Read(t, pool, f.target); got != f.want {
			t.Errorf("%s: got %+v, want %+v", f.name, got, f.want)
		}
	}
	if t.Failed() {
		return
	}

	// Idempotent: a second application changes no row — not even updated_at
	// or the audit marker.
	before := m154Snapshot(t, pool)
	m154ApplyAgain(t, pool)
	after := m154Snapshot(t, pool)
	for id, state := range before {
		if after[id] != state {
			t.Errorf("second application changed %s:\n  before %s\n  after  %s", id, state, after[id])
		}
	}

	// Fixpoint: the repaired state is exactly what the runtime reconcile
	// keeps — running it over every repaired snapshot target changes nothing.
	// (Knowledge targets are excluded: case 10 is the documented gap the
	// runtime closes on touch, not a disagreement.)
	var snapshots []string
	for _, f := range fx {
		if f.want.lifecycle == "snapshot" {
			snapshots = append(snapshots, f.target)
		}
	}
	if err := pgxdb.Write(ctx, pool, pgxdb.At("m154 fixpoint"), func(tx pgx.Tx) error {
		restored, err := store.ReconcileSupersedesTargets(ctx, tx, snapshots, "")
		if err == nil && len(restored) > 0 {
			t.Errorf("runtime reconcile restored %v after the repair", restored)
		}
		return err
	}); err != nil {
		t.Fatalf("runtime reconcile: %v", err)
	}
	fixpoint := m154Snapshot(t, pool)
	for id, state := range after {
		if fixpoint[id] != state {
			t.Errorf("runtime reconcile disagrees with the repair on %s:\n  repair    %s\n  reconcile %s", id, state, fixpoint[id])
		}
	}
}
