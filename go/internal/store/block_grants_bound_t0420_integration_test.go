//go:build integration

// Integration test R10 (Masterplan §6 / Amendment A-11) for wave T04-20: the
// per-request BOUND on the row-level grant set.
//
// Strategy A resolves every grant of a tenant into one uuid[] that is bound into
// every read path (ten visibility.Predicate sites plus ctx_rrf's
// p_granted_block_ids). design/07 §4.1/§6.3 holds that form sound only up to a
// small grant cardinality (Richtwert <~1000) — beyond it the design wants a
// semi-join or abruf-only, and neither is reachable without a migration (K1).
// So the resolver keeps the ONE form and refuses above the bound.
//
// Gates (RED before T04-20, GREEN after):
//   - below the bound: the resolved set is complete and unchanged (the
//     byte-identical arm — this is what every existing caller relies on).
//   - fail-closed probe: a grant on an ARCHIVED block never surfaces, in NO
//     state of the bound — RecentBlocks, GetBlock and the ResolveBlockID prefix
//     path. (The EgoGraph arm of the same probe is pinned by
//     block_grants_t40a_integration_test.go G3 and is not duplicated here.)
//   - above the bound: store.ErrTooManyBlockGrants and NO list. Before T04-20
//     this call returned all bound+1 ids and no error.
//   - exactly ON the bound: served completely — revoking one grant from the
//     over-bound tenant (through the production revoke path) makes the same
//     tenant readable again, so the bound is proven in both directions.
//
// Fixture doctrine (W10): the grants — the thing under test — are written
// through the PRODUCTION write path store.CreateBlockGrant/RevokeBlockGrant, so
// the FK gates and the idempotent upsert run exactly as in production. Only the
// bulk BLOCKS behind the bound+1 grants ride pgx.CopyFrom, because the store has
// no Go writer for a block that does not also want an embedding.
//
//	go test -tags=integration ./internal/store/ -run TestBlockGrantsBoundT0420 -count=1 -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// t0420Bound pins store.maxGrantedBlockIDs from OUTSIDE the package. The
// constant is unexported on purpose (mechanism, not policy — no registry key),
// so this literal is the guard: move the bound and this test fails loudly,
// which is exactly the conversation that move should trigger.
const t0420Bound = 1024

