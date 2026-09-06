//go:build integration

// The migration runner's four failure paths. RunMigrations is the boot path
// of every live start (cmd/ctxd/main.go:370) and, since T04-4k, it brackets
// each migration with pgxdb.Write: begin, deferred rollback, commit. Four
// texts name the stages of that bracket — "beginning transaction for
// migration N", "executing migration N (file)", "recording migration N",
// "committing migration N" — and until this file nothing in the tree asserted
// a single one of them (reports/bau/T04-4k-pruefung.md §2: zero consumers,
// zero tests). The invariant behind them was equally unguarded: a migration
// that fails leaves NO row in _migrations and NO half-applied schema, so the
// next boot runs it again instead of skipping a version that never ran.
//
// Every case drives the PRODUCTION entry point store.RunMigrations against a
// database the production runner itself built (testdb seeds through
// RunMigrationsUpTo). The failures are injected into the DATABASE — a renamed
// table, a trigger, a pool that refuses one connection — and never into the
// runner, so no case can pass by describing its own fixture.
//
//	go test -tags=integration ./internal/store/ -run TestMigrationRunnerFailurePaths -count=1 -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// The frame every failure case below stands in: the database is seeded
// through runnerCapVersion, so the FIRST migration the runner still has to
// apply is runnerAnchorVersion. That stays true however many migrations land
// later — which is what keeps this file free of per-release maintenance — and
// the anchor's content is frozen, because a landed migration is never edited
// (pre-commit gate 3). 151 earns the role for a second reason: it adds a
// column (ADD COLUMN IF NOT EXISTS rej_novelty on distill_run), so the
// database itself can answer whether the DDL of a failed transaction survived.
const (
	runnerCapVersion    = 150
	runnerAnchorVersion = 151
	runnerAnchorTable   = "distill_run"
	runnerAnchorColumn  = "rej_novelty"
)

// runnerAcquiresBeforeBegin is how many connections RunMigrations takes from
// the pool before the first BeginTx on an EMPTY database: (1) the CREATE
// TABLE IF NOT EXISTS _migrations, (2) the baseline precondition aggregate,
// (3) the EXISTS probe for the baseline version, and then (4) the BeginTx of
// the bracket. Case E blocks exactly the fourth.
const runnerAcquiresBeforeBegin = 4

// runnerAnchorFile returns the embedded filename of runnerAnchorVersion and
// verifies the assumption the whole file rests on: after runnerCapVersion the
// anchor really is the next version the runner reaches.
func runnerAnchorFile(t *testing.T) string {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}
	next, name := 0, ""
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".sql") {
			continue
		}
		v, err := strconv.Atoi(strings.SplitN(n, "_", 2)[0])
		if err != nil {
			continue
		}
		if v <= runnerCapVersion {
			continue
		}
		if next == 0 || v < next {
			next, name = v, n
		}
	}
	if next != runnerAnchorVersion {
		t.Fatalf("the first migration above %d is %d (%s), want %d — the anchor of this file moved",
			runnerCapVersion, next, name, runnerAnchorVersion)
	}
	return name
}

// runnerLedgerRows reads _migrations twice over: the versions it carries, and
// a stable text rendering used to compare a ledger against itself after a
// second pass.
func runnerLedgerRows(t *testing.T, pool *pgxpool.Pool) ([]int, string) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT version, filename, COALESCE(checksum, '<null>') FROM _migrations ORDER BY version")
	if err != nil {
		t.Fatalf("select _migrations: %v", err)
	}
	defer rows.Close()

	var versions []int
	var b strings.Builder
	for rows.Next() {
		var v int
		var filename, checksum string
		if err := rows.Scan(&v, &filename, &checksum); err != nil {
			t.Fatalf("scan _migrations row: %v", err)
		}
		versions = append(versions, v)
		fmt.Fprintf(&b, "%d\t%s\t%s\n", v, filename, checksum)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate _migrations: %v", err)
	}
	fmt.Fprintf(&b, "rows=%d\n", len(versions))
	return versions, b.String()
}

// runnerHasVersion answers the ledger question every failure case asks: did
// this version get recorded?
func runnerHasVersion(t *testing.T, pool *pgxpool.Pool, version int) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = $1)", version).Scan(&ok); err != nil {
		t.Fatalf("probe for version %d: %v", version, err)
	}
	return ok
}

// runnerHasColumn answers the schema question: did the DDL of a failed
// migration survive the rollback?
func runnerHasColumn(t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)",
		table, column).Scan(&ok); err != nil {
		t.Fatalf("probe for column %s.%s: %v", table, column, err)
	}
	return ok
}

