//go:build integration

// Rot-Gate for the tracking issue, force-push.me/damienmoon/ctx/issues/1, scheduler half: the embed-backfill
// arm (backfillOneEmbedding) was called ONLY at the top of runDreamLoop, so
// with dream.enabled=false — the registry default — no goroutine ever
// embedded a pending block in the background. Live: a block stayed pending
// for two hours while the scheduler logged guard and digest ticks only; the
// query path was the sole embedder, inside its caller's deadline.
//
// Rot is the premise, stated against the production wiring: a scheduler
// with DreamEnabled=false starts no dream worker, and nothing else in Run
// called backfillOneEmbedding before this wave. Grün is the dedicated arm
// (runEmbedBackfillLoop): started regardless of Dream, it embeds the pending
// block within a few intervals, and a caller_timeout memo (the query path's
// hand-off) does not hold it back.
//
// Run: `go test -tags=integration ./internal/events/ -run TestEmbedBackfillArm -count=1 -v`.
package events

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestEmbedBackfillArm_RunsWithoutDream_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	srv := headOfLineEmbedServer(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.URL, 100)})
	d := dispatch.New(nil, dispatch.DefaultSettings()) // empty policy: pass-through
	t.Cleanup(d.Close)

	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		Interval:    100 * time.Millisecond,
		MaxTokens:   0,
		BackoffBase: time.Hour,
		BackoffCap:  24 * time.Hour,
	}}
	// DreamEnabled=false: the RED premise — before this wave, Run started no
	// goroutine that could reach backfillOneEmbedding in this configuration.
	s := NewScheduler(pool, config.NewStore(cfg), bpool, StartupConfig{DreamEnabled: false})
	s.SetDispatcher(d)

	waitEmbedded := func(t *testing.T, within time.Duration) bool {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if pendingCount(t, pool) == 0 {
				return true
			}
			time.Sleep(25 * time.Millisecond)
		}
		return false
	}

	t.Run("arm_embeds_a_pending_block_with_dream_off", func(t *testing.T) {
		seedPendingBlock(t, pool, "arm-pending-normal", time.Hour)
		defer clearBlocks(t, pool)

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)

		if !waitEmbedded(t, 5*time.Second) {
			t.Fatalf("block still pending after 5 s: the dedicated arm did not embed it (pending=%d)", pendingCount(t, pool))
		}
	})

	t.Run("arm_picks_a_caller_timeout_block_at_once", func(t *testing.T) {
		seedPendingBlock(t, pool, "arm-handed-off", time.Hour)
		defer clearBlocks(t, pool)

		// The query path's hand-off: a caller_timeout memo, due now.
		var blockID string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = 'arm-handed-off'`).Scan(&blockID); err != nil {
			t.Fatalf("read block id: %v", err)
		}
		if err := store.RecordEmbedFailure(ctx, pool, blockID, store.EmbedFailureCallerTimeout,
			store.NormalizeEmbedError(store.EmbedFailureCallerTimeout, "context deadline exceeded"),
			cfg.EmbedBackfill.BackoffBase, cfg.EmbedBackfill.BackoffCap); err != nil {
			t.Fatalf("record caller_timeout memo: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM context_embed_failures WHERE block_id = $1`, blockID) })

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)

		if !waitEmbedded(t, 5*time.Second) {
			t.Fatalf("caller_timeout block still pending after 5 s: the arm must pick it at once, not after a backoff")
		}
	})

	// Rebase onto 4d14549a (backfillOneEmbedding's tenant scope window): the
	// arm passes the ITERATED tenant's owned scopes, so while it iterates
	// tenant T a pending block of tenant U is neither picked nor sent to T's
	// private embed backend; once U's turn comes the block is embedded over
	// the global backend. Fixture as in TestBackfillTenantScope_Integration:
	// global backend (priority 10) plus a private backend of T (priority 100,
	// preferred whenever visible), one pending block in scope U.
	t.Run("arm_stays_inside_the_iterated_tenants_scope_window", func(t *testing.T) {
		const scopeT, scopeU = "armscope-tenant-t", "armscope-tenant-u"
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, created_at, updated_at)
			 VALUES ('learnings', 'armscope-block-of-u', 'content that belongs to tenant U only', $1, 'knowledge', 'manual', now(), now())`,
			scopeU); err != nil {
			t.Fatalf("seed: %v", err)
		}
		defer clearBlocks(t, pool)

		counting := func(hits *atomic.Int64) *httptest.Server {
			inner := headOfLineEmbedServer(t).Config.Handler
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				inner.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			return srv
		}
		var globalHits, privateHits atomic.Int64
		private := embedPoolRow("embed-private-t", counting(&privateHits).URL, 100)
		private.Scope = scopeT
		bpoolMT := backends.NewPool(nil, nil)
		bpoolMT.SeedSnapshotForTest([]backends.Backend{
			embedPoolRow("embed-global", counting(&globalHits).URL, 10),
			private,
		})

		sMT := NewScheduler(pool, config.NewStore(cfg), bpoolMT, StartupConfig{DreamEnabled: false})
		sMT.SetDispatcher(d)
		var includeU atomic.Bool
		sMT.backgroundTenantsFn = func(context.Context) []backgroundTenant {
			tenants := []backgroundTenant{{scope: scopeT, owned: []string{scopeT}}}
			if includeU.Load() {
				tenants = append(tenants, backgroundTenant{scope: scopeU, owned: []string{scopeU}})
			}
			return tenants
		}

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go sMT.runEmbedBackfillLoop(armCtx)

		// Six intervals of T-only iteration: the U block must stay untouched.
		time.Sleep(600 * time.Millisecond)
		if got := pendingCount(t, pool); got != 1 {
			t.Fatalf("pending = %d while only tenant T is iterated, want 1 (U's block is outside T's window)", got)
		}
		if g, p := globalHits.Load(), privateHits.Load(); g != 0 || p != 0 {
			t.Fatalf("wire hits global=%d private=%d while iterating T, want 0/0 (U's content must not leave via T's chain)", g, p)
		}

		includeU.Store(true)
		if !waitEmbedded(t, 5*time.Second) {
			t.Fatalf("U's block still pending 5 s after U joined the rotation")
		}
		if g, p := globalHits.Load(), privateHits.Load(); g != 1 || p != 0 {
			t.Errorf("wire hits global=%d private=%d, want 1/0 (T's private backend is invisible to U)", g, p)
		}
	})

	t.Run("interval_zero_turns_the_arm_off", func(t *testing.T) {
		seedPendingBlock(t, pool, "arm-off-pending", time.Hour)
		defer clearBlocks(t, pool)

		off := *cfg
		off.EmbedBackfill.Interval = 0
		sOff := NewScheduler(pool, config.NewStore(&off), bpool, StartupConfig{DreamEnabled: false})
		sOff.SetDispatcher(d)

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go sOff.runEmbedBackfillLoop(armCtx)

		time.Sleep(600 * time.Millisecond)
		if got := pendingCount(t, pool); got != 1 {
			t.Fatalf("pending = %d, want 1 (Interval=0 is the explicit off switch)", got)
		}
	})
}

// TestEmbedBackfillArm_Pacing_Integration is the Rot-Gate for review finding
// m3: after a block-specific memo (oversize — pre-wire estimate or HTTP 400
// exceed_context_size) backfillOneEmbedding reported "nothing backfilled"
// and the arm waited a full embed_backfill.interval before the next pick,
// although the memo had moved the block out of the way and said nothing
// about the backend. 1000 oversize blocks at the 15 s default ≈ 4 h before
// the first embeddable block behind them. Grün: a parked block lets the
// arm go on at once; a wire-class memo (backend suspect) keeps the
// interval wait, so a failing backend is not walked through the backlog.
func TestEmbedBackfillArm_Pacing_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)

	seedContent := func(t *testing.T, title, content string, age time.Duration) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
			 VALUES ('learnings', $1, $2, 'shared', now() - $3::interval, now()) RETURNING id`,
			title, content, age.String()).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
		return id
	}
	embedded := func(t *testing.T, id string) bool {
		t.Helper()
		var has bool
		if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1`, id).Scan(&has); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return has
	}
	// Interval = 1 h: any wait of the arm outlasts the test by far, so a
	// block reached within seconds was reached WITHOUT an interval wait.
	newArm := func(t *testing.T, url string, maxTokens int) *Scheduler {
		t.Helper()
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-pacing", url, 100)})
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			Interval: time.Hour, MaxTokens: maxTokens, BackoffBase: time.Hour, BackoffCap: 24 * time.Hour,
		}}
		s := NewScheduler(pool, config.NewStore(cfg), bpool, StartupConfig{DreamEnabled: false})
		s.SetDispatcher(d)
		return s
	}

	t.Run("parked_blocks_let_the_arm_go_on_at_once", func(t *testing.T) {
		defer clearBlocks(t, pool)
		long := strings.Repeat("x", 400) // ≈ 100 estimated tokens > MaxTokens 50
		for i := range 3 {
			seedContent(t, fmt.Sprintf("pacing-precheck-oversize-%d", i), long, time.Duration(5-i)*time.Hour)
		}
		// Passes the estimate, comes back HTTP 400 exceed_context_size.
		seedContent(t, "pacing-wire-oversize "+oversizeMarker, "short", 2*time.Hour)
		youngID := seedContent(t, "pacing-young", "short", time.Hour)

		s := newArm(t, headOfLineEmbedServer(t).URL, 50)
		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)

		deadline := time.Now().Add(5 * time.Second)
		for !embedded(t, youngID) && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
		}
		if !embedded(t, youngID) {
			t.Fatalf("young block still pending after 5 s behind four oversize blocks: the arm waited out the interval after a memo")
		}
		var parked int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_failures WHERE last_class = 'oversize' AND next_attempt_at = 'infinity'`).
			Scan(&parked); err != nil {
			t.Fatalf("count memos: %v", err)
		}
		if parked != 4 {
			t.Errorf("oversize memos = %d, want 4", parked)
		}
	})

	t.Run("wire_memo_keeps_the_interval_wait", func(t *testing.T) {
		defer clearBlocks(t, pool)
		var hits atomic.Int64
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(down.Close)
		seedContent(t, "pacing-down-a", "short", 2*time.Hour)
		seedContent(t, "pacing-down-b", "short", time.Hour)

		s := newArm(t, down.URL, 0)
		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)

		time.Sleep(1500 * time.Millisecond)
		if got := hits.Load(); got != 1 {
			t.Fatalf("failing backend saw %d embed calls in 1.5 s, want 1 (a wire memo must keep the interval wait)", got)
		}
	})
}

