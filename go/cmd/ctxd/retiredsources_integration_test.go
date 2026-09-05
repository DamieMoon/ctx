//go:build integration

// A06-A1 (design/06 §3.4 #2, §7 A1) — the row half of the deprecation window's
// boot sweep, against a real PG18 testcontainer:
//
//	go test -tags=integration ./cmd/ctxd/ -run TestWarnRetiredSettingRowsBoot -count=1 -v
//
// The env half (cmd/ctxd/retiredsources_test.go) needs no database. This half
// does, because the property that matters cannot be faked: the sweep has to see
// rows in scopes nothing else at boot ever reads. Boot-time settings loading is
// hard-wired to '_global' (reload.go:169-171) and the tenant overlay is lazy —
// so a tenant-scope override on one of the 6 tenant-overridable api_key keys is
// invisible to every existing boot path, while being exactly the row class that
// becomes unreachable when the API starts answering 404 in every scope
// (design/06 §3.3 step 2a, §5.3).
//
// It lives in cmd/ctxd rather than internal/store because only the cmd/** layer
// may import internal/config (F1 layering, depguard), and the key list under
// test is the real config.RetiredKeyNames(), not a fixture that resembles it.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// The fixtures write context_settings rows directly — the psql shape,
// deliberately bypassing the settings API. That API has refused writes on the
// superseded keys with 409 since Stufe 1, so every row the sweep will ever meet
// in the field predates that gate or was written around it; a fixture going
// through the handler could not produce the state under test.
//
// TestWarnRetiredSettingRowsBoot is the A1 row gate: rows on retired keys are
// named with their scope and the still-open cleanup path; anything else is
// silence.
//
// Mutation probe: replace config.RetiredKeyNames() in warnRetiredSettingRowsBoot
// with a single-element slice and the tenant-scope subtest goes red.
//
// The flush-hint subtest died in β7. It pinned that a row on a
// coupled:embed-cache key ALSO named the price of the recommended DELETE — a
// process-wide embed-cache flush for all tenants. Both carriers of that tag
// (embed.host/protocol) left the registry with the tuple, and the flush they
// referred to left with them: a DELETE on a cut key changes no effective value
// and costs nothing, so the hint would have quoted a price that no longer
// exists. Rows on the cut keys still get their ordinary line — the sweep reads
// config.RetiredKeyNames(), not the registry, which the "rows are named across
// every scope" subtest keeps asserting.
func TestWarnRetiredSettingRowsBoot(t *testing.T) {
	// Silence on a clean installation, and on rows for keys that survive the
	// cut. The second half is the load-bearing one: a sweep that warned about
	// dream.parallelism would send an operator deleting live configuration.
	t.Run("no retired rows stays silent", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()

		if _, err := pool.Exec(ctx,
			`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3)`,
			"dream.parallelism", store.GlobalScope, `4`); err != nil {
			t.Fatalf("insert surviving-key row: %v", err)
		}

		buf := captureBootLog(t)
		warnRetiredSettingRowsBoot(ctx, pool)

		if out := buf.String(); out != "" {
			t.Errorf("log = %q, want silence — no row sits on a retired key", out)
		}
	})

	// The cross-scope property. Two rows, two scopes, and the tenant one is the
	// row no boot path reads today.
	t.Run("rows are named across every scope", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()

		for _, row := range []struct{ key, scope, value string }{
			{"chat.host", store.GlobalScope, `"http://legacy.example.com"`},
			{"chat.api_key", "acme", `"secret-ref-name"`},
		} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3::jsonb)`,
				row.key, row.scope, row.value); err != nil {
				t.Fatalf("insert %s/%s: %v", row.key, row.scope, err)
			}
		}

		buf := captureBootLog(t)
		warnRetiredSettingRowsBoot(ctx, pool)

		out := buf.String()
		if n := deprecationLines(out, deprecationRetiredRow); n != 2 {
			t.Fatalf("log = %q, want 2 retired_settings_row lines, got %d — one per row, in every scope", out, n)
		}
		for _, want := range []string{
			"level=WARN",
			"key=chat.host", "scope=" + store.GlobalScope,
			"key=chat.api_key", "scope=acme",
			"v5.0.0",
			// The remedy has to be EXECUTABLE against this binary. It is the
			// cut itself (E13), so the settings API answers 404 on all 29 keys
			// and the row is only reachable through SQL — scope included, or
			// the operator deletes the other tenants' rows along with it.
			`DELETE FROM context_settings WHERE key = 'chat.host' AND scope = '` + store.GlobalScope + `'`,
			`DELETE FROM context_settings WHERE key = 'chat.api_key' AND scope = 'acme'`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log = %q, want %q", out, want)
			}
		}
		// Mutation probe for the E13 alignment: reinstate the pre-v5 wording
		// ("remove it with DELETE /api/settings/<key> while this version still
		// answers") and both SQL rows above go red — that line names a route
		// this very binary 404s.
		if strings.Contains(out, "DELETE /api/settings/") {
			t.Errorf("log = %q, must not point at the settings API — it answers 404 for every retired key", out)
		}
		// The reason the API path is closed is still named, so the operator
		// does not read the SQL as a shortcut around a working endpoint.
		if !strings.Contains(out, "404") {
			t.Errorf("log = %q, want the closed API path named", out)
		}
		// No value, ever: half the retired keys are secret-class, and a resolved
		// api_key in a boot log is the leak the §3.3 log invariant forbids.
		if strings.Contains(out, "secret-ref-name") || strings.Contains(out, "legacy.example.com") {
			t.Errorf("log = %q, must not carry row VALUES", out)
		}
	})

	// A row on a CUT key is still named. The sweep's key list is
	// config.RetiredKeyNames(), which keeps all 29 names past the registry cut
	// — this is the half that would silently disappear if the sweep were ever
	// rewritten to walk the registry, and it is the class the field will
	// mostly have (rows written before the cut).
	t.Run("rows on cut keys are named too, without a flush cost", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()

		if _, err := pool.Exec(ctx,
			`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3::jsonb)`,
			"embed.host", store.GlobalScope, `"http://legacy.example.com"`); err != nil {
			t.Fatalf("insert embed.host: %v", err)
		}

		buf := captureBootLog(t)
		warnRetiredSettingRowsBoot(ctx, pool)

		out := buf.String()
		if n := deprecationLines(out, deprecationRetiredRow); n != 1 {
			t.Fatalf("log = %q, want 1 retired_settings_row line for the cut key, got %d", out, n)
		}
		if !strings.Contains(out, "key=embed.host") {
			t.Errorf("log = %q, want the cut key named", out)
		}
		// The price the hint used to quote is gone with the settings-side
		// flush; quoting it now would send an operator looking for a
		// cold-cache spike that cannot happen.
		if strings.Contains(out, "flushes the embed cache") {
			t.Errorf("log = %q, must not claim an embed-cache flush — the settings-side flush left in β7", out)
		}
	})

	// Fail-closed input guard of the store read the sweep depends on: an empty
	// key list would compile to key = ANY('{}'), match nothing, and report a
	// clean installation for a reason that has nothing to do with the data.
	t.Run("an empty key list is an error, not an empty result", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()

		if _, err := store.SettingRowsForKeys(ctx, pool, nil); err == nil {
			t.Error("SettingRowsForKeys(nil) = nil error, want a refusal — silence must not be mistakable for cleanliness")
		}
		if _, err := store.SettingRowsForKeys(ctx, pool, []string{"chat.host", ""}); err == nil {
			t.Error("SettingRowsForKeys with an empty key = nil error, want a refusal")
		}
	})
}

// TestWarnRetiredV2SettingRowsBoot is the row gate of the SECOND retirement
// vintage (config/retired.go, retiredKeysWithoutSuccessor).
//
//	go test -tags=integration ./cmd/ctxd/ -run TestWarnRetiredV2SettingRowsBoot -count=1 -v
//
// It is the vintage's only channel to a foreign installation. After the cut
// the row is invisible everywhere else: GET /api/settings is registry-driven
// and no longer lists the key, the settings build turns the override into an
// "unknown settings key" Issue on the OVERRIDE rather than a boot line about
// the row, and a tenant-scoped row is not read at boot at all. The delete
// migration of this vintage removes what exists at upgrade time; a row this
// sweep finds afterwards was written around the API.
//
// Mutation probe: drop the warnRetiredV2SettingRowsBoot call from main.go's
// boot block and the "one line per row" arm keeps passing (it calls the
// function directly) while the boot itself goes silent — which is why the
// call site is pinned in main.go by review, and why this test additionally
// asserts the two sweeps do not answer for each other below.
func TestWarnRetiredV2SettingRowsBoot(t *testing.T) {
	keys := config.RetiredV2KeyNames()
	if len(keys) == 0 {
		t.Fatal("RetiredV2KeyNames() is empty — the second vintage carries no key, so this sweep has no subject")
	}
	probe := keys[0]

	// A row on a V2 key produces exactly ONE line, and it is the V2 line: own
	// release, no successor named, executable SQL remedy with the scope in it.
	t.Run("a row on a retired V2 key is named exactly once", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()

		if _, err := pool.Exec(ctx,
			`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3::jsonb)`,
			probe, store.GlobalScope, `false`); err != nil {
			t.Fatalf("insert %s: %v", probe, err)
		}

		buf := captureBootLog(t)
		warnRetiredV2SettingRowsBoot(ctx, pool)

		out := buf.String()
		if n := deprecationLines(out, deprecationRetiredRowV2); n != 1 {
			t.Fatalf("log = %q, want exactly 1 %s line, got %d", out, deprecationRetiredRowV2, n)
		}
		if n := strings.Count(out, "level=WARN"); n != 1 {
			t.Fatalf("log = %q, want exactly 1 WARN for 1 row, got %d", out, n)
		}
		for _, want := range []string{
			"level=WARN",
			"key=" + probe,
			"scope=" + store.GlobalScope,
			retiredV2Release,
			// The remedy has to be EXECUTABLE against this binary: the key is
			// cut, so the settings API answers 404 and SQL is the only way —
			// scope included, or the operator deletes other tenants' rows too.
			`DELETE FROM context_settings WHERE key = '` + probe + `' AND scope = '` + store.GlobalScope + `'`,
			"404",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log = %q, want %q", out, want)
			}
		}
		// The two references that are FALSE for this vintage. v5.0.0 and
		// Migration 133 belong to the backend tuple; naming either would send
		// the operator to a runbook section about a pool his key never had.
		if strings.Contains(out, retiredMajor) {
			t.Errorf("log = %q, must not name %s — that is the first vintage's release", out, retiredMajor)
		}
		if strings.Contains(out, "Migration 133") {
			t.Errorf("log = %q, must not name Migration 133 — that migration does not carry this key", out)
		}
		if strings.Contains(out, "ctx backends") {
			t.Errorf("log = %q, must not point at the backend pool — this key has no successor", out)
		}
		// The other visible path for the same row is a config Issue on the
		// override ("unknown settings key", config/build.go), which is a
		// different mechanism on a different channel. If its wording ever
		// appeared HERE, the operator would read one row as two problems.
		if strings.Contains(out, "unknown settings key") {
			t.Errorf("log = %q, must not repeat the settings-build Issue — that path speaks about the override, this one about the row", out)
		}
	})

	// The vintages do not answer for each other. One row, one line, one
	// window — this is the runtime half of the list separation.
	t.Run("the vintages do not sweep each other's rows", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()

		for _, row := range []struct{ key, value string }{
			{probe, `false`},
			{"chat.host", `"http://legacy.example.com"`},
		} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3::jsonb)`,
				row.key, store.GlobalScope, row.value); err != nil {
				t.Fatalf("insert %s: %v", row.key, err)
			}
		}

		buf := captureBootLog(t)
		warnRetiredV2SettingRowsBoot(ctx, pool)
		out := buf.String()
		if n := strings.Count(out, "level=WARN"); n != 1 {
			t.Fatalf("V2 sweep log = %q, want 1 WARN (its own row only), got %d", out, n)
		}
		if strings.Contains(out, "key=chat.host") {
			t.Errorf("V2 sweep log = %q, spoke about a first-vintage row", out)
		}

		buf = captureBootLog(t)
		warnRetiredSettingRowsBoot(ctx, pool)
		out = buf.String()
		if n := strings.Count(out, "level=WARN"); n != 1 {
			t.Fatalf("V1 sweep log = %q, want 1 WARN (its own row only), got %d", out, n)
		}
		if strings.Contains(out, "key="+probe) {
			t.Errorf("V1 sweep log = %q, spoke about a second-vintage row", out)
		}
	})

	// Silence on a clean installation and on rows of surviving keys — the
	// load-bearing half: a sweep that warned about live configuration would
	// send an operator deleting it.
	t.Run("no V2 rows stays silent", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()

		if _, err := pool.Exec(ctx,
			`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3)`,
			"dream.parallelism", store.GlobalScope, `4`); err != nil {
			t.Fatalf("insert surviving-key row: %v", err)
		}

		buf := captureBootLog(t)
		warnRetiredV2SettingRowsBoot(ctx, pool)

		if out := buf.String(); out != "" {
			t.Errorf("log = %q, want silence — no row sits on a second-vintage key", out)
		}
	})

	// The cross-scope property, same as V1: the row a boot path never reads.
	// Boot settings loading is hard-wired to the global scope and the tenant
	// overlay is lazy, so a tenant-scoped row is invisible to everything else.
	t.Run("rows are named across every scope", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()

		for _, scope := range []string{store.GlobalScope, "acme"} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3::jsonb)`,
				probe, scope, `false`); err != nil {
				t.Fatalf("insert %s/%s: %v", probe, scope, err)
			}
		}

		buf := captureBootLog(t)
		warnRetiredV2SettingRowsBoot(ctx, pool)

		out := buf.String()
		if n := deprecationLines(out, deprecationRetiredRowV2); n != 2 {
			t.Fatalf("log = %q, want 2 lines — one per row, in every scope, got %d", out, n)
		}
		if !strings.Contains(out, "scope=acme") {
			t.Errorf("log = %q, want the tenant-scoped row named — it is the one no boot path reads", out)
		}
	})
}
