//go:build integration

// Gate of Tree-Shaking-Run wave T05-10 (design/05 §3, DECISIONS.md K32):
// migration 152 deletes the context_settings rows of the SECOND retirement
// vintage across EVERY scope, the audit trigger records each delete as an
// attributable unset, the living neighbour key is untouched, the operator
// hears about it once, and the boot sweep that used to name those rows goes
// silent afterwards.
//
//	go test -tags=integration ./cmd/ctxd/ -run TestMigration152 -count=1 -v
//
// The migration is never run against the live database from here — the whole
// gate lives on a throwaway testcontainer database capped at migration 151.
//
// It lives in cmd/ctxd for the reason settings_retire_integration_test.go
// does: only the cmd/** layer may import internal/config (F1 layering,
// depguard), and the expectation this gate holds the migration against has to
// BE config.RetiredV2KeyNames() — a fixture that merely resembles it would
// drift with the thing it is supposed to catch drifting. The set equality
// between the SQL array and that same map is a unit test next to the map
// itself (internal/config/retiredv2migration_test.go) and needs no database.
package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	retireV2MigrationVersion = 152
	retireV2MigrationFile    = "152_retire_v2_settings.sql"
	retireV2RequestID        = "migration-152-retire-v2-settings"

	// contrastKeyV2 is a LIVING key that survived the registry cut and shares
	// the `distill.` prefix with one of the two retired ones — the closest
	// neighbour an over-broad array literal could reach into. Seeded at a
	// global and a tenant scope so the over-reach has two ways to be caught.
	contrastKeyV2 = "distill.enabled"
)

