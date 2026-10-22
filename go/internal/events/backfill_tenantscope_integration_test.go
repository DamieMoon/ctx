//go:build integration

// Rot-Gate für den Cross-Tenant-Egress im Scheduler-Backfill (gefunden im
// Review von PR #46, 2026-10-22, per Probe reproduziert): backfillOneEmbedding
// pickte oldest-first über ALLE Scopes, löste die Chain aber mit der
// Backend-Sichtbarkeit des gerade iterierten Tenants auf (newRouter →
// Pool.Chain → VisibleTo). Ein Block von Tenant U landete so auf einem
// tenant-privaten Embed-Backend von Tenant T, sobald T an der Reihe war.
//
// Fixture: ein globales Backend (Priorität 10) und ein privates Backend von
// Tenant T (Priorität 100, wird bevorzugt, sobald sichtbar), ein pending
// Block in Scope U.
//
//	Lauf als T mit owned={T} → kein Pick, kein Wire-Kontakt auf beiden Backends
//	Lauf als U mit owned={U} → Pick, embedded über das globale Backend
//
// Run: `go test -tags=integration ./internal/events/ -run TestBackfillTenantScope_Integration -count=1 -v`.
package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillTenantScope_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	const scopeT, scopeU = "tscope-tenant-t", "tscope-tenant-u"
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, created_at, updated_at)
		 VALUES ('learnings', 'tscope-block-of-u', 'content that belongs to tenant U only', $1, 'knowledge', 'manual', now(), now())`,
		scopeU); err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer clearBlocks(t, pool)

	counting := func(inner http.Handler, hits *atomic.Int64) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			inner.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	var globalHits, privateHits atomic.Int64
	globalSrv := counting(headOfLineEmbedServer(t).Config.Handler, &globalHits)
	privateSrv := counting(headOfLineEmbedServer(t).Config.Handler, &privateHits)

	private := embedPoolRow("embed-private-t", privateSrv.URL, 100)
	private.Scope = scopeT
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		embedPoolRow("embed-global", globalSrv.URL, 10),
		private,
	})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	cfg := headOfLineCfg()

	routerT := backfillRouter(bpool, d)
	routerT.Tenant = scopeT
	ok, err := s.backfillOneEmbedding(ctx, routerT, cfg, []string{scopeT})
	if err != nil {
		t.Fatalf("tenant T cycle: unexpected error %v", err)
	}
	if ok {
		t.Fatalf("tenant T cycle: backfilled=true, want false (the only pending block belongs to tenant U)")
	}
	if g, p := globalHits.Load(), privateHits.Load(); g != 0 || p != 0 {
		t.Fatalf("tenant T cycle: wire hits global=%d private=%d, want 0/0 (U's content must not leave via T's chain)", g, p)
	}

	routerU := backfillRouter(bpool, d)
	routerU.Tenant = scopeU
	ok, err = s.backfillOneEmbedding(ctx, routerU, cfg, []string{scopeU})
	if err != nil {
		t.Fatalf("tenant U cycle: unexpected error %v", err)
	}
	if !ok {
		t.Fatalf("tenant U cycle: backfilled=false, want true")
	}
	if g, p := globalHits.Load(), privateHits.Load(); g != 1 || p != 0 {
		t.Errorf("tenant U cycle: wire hits global=%d private=%d, want 1/0 (T's private backend is invisible to U)", g, p)
	}
	var embedded bool
	if err := pool.QueryRow(ctx,
		`SELECT embedding IS NOT NULL FROM context_blocks WHERE title = 'tscope-block-of-u'`).Scan(&embedded); err != nil {
		t.Fatalf("read block: %v", err)
	}
	if !embedded {
		t.Errorf("block of tenant U is still unembedded after U's cycle")
	}
}