// runnerAssertFailure is the shared verdict of cases B, C and D: the run
// failed, the error NAMES its stage and version, the driver error is still
// reachable through the wrap chain (the runner wraps with %w, pgxdb passes
// fn's error through unchanged), and the ledger carries neither a row for the
// migration that failed nor one version less than it had before.
func runnerAssertFailure(t *testing.T, pool *pgxpool.Pool, err error, stage, sqlstate string) {
	t.Helper()
	if err == nil {
		t.Fatalf("RunMigrations returned nil, want a failure labelled %q", stage)
	}
	if !strings.Contains(err.Error(), stage) {
		t.Errorf("error = %q, want it to name the stage %q", err.Error(), stage)
	}
	var pgErr *pgconn.PgError
	switch {
	case !errors.As(err, &pgErr):
		t.Errorf("error = %q, want the driver error reachable through the wrap chain", err.Error())
	case pgErr.Code != sqlstate:
		t.Errorf("SQLSTATE = %s, want %s (error: %q)", pgErr.Code, sqlstate, err.Error())
	}
	if runnerHasVersion(t, pool, runnerAnchorVersion) {
		t.Errorf("version %d is recorded although its transaction failed — the next boot would SKIP a migration that never ran",
			runnerAnchorVersion)
	}
	if !runnerHasVersion(t, pool, runnerCapVersion) {
		t.Errorf("version %d disappeared from the ledger — a failed migration took applied history with it", runnerCapVersion)
	}
}