// TestEmbedBackfillArm_IntervalHotReload_Integration: embed_backfill.interval
// is a hot key, but the arm's idle wait was one time.After(interval) taken
// when the wait began — lowering an hour-long interval left the arm asleep
// for the rest of that hour. Grün: the wait runs in slices of at most
// backfillArmPoll and re-reads the key between slices.
func TestEmbedBackfillArm_IntervalHotReload_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)

	oldPoll := backfillArmPoll
	backfillArmPoll = 50 * time.Millisecond
	t.Cleanup(func() { backfillArmPoll = oldPoll })

	srv := headOfLineEmbedServer(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-hot", srv.URL, 100)})

	// Validate-clean generations (Replace runs Validate), differing only in
	// the interval: one hour, then 100 ms.
	cfgA := captureTestConfig(t, 12)
	cfgA.EmbedBackfill.Interval = time.Hour
	cfgA.EmbedBackfill.MaxTokens = 0
	cfgB := *cfgA
	cfgB.EmbedBackfill.Interval = 100 * time.Millisecond
	st := config.NewStore(cfgA)
	s := NewScheduler(pool, st, bpool, StartupConfig{DreamEnabled: false})
	s.SetDispatcher(d)

	armCtx, stop := context.WithCancel(ctx)
	defer stop()
	go s.runEmbedBackfillLoop(armCtx)

	// Nothing pending: the first pick is empty and the arm enters its
	// one-hour idle wait.
	time.Sleep(300 * time.Millisecond)
	if err := st.Replace(&cfgB); err != nil {
		t.Fatalf("store.Replace(interval=100ms): %v", err)
	}
	seedPendingBlock(t, pool, "hot-interval-pending", time.Hour)
	defer clearBlocks(t, pool)

	deadline := time.Now().Add(3 * time.Second)
	for pendingCount(t, pool) != 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got := pendingCount(t, pool); got != 0 {
		t.Fatalf("pending = %d 3 s after lowering the interval from 1 h to 100 ms: the arm is still in the old wait", got)
	}
}