// TestMigration152RetiresV2SettingRows is the wave's main gate.
func TestMigration152RetiresV2SettingRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// Capped at 151: the file under test is the next one in the chain and has
	// NOT run yet, so rows can be seeded into the pre-migration world the way
	// a foreign installation carries them.
	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, retireV2MigrationVersion-1)

	if applied := migrationApplied(t, pool, retireV2MigrationVersion); applied {
		t.Fatalf("migration %d is already recorded on a database capped at %d — the seed below would test nothing",
			retireV2MigrationVersion, retireV2MigrationVersion-1)
	}

	retired := config.RetiredV2KeyNames()
	if len(retired) == 0 {
		t.Fatal("config.RetiredV2KeyNames() is empty — this vintage carries no key, so the migration has no subject")
	}

	// Seed 1 — one _global row per retired key, programmatically from the map
	// rather than a hand-picked sample: a key missing from the migration's
	// array is exactly the drift this gate has to see.
	for _, key := range retired {
		insertSettingRow(t, pool, key, store.GlobalScope, seedValueForV2(key))
	}

	// Seed 2 — the same keys at two tenant scopes. Both were mut:"hot" and
	// therefore writable per tenant through PUT /api/settings; this is the
	// class a `WHERE scope = '_global'` filter would leave behind, invisible
	// even to the boot sweep (the full reload only ever loads '_global').
	// context_settings.scope carries no foreign key, so no tenant setup is
	// needed to make these rows real.
	tenantSeeded := 0
	for _, scope := range tenantScopes {
		for _, key := range retired {
			insertSettingRow(t, pool, key, scope, seedValueForV2(key))
			tenantSeeded++
		}
	}
	if want := len(retired) * len(tenantScopes); tenantSeeded != want {
		t.Fatalf("seeded %d tenant rows, want %d (%d keys × %d scopes)", tenantSeeded, want, len(retired), len(tenantScopes))
	}

	// Seed 3 — the contrast rows on a living key, global and tenant.
	insertSettingRow(t, pool, contrastKeyV2, store.GlobalScope, `true`)
	insertSettingRow(t, pool, contrastKeyV2, tenantScopes[0], `false`)

	wantDeleted := len(retired) + tenantSeeded
	auditBefore := countAudit(t, pool, settingEntity, "")
	if auditBefore == 0 {
		t.Fatalf("no audit rows after seeding %d settings rows — the audit trigger is not firing, and every assertion below would be vacuous", wantDeleted+2)
	}

	// Run the rest of the chain over a PRODUCTION-shaped pool: store.NewPool
	// is what cmd/ctxd builds before store.RunMigrations, and it is the thing
	// that carries a RAISE NOTICE into the process log.
	logs := captureSlog(t)
	prodPool, err := store.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open production-shaped pool: %v", err)
	}
	defer prodPool.Close()

	if err := store.RunMigrations(ctx, prodPool); err != nil {
		t.Fatalf("run migrations through %d: %v", retireV2MigrationVersion, err)
	}

	if !migrationApplied(t, pool, retireV2MigrationVersion) {
		t.Fatalf("migration %d is not recorded after RunMigrations — the file never ran and the assertions below would pass on an empty seed",
			retireV2MigrationVersion)
	}

	t.Run("every retired row is gone, in every scope", func(t *testing.T) {
		var left int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key = ANY($1)`, retired,
		).Scan(&left); err != nil {
			t.Fatalf("count surviving retired rows: %v", err)
		}
		if left != 0 {
			rows, _ := pool.Query(ctx,
				`SELECT key, scope FROM context_settings WHERE key = ANY($1) ORDER BY key, scope`, retired)
			defer rows.Close()
			var leftovers []string
			for rows.Next() {
				var k, s string
				_ = rows.Scan(&k, &s)
				leftovers = append(leftovers, k+"@"+s)
			}
			t.Errorf("%d retired row(s) survived migration %d: %v", left, retireV2MigrationVersion, leftovers)
		}
	})

	t.Run("the living neighbour keeps both of its rows", func(t *testing.T) {
		var alive int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key = $1`, contrastKeyV2,
		).Scan(&alive); err != nil {
			t.Fatalf("count contrast rows: %v", err)
		}
		if alive != 2 {
			t.Errorf("%s has %d row(s) after the migration, want 2 — the array literal reaches into a LIVING key", contrastKeyV2, alive)
		}
	})

	t.Run("each delete left an attributable unset in the audit", func(t *testing.T) {
		marked := countAudit(t, pool, settingEntity, retireV2RequestID)
		if marked != wantDeleted {
			t.Errorf("audit rows carrying request_id %q = %d, want %d (one per deleted row)", retireV2RequestID, marked, wantDeleted)
		}

		var bad int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM context_settings_audit
			 WHERE metadata->>'request_id' = $1
			   AND NOT (entity_type = 'setting'
			            AND action = 'unset'
			            AND metadata->>'via' = 'sql'
			            AND api_key_id IS NULL
			            AND old_value IS NOT NULL)`, retireV2RequestID,
		).Scan(&bad); err != nil {
			t.Fatalf("inspect marked audit rows: %v", err)
		}
		if bad != 0 {
			t.Errorf("%d marked audit row(s) are not a plain SQL-side unset with the deleted value attached — the recovery query in the runbook would return junk", bad)
		}

		// Scope fidelity: the audit has to name the scope each row lived in,
		// otherwise a tenant value cannot be restored to the right place.
		for _, scope := range append([]string{store.GlobalScope}, tenantScopes...) {
			var n int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM context_settings_audit
				 WHERE metadata->>'request_id' = $1 AND scope = $2`, retireV2RequestID, scope,
			).Scan(&n); err != nil {
				t.Fatalf("count marked audit rows for scope %s: %v", scope, err)
			}
			if n != len(retired) {
				t.Errorf("scope %q: %d marked audit row(s), want %d", scope, n, len(retired))
			}
		}

		// Append-only for the settings history. Narrowed to entity_type =
		// 'setting' on purpose: context_settings_audit is shared with the
		// block-type trigger, and RunMigrations applies the whole remaining
		// chain, so foreign rows may legitimately appear.
		total := countAudit(t, pool, settingEntity, "")
		if total != auditBefore+wantDeleted {
			t.Errorf("audit rows for entity_type=%q = %d, want %d (%d before + %d unsets) — append-only was violated",
				settingEntity, total, auditBefore+wantDeleted, auditBefore, wantDeleted)
		}
	})

	t.Run("the operator is told, in this vintage's own words", func(t *testing.T) {
		out := logs.String()
		if !strings.Contains(out, fmt.Sprintf("deleted %d context_settings row(s)", wantDeleted)) {
			t.Errorf("the boot log does not name the number of deleted rows (%d). Log:\n%s", wantDeleted, out)
		}
		if !strings.Contains(out, retireV2RequestID) {
			t.Errorf("the boot log does not name the audit marker %q, so the operator cannot find the deleted values. Log:\n%s", retireV2RequestID, out)
		}
		if !strings.Contains(out, retiredV2Release) {
			t.Errorf("the boot log does not name %s, the release these keys disappear in. Log:\n%s", retiredV2Release, out)
		}
		// The two references that are FALSE for this vintage — same negatives
		// the boot sweep holds (TestWarnRetiredV2SettingRowsBoot). Naming
		// either would send the operator to a runbook section about a pool
		// his key never had.
		if strings.Contains(out, retireHopHint) {
			t.Errorf("the boot log carries the %q hop hint — that belongs to the backend tuple, not to a key without a successor. Log:\n%s", retireHopHint, out)
		}
		if strings.Contains(out, retiredMajor) {
			t.Errorf("the boot log names %s — that is the first vintage's release. Log:\n%s", retiredMajor, out)
		}
	})

	t.Run("the boot row sweep has nothing left to name", func(t *testing.T) {
		// The sweep is this vintage's channel to a foreign installation, and
		// the migration is what closes the channel: cmd/ctxd runs
		// store.RunMigrations before warnRetiredV2SettingRowsBoot, so on the
		// upgrade boot the sweep already looks at a cleaned table.
		buf := captureBootLog(t)
		warnRetiredV2SettingRowsBoot(ctx, pool)
		out := buf.String()
		if n := deprecationLines(out, deprecationRetiredRowV2); n != 0 {
			t.Errorf("the sweep still names %d row(s) after the migration ran. Log:\n%s", n, out)
		}
		if n := strings.Count(out, "level=WARN"); n != 0 {
			t.Errorf("the sweep logged %d WARN(s) on a cleaned database. Log:\n%s", n, out)
		}
	})

	t.Run("a second application changes and says nothing", func(t *testing.T) {
		// Unfiltered on purpose: nothing but the re-applied file runs here, so
		// a re-run must not append a row of ANY entity.
		auditBeforeRerun := countAudit(t, pool, "", "")
		logs.reset()

		// RunMigrations cannot serve here: version 152 is recorded now and
		// would be skipped, which tests the runner's bookkeeping instead of
		// the statement's idempotency.
		applyRetireV2MigrationAgain(t, prodPool)

		if got := countAudit(t, pool, "", ""); got != auditBeforeRerun {
			t.Errorf("audit grew from %d to %d on the second application — the DELETE is not a no-op on an already-clean database", auditBeforeRerun, got)
		}
		if out := logs.String(); strings.Contains(out, retireV2RequestID) {
			t.Errorf("the second application still speaks to the operator — the notice is not gated on rows actually deleted. Log:\n%s", out)
		}
		var alive int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_settings WHERE key = $1`, contrastKeyV2).Scan(&alive); err != nil {
			t.Fatalf("re-count contrast rows: %v", err)
		}
		if alive != 2 {
			t.Errorf("%s lost rows on the second application (%d left, want 2)", contrastKeyV2, alive)
		}
	})
}

// TestMigration152OnAnInstallationWithoutSuchRows is the negative half, and it
// is the case that actually ships: an installation that never wrote either key
// — every fresh install, and the maintainer's own instance (row sweep 0 on
// 2026-09-05). It must lose nothing and, above all, say nothing: a migration
// that greets every upgrade with a deletion notice about rows it did not find
// teaches operators to skip the notices that matter.
func TestMigration152OnAnInstallationWithoutSuchRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, retireV2MigrationVersion-1)

	// Only the living neighbour is seeded — the retired keys are absent, the
	// way they are on an installation that never set them.
	insertSettingRow(t, pool, contrastKeyV2, store.GlobalScope, `true`)
	auditBefore := countAudit(t, pool, settingEntity, "")

	logs := captureSlog(t)
	prodPool, err := store.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open production-shaped pool: %v", err)
	}
	defer prodPool.Close()

	if err := store.RunMigrations(ctx, prodPool); err != nil {
		t.Fatalf("run migrations through %d: %v", retireV2MigrationVersion, err)
	}

	if marked := countAudit(t, pool, "", retireV2RequestID); marked != 0 {
		t.Errorf("%d audit row(s) carry request_id %q on a database that held no such row — the migration deleted something it should not see",
			marked, retireV2RequestID)
	}
	if got := countAudit(t, pool, settingEntity, ""); got != auditBefore {
		t.Errorf("settings audit grew from %d to %d — the migration touched context_settings on an installation without retired rows", auditBefore, got)
	}
	if out := logs.String(); strings.Contains(out, retireV2RequestID) {
		t.Errorf("the migration spoke to an operator who has nothing to act on. Log:\n%s", out)
	}

	var alive int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_settings WHERE key = $1`, contrastKeyV2).Scan(&alive); err != nil {
		t.Fatalf("count contrast rows: %v", err)
	}
	if alive != 1 {
		t.Errorf("%s has %d row(s), want 1", contrastKeyV2, alive)
	}
}

// seedValueForV2 gives each key of the second vintage a plausible pre-cut
// value in its registered type: distill.local_only was a bool, and
// root_map.label_budget an int. The shape matters because the audit assertion
// reads old_value back — a value the settings API would have rejected would
// make the recovery path this gate claims to protect untestable.
func seedValueForV2(key string) string {
	switch key {
	case "root_map.label_budget":
		return `64`
	default:
		return `true`
	}
}

// applyRetireV2MigrationAgain re-executes the embedded file in its own
// transaction, mirroring what the runner does — the SET LOCAL statements the
// file opens with are only meaningful inside one.
func applyRetireV2MigrationAgain(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	sql, err := migrations.FS.ReadFile(retireV2MigrationFile)
	if err != nil {
		t.Fatalf("read %s: %v", retireV2MigrationFile, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin re-apply tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("re-apply %s: %v", retireV2MigrationFile, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit re-apply: %v", err)
	}
}