// TestMigrationRunnerFailurePaths covers the boot path end to end: the
// success case against an empty database (A) and each of the four stages the
// bracket can fail in — executing (B), recording (C), committing (D),
// beginning (E).
func TestMigrationRunnerFailurePaths(t *testing.T) {
	ctx := context.Background()
	anchorFile := runnerAnchorFile(t)

	// A — the real boot: an empty database, the production entry point, and
	// the ledger the chain is supposed to leave behind. Its second half is
	// the property the boot path depends on every restart: running again
	// changes nothing.
	t.Run("A-boot-empty-db", func(t *testing.T) {
		// Below the fold line nothing applies on its own: versions 001-113
		// live INSIDE 113_baseline.sql, so this database starts empty.
		pool := testdb.SetupTestDBUpTo(t, migrations.BaselineVersion-1)

		if versions, dump := runnerLedgerRows(t, pool); len(versions) != 0 {
			t.Fatalf("precondition: %d rows in _migrations, want an empty database:\n%s", len(versions), dump)
		}

		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("RunMigrations against an empty database: %v", err)
		}

		got, first := runnerLedgerRows(t, pool)
		// The oracle is the chain itself: every version folded into the
		// baseline plus every embedded .sql file above the fold line.
		want := expectedMigrationRows(t)
		recorded := make(map[int]bool, len(got))
		for _, v := range got {
			recorded[v] = true
			if _, known := want[v]; !known {
				t.Errorf("version %d is recorded but not part of the chain", v)
			}
		}
		for v := range want {
			if !recorded[v] {
				t.Errorf("version %d is part of the chain but was not recorded", v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("_migrations row count = %d, want %d (folded versions + embedded files above the fold)", len(got), len(want))
		}

		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("second RunMigrations: %v", err)
		}
		if _, second := runnerLedgerRows(t, pool); second != first {
			t.Errorf("a second RunMigrations changed the ledger:\n--- first\n%s--- second\n%s", first, second)
		}

		if err := store.BackfillChecksums(ctx, pool); err != nil {
			t.Fatalf("BackfillChecksums after a fresh chain: %v", err)
		}
		if _, third := runnerLedgerRows(t, pool); third != first {
			t.Errorf("BackfillChecksums changed a ledger the runner had just stamped:\n--- before\n%s--- after\n%s", first, third)
		}
	})

	// B — "executing migration N (file)": the migration's own SQL fails.
	// The table the anchor migration alters is renamed out from under it, so
	// the failure comes from the database, not from a doctored runner.
	t.Run("B-exec-error", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, runnerCapVersion)
		if _, err := pool.Exec(ctx, "ALTER TABLE "+runnerAnchorTable+" RENAME TO "+runnerAnchorTable+"_moved"); err != nil {
			t.Fatalf("rename %s: %v", runnerAnchorTable, err)
		}

		err := store.RunMigrations(ctx, pool)
		// 42P01 = undefined_table.
		runnerAssertFailure(t, pool, err,
			fmt.Sprintf("executing migration %d (%s)", runnerAnchorVersion, anchorFile), "42P01")
	})

	// C — "recording migration N": the migration's SQL succeeded, the ledger
	// INSERT fails. A BEFORE INSERT trigger on _migrations refuses exactly
	// the anchor version, and the schema probe afterwards is the rollback
	// proof: the column the anchor adds must be gone again.
	t.Run("C-record-error", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, runnerCapVersion)
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
CREATE FUNCTION runner_block_record() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.version = %d THEN RAISE EXCEPTION 'runner test: blocked record'; END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER runner_block_record BEFORE INSERT ON _migrations
  FOR EACH ROW EXECUTE FUNCTION runner_block_record();`, runnerAnchorVersion)); err != nil {
			t.Fatalf("install record trigger: %v", err)
		}
		if runnerHasColumn(t, pool, runnerAnchorTable, runnerAnchorColumn) {
			t.Fatalf("precondition: %s.%s exists before migration %d ran",
				runnerAnchorTable, runnerAnchorColumn, runnerAnchorVersion)
		}

		err := store.RunMigrations(ctx, pool)
		// P0001 = raise_exception.
		runnerAssertFailure(t, pool, err, fmt.Sprintf("recording migration %d", runnerAnchorVersion), "P0001")
		if runnerHasColumn(t, pool, runnerAnchorTable, runnerAnchorColumn) {
			t.Errorf("%s.%s survived a failed migration — the DDL was not rolled back",
				runnerAnchorTable, runnerAnchorColumn)
		}
	})

	// D — "committing migration N": SQL and ledger INSERT both succeeded, the
	// COMMIT fails. A DEFERRABLE INITIALLY DEFERRED constraint trigger moves
	// the same refusal to commit time. Afterwards the pool must still be
	// usable: a failed commit may not leave a wedged connection behind.
	t.Run("D-commit-error", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, runnerCapVersion)
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
CREATE FUNCTION runner_block_commit() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.version = %d THEN RAISE EXCEPTION 'runner test: blocked commit'; END IF;
  RETURN NEW;
END $fn$;
CREATE CONSTRAINT TRIGGER runner_block_commit AFTER INSERT ON _migrations
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION runner_block_commit();`, runnerAnchorVersion)); err != nil {
			t.Fatalf("install commit trigger: %v", err)
		}

		err := store.RunMigrations(ctx, pool)
		runnerAssertFailure(t, pool, err, fmt.Sprintf("committing migration %d", runnerAnchorVersion), "P0001")
		if runnerHasColumn(t, pool, runnerAnchorTable, runnerAnchorColumn) {
			t.Errorf("%s.%s survived a failed commit — the transaction landed after all",
				runnerAnchorTable, runnerAnchorColumn)
		}
		var one int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Errorf("the pool is unusable after a failed commit: %v", err)
		}
	})

	// E — "beginning transaction for migration N": BeginTx itself fails, on
	// the real path through RunMigrations. The pool hands out the first three
	// connections normally (see runnerAcquiresBeforeBegin) and refuses the
	// fourth, the one the bracket opens its transaction on; BeforeConnect
	// then keeps the pool from healing itself. Nothing may be recorded.
	t.Run("E-begin-error-realpath", func(t *testing.T) {
		_, dsn := testdb.SetupTestDBUpToWithDSN(t, migrations.BaselineVersion-1)
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		var acquires atomic.Int32
		var blocked atomic.Bool
		// PrepareConn, not the deprecated BeforeAcquire: false with a nil
		// error destroys the connection and lets the pool retry on a fresh
		// one — which BeforeConnect then refuses.
		cfg.PrepareConn = func(context.Context, *pgx.Conn) (bool, error) {
			if acquires.Add(1) == runnerAcquiresBeforeBegin {
				blocked.Store(true)
				return false, nil
			}
			return true, nil
		}
		cfg.BeforeConnect = func(context.Context, *pgx.ConnConfig) error {
			if blocked.Load() {
				return errors.New("runner test: connect blocked")
			}
			return nil
		}
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("open blocked pool: %v", err)
		}
		defer pool.Close()

		err = store.RunMigrations(ctx, pool)
		if err == nil {
			t.Fatal("RunMigrations returned nil, want a failed BeginTx")
		}
		stage := fmt.Sprintf("beginning transaction for migration %d", migrations.BaselineVersion)
		if !strings.Contains(err.Error(), stage) {
			t.Errorf("error = %q, want it to name the stage %q", err.Error(), stage)
		}
		if !strings.Contains(err.Error(), "runner test: connect blocked") {
			t.Errorf("error = %q, want the pool's own error to travel with it", err.Error())
		}
		if got := acquires.Load(); got != runnerAcquiresBeforeBegin {
			t.Errorf("pool acquisitions = %d, want %d — the probe no longer lands on BeginTx (error: %q)",
				got, runnerAcquiresBeforeBegin, err.Error())
		}

		// The ledger is read through a pool of its own: the blocked one
		// cannot serve a query any more, by construction.
		check, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatalf("open verification pool: %v", err)
		}
		defer check.Close()
		if versions, dump := runnerLedgerRows(t, check); len(versions) != 0 {
			t.Errorf("a failed BeginTx left %d rows in _migrations:\n%s", len(versions), dump)
		}
	})
}
