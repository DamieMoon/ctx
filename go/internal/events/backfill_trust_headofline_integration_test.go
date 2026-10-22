//go:build integration

// Rot-Gate for review finding M2 of PR #46: backfillOneEmbedding peeks the
// OLDEST pending block, and when no embed backend is trusted for that block's
// floor-adjusted sensitivity it returned (false, nil) without a memo — by
// design, so a pool outage never parks blocks (store.EmbedFailureSensitivity
// Ineligible). But the next pick peeked the SAME block again: every younger
// block waited behind it for good, a caller_timeout block (which only the
// background arm takes once the query path handed it off) included, and the
// scheduler logged one WARN per pick forever.
//
// Grün: the pick skips blocks the router's embed chain cannot serve at all —
// computed per pick from the chain, no memo written — and reaches the next
// embeddable block in the same call. A pool-wide outage (no chain even for
// public) stays the memo-free "nothing to do" it always was.
//
// Run: `go test -tags=integration ./internal/events/ -run TestBackfillTrustHeadOfLine -count=1 -v`.
package events

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedSensitiveBlock inserts one pending block with an explicit sensitivity
// and scope, `age` in the past, and returns its id.
func seedSensitiveBlock(t *testing.T, pool *pgxpool.Pool, title, sensitivity, scope string, age time.Duration) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity, created_at, updated_at)
		 VALUES ('learnings', $1, $2, $3, $4, now() - $5::interval, now())
		 RETURNING id`,
		title, "content of "+title, scope, sensitivity, age.String()).Scan(&id); err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
	return id
}

func isEmbedded(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var has bool
	if err := pool.QueryRow(context.Background(),
		`SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1`, id).Scan(&has); err != nil {
		t.Fatalf("read block %s: %v", id, err)
	}
	return has
}

func memoCount(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_embed_failures WHERE block_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("memo count %s: %v", id, err)
	}
	return n
}

func TestBackfillTrustHeadOfLine_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	srv := headOfLineEmbedServer(t)
	noCreds := embedPoolRow("embed-no-credentials", srv.URL, 100)
	noCreds.Trust = backends.TrustNoCredentials
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)

	newSched := func(t *testing.T, cfg *config.Config, rows ...backends.Backend) *Scheduler {
		t.Helper()
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest(rows)
		s := NewScheduler(pool, config.NewStore(cfg), bpool, StartupConfig{DreamEnabled: false})
		s.SetDispatcher(d)
		return s
	}
	baseCfg := func() *config.Config {
		return &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			Interval:    100 * time.Millisecond,
			MaxTokens:   0,
			BackoffBase: time.Hour,
			BackoffCap:  24 * time.Hour,
		}}
	}

	// The finding's exact shape, through the dedicated arm: the oldest block
	// is credentials, the only backend is no-credentials, and the younger
	// block carries a caller_timeout memo — the query path handed it off and
	// only the arm takes it now.
	t.Run("arm_reaches_a_caller_timeout_block_past_an_ineligible_oldest", func(t *testing.T) {
		defer clearBlocks(t, pool)
		oldID := seedSensitiveBlock(t, pool, "trust-old-credentials", "credentials", "shared", 2*time.Hour)
		youngID := seedSensitiveBlock(t, pool, "trust-young-handed-off", "personal", "shared", time.Hour)
		if err := store.RecordEmbedFailure(ctx, pool, youngID, store.EmbedFailureCallerTimeout,
			store.NormalizeEmbedError(store.EmbedFailureCallerTimeout, "context deadline exceeded"),
			time.Hour, 24*time.Hour); err != nil {
			t.Fatalf("record caller_timeout memo: %v", err)
		}

		s := newSched(t, baseCfg(), noCreds)
		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)

		deadline := time.Now().Add(5 * time.Second)
		for !isEmbedded(t, pool, youngID) && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
		}
		if !isEmbedded(t, pool, youngID) {
			t.Fatalf("younger caller_timeout block still pending after 5 s: the arm is stuck on the credentials block it cannot embed")
		}
		if isEmbedded(t, pool, oldID) {
			t.Errorf("credentials block got an embedding over a no-credentials backend — trust border crossed")
		}
		// Memo-free: the ineligible block is skipped, not parked, so a trust
		// change (or the audit lowering its sensitivity) makes it embeddable
		// on the very next pick.
		if n := memoCount(t, pool, oldID); n != 0 {
			t.Errorf("credentials block carries %d memo row(s), want 0 (the skip must stay memo-free)", n)
		}
	})

	// The same function serves the dream loop: ONE call must reach the
	// younger block, not only some later call.
	t.Run("one_call_embeds_past_the_ineligible_head", func(t *testing.T) {
		defer clearBlocks(t, pool)
		oldID := seedSensitiveBlock(t, pool, "trust-old-credentials-2", "credentials", "shared", 2*time.Hour)
		youngID := seedSensitiveBlock(t, pool, "trust-young-internal", "internal", "shared", time.Hour)

		cfg := baseCfg()
		s := newSched(t, cfg, noCreds)
		ok, err := s.backfillOneEmbedding(ctx, s.newRouter(cfg, store.GlobalScope), cfg, nil)
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
		if !ok || !isEmbedded(t, pool, youngID) {
			t.Fatalf("backfilled=%v, younger embedded=%v; want true/true in the first call", ok, isEmbedded(t, pool, youngID))
		}
		if isEmbedded(t, pool, oldID) || memoCount(t, pool, oldID) != 0 {
			t.Errorf("credentials block: embedded=%v memos=%d, want false/0", isEmbedded(t, pool, oldID), memoCount(t, pool, oldID))
		}
		// Only the ineligible block is left: an empty pick, still no memo.
		ok, err = s.backfillOneEmbedding(ctx, s.newRouter(cfg, store.GlobalScope), cfg, nil)
		if err != nil || ok {
			t.Fatalf("second call: backfilled=%v err=%v, want false/nil", ok, err)
		}
		if n := memoCount(t, pool, oldID); n != 0 {
			t.Errorf("credentials block carries %d memo row(s) after the empty pick, want 0", n)
		}
	})

	// A scope floor lifts every block of its scope: an internal block in a
	// scope floored at credentials is as unreachable as a credentials block.
	t.Run("scope_floor_lifts_a_block_out_of_reach", func(t *testing.T) {
		defer clearBlocks(t, pool)
		const floored = "trust-floored-scope"
		oldID := seedSensitiveBlock(t, pool, "trust-old-floored", "internal", floored, 2*time.Hour)
		youngID := seedSensitiveBlock(t, pool, "trust-young-plain", "internal", "shared", time.Hour)

		cfg := baseCfg()
		cfg.Pool.ScopeSensitivityFloor = config.ScopeFloor{floored: backends.SensCredentials}
		s := newSched(t, cfg, noCreds)
		ok, err := s.backfillOneEmbedding(ctx, s.newRouter(cfg, store.GlobalScope), cfg, nil)
		if err != nil || !ok {
			t.Fatalf("backfilled=%v err=%v, want true/nil (the floored block must not block the plain one)", ok, err)
		}
		if !isEmbedded(t, pool, youngID) {
			t.Errorf("plain block not embedded")
		}
		if isEmbedded(t, pool, oldID) || memoCount(t, pool, oldID) != 0 {
			t.Errorf("floored block: embedded=%v memos=%d, want false/0", isEmbedded(t, pool, oldID), memoCount(t, pool, oldID))
		}
	})

	// Design constraint (store.EmbedFailureSensitivityIneligible): a pool
	// outage — here the only embed backend disabled — must not park blocks.
	// Nothing is picked, nothing is memoized, both blocks wait for the pool.
	// Already green before the fix; it pins that the fix kept it so.
	t.Run("pool_outage_stays_memo_free", func(t *testing.T) {
		defer clearBlocks(t, pool)
		aID := seedSensitiveBlock(t, pool, "trust-outage-a", "credentials", "shared", 2*time.Hour)
		bID := seedSensitiveBlock(t, pool, "trust-outage-b", "public", "shared", time.Hour)

		disabled := noCreds
		disabled.Enabled = false
		cfg := baseCfg()
		s := newSched(t, cfg, disabled)
		ok, err := s.backfillOneEmbedding(ctx, s.newRouter(cfg, store.GlobalScope), cfg, nil)
		if err != nil || ok {
			t.Fatalf("backfilled=%v err=%v, want false/nil", ok, err)
		}
		for _, id := range []string{aID, bID} {
			if isEmbedded(t, pool, id) || memoCount(t, pool, id) != 0 {
				t.Errorf("block %s: embedded=%v memos=%d, want false/0 (an outage parks nothing)",
					id, isEmbedded(t, pool, id), memoCount(t, pool, id))
			}
		}
	})
}
