//go:build integration

// Integration test for the CALLER half of wave T04-20: the over-bound grant set
// (store.ErrTooManyBlockGrants) must reach the caller as a REFUSAL with prose,
// on every read surface — never as a quietly narrowed, scope-only answer.
//
// resolveGrants degrades a transient resolver failure to scope-only (unchanged,
// design/07 §4.1) but hands the over-bound condition UP, because that one is
// structural: degrading it would hide every granted block for as long as it
// lasts, and the caller could not tell that from "the blocks are not there".
//
// Probes:
//   - control: a grantee BELOW the bound reads normally (so a green rejection
//     below cannot come from broken wiring).
//   - HTTP: manage-get answers 500 with tooManyGrantsMsg.
//   - MCP: the recent tool answers IsError with tooManyGrantsMsg.
//
// Negative probe (documented, not automated): remove the error arm in
// resolveGrants or the mapping at a call site and both probes go red — the
// answer becomes a normal 200/success computed from a scope-only view.
//
//	go test -tags=integration ./internal/handler/ -run TestBlockGrantBoundT0420 -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// t0420HBound mirrors store.maxGrantedBlockIDs (unexported there — mechanism,
// not policy). Kept in sync with internal/store/block_grants_bound_t0420_integration_test.go.
const t0420HBound = 1024

// t0420HBulkGrants seeds n blocks in scope via pgx.CopyFrom and grants every one
// of them to granteeTenant through the PRODUCTION writer store.CreateBlockGrant
// (W10: the grants are the thing under test, so they take the real path).
func t0420HBulkGrants(t *testing.T, pool *pgxpool.Pool, scope, granteeTenant string, n int) {
	t.Helper()
	ctx := context.Background()
	rows := make([][]any, 0, n)
	for i := range n {
		title := fmt.Sprintf("t0420h-bulk-%04d", i)
		rows = append(rows, []any{"learnings", title, "content of " + title, scope})
	}
	if _, err := pool.CopyFrom(ctx,
		pgx.Identifier{"context_blocks"},
		[]string{"category", "title", "content", "scope"},
		pgx.CopyFromRows(rows)); err != nil {
		t.Fatalf("CopyFrom %d bulk blocks: %v", n, err)
	}
	q, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE scope = $1`, scope)
	if err != nil {
		t.Fatalf("read back bulk block ids: %v", err)
	}
	defer q.Close()
	seeded := 0
	for q.Next() {
		var id string
		if err := q.Scan(&id); err != nil {
			t.Fatalf("scan bulk block id: %v", err)
		}
		if _, err := store.CreateBlockGrant(ctx, pool, id, granteeTenant, ""); err != nil {
			t.Fatalf("CreateBlockGrant(%s): %v", id, err)
		}
		seeded++
	}
	if err := q.Err(); err != nil {
		t.Fatalf("read back bulk block ids: %v", err)
	}
	if seeded != n {
		t.Fatalf("granted %d blocks, want %d", seeded, n)
	}
}

func TestBlockGrantBoundT0420Mapping_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scopeOwner = "t0420h-owner"
	const scopeBulk = "t0420h-bulk"
	const scopeSmall = "t0420h-small"
	const scopeBig = "t0420h-big"
	owner := bgTenant(t, pool, "t0420h-owner")
	small := bgTenant(t, pool, "t0420h-small")
	big := bgTenant(t, pool, "t0420h-big")
	bgMapScope(t, pool, scopeOwner, owner)
	bgMapScope(t, pool, scopeBulk, owner)
	bgMapScope(t, pool, scopeSmall, small)
	bgMapScope(t, pool, scopeBig, big)

	shared := bgBlock(t, pool, scopeOwner, "t0420h-shared-block")
	if _, err := store.CreateBlockGrant(ctx, pool, shared, small, ""); err != nil {
		t.Fatalf("CreateBlockGrant(shared → small): %v", err)
	}
	t0420HBulkGrants(t, pool, scopeBulk, big, t0420HBound+1)

	arFor := func(scope, tenant string) *auth.AuthResult {
		return &auth.AuthResult{
			IsValid: true, TenantRole: auth.RoleMember,
			HomeScope: scope, ReadScopes: []string{scope}, TenantID: tenant,
		}
	}

	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
	manageGet := func(ar *auth.AuthResult, blockID string) (int, map[string]any) {
		raw, _ := json.Marshal(map[string]any{"id": blockID})
		rec := httptest.NewRecorder()
		h.handleGet(rec, httptest.NewRequest(http.MethodPost, "/api/manage", nil), ar,
			manageRequest{ID: blockID, Data: raw})
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode manage-get: %v (body %s)", err, rec.Body.String())
		}
		return rec.Code, resp
	}

	// Control: below the bound the grant arm works as before — the granted
	// foreign block is readable. Without this a green rejection below could just
	// be broken wiring.
	t.Run("below_bound_reads_normally", func(t *testing.T) {
		code, resp := manageGet(arFor(scopeSmall, small), shared)
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("manage-get below the bound = %d %v, want 200 success (the grant arm must still work)", code, resp)
		}
	})

	t.Run("http_manage_get_refuses_with_prose", func(t *testing.T) {
		code, resp := manageGet(arFor(scopeBig, big), shared)
		if code != http.StatusInternalServerError {
			t.Fatalf("manage-get over the bound = %d, want 500 (a silent scope-only 200 is the failure mode under test)", code)
		}
		if got, _ := resp["error"].(string); got != tooManyGrantsMsg {
			t.Fatalf("manage-get error = %q, want the one rejection prose %q", got, tooManyGrantsMsg)
		}
	})

	t.Run("mcp_recent_refuses_with_prose", func(t *testing.T) {
		cfg := MCPConfig{Pool: pool}
		mcpCtx := context.WithValue(context.Background(), authResultKey, arFor(scopeBig, big))
		res, _, err := mcpRecentHandler(cfg)(mcpCtx, nil, recentInput{})
		if err != nil {
			t.Fatalf("mcp recent: transport error %v, want a tool-level rejection", err)
		}
		if !res.IsError {
			t.Fatalf("mcp recent over the bound succeeded (%s), want an error result", mcpTextOf(res))
		}
		if got := mcpTextOf(res); got != tooManyGrantsMsg {
			t.Fatalf("mcp recent error = %q, want the one rejection prose %q", got, tooManyGrantsMsg)
		}
	})
}
