//go:build integration

// Wiring gate for the dedicated embed-backfill arm (review finding m4): the
// arm tests start runEmbedBackfillLoop directly, so deleting
// `go s.runEmbedBackfillLoop(ctx)` from Run left every one of them green —
// and a deployment with dream.enabled=false back at "nothing embeds in the
// background". This test boots the WHOLE Run with DreamEnabled=false and
// asserts the end result: a pending block gets its embedding. No other arm in
// Run embeds pending blocks (the dream loop is off, the re-embed migration
// arm is inert without an active migration), so the block can only be reached
// through the arm's goroutine.
//
// RED (recorded in the wave commit): with the Run line commented out the
// block is still pending at the deadline.
//
// Run: `go test -tags=integration ./internal/events/ -run TestEmbedBackfillArm_WiredIntoRun -count=1 -v`.
package events

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
)

func TestEmbedBackfillArm_WiredIntoRun_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, dsn := testdb.SetupTestDBWithDSN(t)

	// Run starts the NOTIFY listener, whose boot backlog reloads the backend
	// pool from the database — the embed backend must be a ROW, a seeded
	// snapshot would be replaced by an empty pool.
	srv := headOfLineEmbedServer(t)
	row := embedPoolRow("embed-run-wiring", srv.URL, 100)
	row.ProviderClass = "generic" // the column's CHECK; the snapshot fixture leaves it blank
	if err := pgxdb.Write(ctx, pool, pgxdb.At("seed backend"), func(tx pgx.Tx) error {
		_, err := store.CreateBackend(ctx, tx, &row, nil)
		return err
	}); err != nil {
		t.Fatalf("seed backend row: %v", err)
	}
	bpool := backends.NewPool(pool, nil)
	if err := bpool.Reload(ctx); err != nil {
		t.Fatalf("reload backend pool: %v", err)
	}

	// The boot backlog may also REPLACE this programmatic snapshot with one
	// rebuilt from env + settings rows (see TestDistillBlockingArm); the env
	// carries the interval so both paths converge on an armed backfill.
	t.Setenv("CTX_EMBED_BACKFILL_INTERVAL", "1")
	cfg := captureTestConfig(t, 12)
	cfg.EmbedBackfill.Interval = time.Second
	cfg.EmbedBackfill.MaxTokens = 0

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	s := NewScheduler(pool, config.NewStore(cfg), bpool, StartupConfig{DSN: dsn, DreamEnabled: false})
	s.SetDispatcher(d)
	s.SetBlocktypeRegistry(blocktype.NewRegistry())

	seedPendingBlock(t, pool, "run-wiring-pending", time.Hour)

	runCtx, stop := context.WithCancel(ctx)
	defer func() { stop(); s.Wait() }()
	go s.Run(runCtx)

	deadline := time.Now().Add(30 * time.Second)
	for pendingCount(t, pool) != 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatal("context expired while waiting for the arm")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if got := pendingCount(t, pool); got != 0 {
		t.Fatalf("pending = %d 30 s after Run with DreamEnabled=false: Run does not start the embed-backfill arm", got)
	}
}