// t0420BulkBlocks seeds n blocks in one scope via pgx.CopyFrom and returns their
// ids. The ids are read back from the scope (uuidv7 defaults stay with the DB)
// — the scope is the set handle.
func t0420BulkBlocks(t *testing.T, pool *pgxpool.Pool, scope string, n int) []string {
	t.Helper()
	ctx := context.Background()
	rows := make([][]any, 0, n)
	for i := range n {
		title := fmt.Sprintf("t0420-bulk-%04d", i)
		rows = append(rows, []any{"learnings", title, "content of " + title, scope})
	}
	copied, err := pool.CopyFrom(ctx,
		pgx.Identifier{"context_blocks"},
		[]string{"category", "title", "content", "scope"},
		pgx.CopyFromRows(rows))
	if err != nil {
		t.Fatalf("CopyFrom %d bulk blocks: %v", n, err)
	}
	if copied != int64(n) {
		t.Fatalf("CopyFrom copied %d rows, want %d", copied, n)
	}
	ids := make([]string, 0, n)
	q, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE scope = $1`, scope)
	if err != nil {
		t.Fatalf("read back bulk block ids: %v", err)
	}
	defer q.Close()
	for q.Next() {
		var id string
		if err := q.Scan(&id); err != nil {
			t.Fatalf("scan bulk block id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := q.Err(); err != nil {
		t.Fatalf("read back bulk block ids: %v", err)
	}
	if len(ids) != n {
		t.Fatalf("read back %d bulk block ids, want %d", len(ids), n)
	}
	return ids
}

func TestBlockGrantsBoundT0420_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Three tenants (R10): one owner holding the blocks, one grantee below the
	// bound, one grantee above it.
	const scopeOwner = "t0420-owner"   // the two semantic blocks live here
	const scopeBulk = "t0420-bulk"     // the bound+1 filler blocks live here
	const scopeSmall = "t0420-small"   // home scope of the small grantee
	const scopeBig = "t0420-big"       // home scope of the big grantee
	ownerTenant := t40Tenant(t, pool, "t0420-owner")
	granteeSmall := t40Tenant(t, pool, "t0420-grantee-small")
	granteeBig := t40Tenant(t, pool, "t0420-grantee-big")
	t40MapScope(t, pool, scopeOwner, ownerTenant)
	t40MapScope(t, pool, scopeBulk, ownerTenant)
	t40MapScope(t, pool, scopeSmall, granteeSmall)
	t40MapScope(t, pool, scopeBig, granteeBig)

	// Two grants for the small grantee, ONE of them on an archived block.
	liveBlock := t40Block(t, pool, scopeOwner, "t0420-live-block", false)
	archivedBlock := t40Block(t, pool, scopeOwner, "t0420-archived-block", true)
	for _, b := range []string{liveBlock, archivedBlock} {
		if _, err := store.CreateBlockGrant(ctx, pool, b, granteeSmall, ""); err != nil {
			t.Fatalf("CreateBlockGrant(%s → small): %v", b, err)
		}
	}

	// bound+1 grants for the big grantee, through the same production writer.
	bulk := t0420BulkBlocks(t, pool, scopeBulk, t0420Bound+1)
	for _, b := range bulk {
		if _, err := store.CreateBlockGrant(ctx, pool, b, granteeBig, ""); err != nil {
			t.Fatalf("CreateBlockGrant(%s → big): %v", b, err)
		}
	}

	smallScopes := []string{scopeSmall}

	t.Run("below_bound_resolves_completely", func(t *testing.T) {
		got, err := store.GrantedBlockIDs(ctx, pool, granteeSmall)
		if err != nil {
			t.Fatalf("GrantedBlockIDs(small): %v", err)
		}
		want := []string{liveBlock, archivedBlock}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("GrantedBlockIDs(small) = %v, want exactly %v (the set below the bound is unchanged)", got, want)
		}
	})

	// R10 fail-closed probe: the archived granted block must be invisible in
	// EVERY read shape, while the live granted block proves the fixture is real
	// (a probe that sees nothing at all would pass for the wrong reason).
	t.Run("archived_grant_never_surfaces", func(t *testing.T) {
		grants, err := store.GrantedBlockIDs(ctx, pool, granteeSmall)
		if err != nil {
			t.Fatalf("GrantedBlockIDs(small): %v", err)
		}

		previews, err := store.RecentBlocks(ctx, pool, nil, smallScopes, "", 50, nil, nil, grants)
		if err != nil {
			t.Fatalf("RecentBlocks: %v", err)
		}
		ids := previewIDs(previews)
		if !slices.Contains(ids, liveBlock) {
			t.Fatalf("RecentBlocks = %v, want the LIVE granted block %s (fixture premise)", ids, liveBlock)
		}
		if slices.Contains(ids, archivedBlock) {
			t.Fatalf("RecentBlocks surfaced the ARCHIVED granted block %s — the grant OR-arm escaped the archived conjunct", archivedBlock)
		}

		blk, err := store.GetBlock(ctx, pool, nil, archivedBlock, smallScopes, grants)
		if err != nil {
			t.Fatalf("GetBlock(archived): %v", err)
		}
		if blk != nil {
			t.Fatalf("GetBlock returned the ARCHIVED granted block %s, want no block", archivedBlock)
		}

		// Prefix path (a FULL uuid is returned verbatim by contract and re-gated
		// in GetBlock, so only the prefix path exercises the visibility gate).
		// The prefix must be LONG: ids are uuidv7, so every block seeded in the
		// same millisecond shares the first ~12 chars — a short prefix would
		// legitimately hit the live sibling and prove nothing. 24 chars reach
		// past variant into rand_b, so the archived block is the only candidate
		// this prefix can have. The assertion is therefore "the archived id
		// never appears", stated over both return channels.
		resolved, matches, err := store.ResolveBlockID(ctx, pool, archivedBlock[:24], smallScopes, grants)
		if err != nil && !errors.Is(err, store.ErrAmbiguousID) {
			t.Fatalf("ResolveBlockID(archived prefix): %v", err)
		}
		if resolved == archivedBlock {
			t.Fatalf("ResolveBlockID resolved the ARCHIVED granted block %s", archivedBlock)
		}
		for _, m := range matches {
			if m.ID == archivedBlock {
				t.Fatalf("ResolveBlockID listed the ARCHIVED granted block %s as a candidate", archivedBlock)
			}
		}
	})

	t.Run("above_bound_is_refused_loudly", func(t *testing.T) {
		ids, err := store.GrantedBlockIDs(ctx, pool, granteeBig)
		if !errors.Is(err, store.ErrTooManyBlockGrants) {
			t.Fatalf("GrantedBlockIDs(big) err = %v (%d ids), want store.ErrTooManyBlockGrants — before T04-20 this returned all %d ids and no error",
				err, len(ids), t0420Bound+1)
		}
		if ids != nil {
			t.Fatalf("GrantedBlockIDs(big) returned %d ids alongside the error, want no list at all (a truncated set is the silent cut the bound exists to prevent)", len(ids))
		}
	})

	// The other direction of the same bound: take ONE grant away through the
	// production revoke path and the tenant is served again, completely.
	t.Run("exactly_on_bound_is_served", func(t *testing.T) {
		if err := store.RevokeBlockGrant(ctx, pool, bulk[0], granteeBig); err != nil {
			t.Fatalf("RevokeBlockGrant: %v", err)
		}
		ids, err := store.GrantedBlockIDs(ctx, pool, granteeBig)
		if err != nil {
			t.Fatalf("GrantedBlockIDs(big) at exactly the bound: %v", err)
		}
		if len(ids) != t0420Bound {
			t.Fatalf("GrantedBlockIDs(big) = %d ids, want exactly the bound %d", len(ids), t0420Bound)
		}
		if slices.Contains(ids, bulk[0]) {
			t.Fatalf("revoked grant %s still resolved", bulk[0])
		}
	})
}
