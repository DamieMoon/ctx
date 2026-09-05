//go:build integration

// Integration test for wave T03-9: the two search transports answer from ONE
// rule set (internal/handler/search_core.go).
//
// Two rules changed with the wave, and both are checked here on BOTH surfaces:
//
//   - GRANTS (E03-1 = A). /api/search passed a literal nil for the grant set
//     while the MCP tool resolved it (T40a wired four paths; this route was not
//     one of them). A block a tenant had been GRANTED was therefore readable
//     through the model's door and invisible through the operator's — one
//     authorisation answer per transport for one block. R10 half: a granted
//     ARCHIVED block stays invisible on both, because the archive predicate is
//     mandatory and sits ahead of the scope/grant OR (store/blocks.go).
//   - ALIAS (E03-3 = A). The MCP search tool dropped block_roles_exclude: the
//     same body that narrowed a REST search came back unfiltered from the tool,
//     with no error — more rows than the caller asked for.
//
// The over-bound probes are the guard on the sentinel translation: the core
// hands store.ErrTooManyBlockGrants up untouched and each transport renders it
// in its own envelope (REST 500 + prose, MCP errResult + prose). Remove either
// arm and the answer silently becomes a scope-only 200/success (T04-20).
//
//	go test -tags=integration ./internal/handler/ -run TestSearchOneRuleSetT039 -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// t039Typed seeds one block with an explicit type name (the alias probe needs a
// type it can exclude without emptying the corpus).
func t039Typed(t *testing.T, pool *pgxpool.Pool, scope, title, typeName string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name)
		 VALUES ('learnings', $1, 'content of ' || $1, $2, $3) RETURNING id::text`,
		title, scope, typeName).Scan(&id); err != nil {
		t.Fatalf("seed typed block %s: %v", title, err)
	}
	return id
}

// t039Archive archives a seeded block in place.
func t039Archive(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("archive %s: %v", id, err)
	}
}

func TestSearchOneRuleSetT039_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scopeOwner = "t039-owner" // foreign scope, holds the granted blocks
	const scopeHome = "t039-home"   // the grantee's own scope
	const scopeBulk = "t039-bulk"   // owner-side blocks for the over-bound probe
	const scopeBig = "t039-big"     // the over-bound tenant's own scope
	owner := bgTenant(t, pool, "t039-owner")
	grantee := bgTenant(t, pool, "t039-grantee")
	big := bgTenant(t, pool, "t039-big")
	bgMapScope(t, pool, scopeOwner, owner)
	bgMapScope(t, pool, scopeBulk, owner)
	bgMapScope(t, pool, scopeHome, grantee)
	bgMapScope(t, pool, scopeBig, big)

	granted := bgBlock(t, pool, scopeOwner, "t039-granted-visible")
	archived := bgBlock(t, pool, scopeOwner, "t039-granted-archived")
	t039Archive(t, pool, archived)
	ungranted := bgBlock(t, pool, scopeOwner, "t039-ungranted")
	bgGrant(t, pool, granted, grantee)
	bgGrant(t, pool, archived, grantee)

	own := bgBlock(t, pool, scopeHome, "t039-own-knowledge")
	audit := t039Typed(t, pool, scopeHome, "t039-own-audit", "audit-trail")

	// The over-bound tenant: more grants than the per-request bound (T04-20).
	t0420HBulkGrants(t, pool, scopeBulk, big, t0420HBound+1)

	arFor := func(scope, tenant string) *auth.AuthResult {
		return &auth.AuthResult{
			IsValid: true, TenantRole: auth.RoleMember,
			HomeScope: scope, ReadScopes: []string{scope}, TenantID: tenant,
		}
	}

	// REST transport. A zero config snapshot means: read rate limit 0 (off) and
	// cluster.facet_enabled false (the C6 dark state) — the two values this
	// route takes from its ONE snapshot per request.
	h := NewSearchHandler(pool, staticConfigStore{cfg: &config.Config{}}, nil)
	restSearch := func(ar *auth.AuthResult, body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader([]byte(body)))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleSearch(rec, req)
		return rec.Code, rec.Body.String()
	}
	// MCP transport. cfg.Cfg stays nil: unconfigured reads as "facet off", the
	// fail-closed direction for a dark feature.
	mcpSearch := func(ar *auth.AuthResult, in searchInput) (bool, string) {
		res, _, err := mcpSearchHandler(MCPConfig{Pool: pool})(
			context.WithValue(ctx, authResultKey, ar), nil, in)
		if err != nil {
			t.Fatalf("mcp search: transport error %v, want a tool-level result", err)
		}
		return res.IsError, mcpTextOf(res)
	}

	granteeAR := arFor(scopeHome, grantee)

	t.Run("rest_search_sees_the_granted_foreign_block", func(t *testing.T) {
		code, body := restSearch(granteeAR, `{"limit":50}`)
		if code != http.StatusOK {
			t.Fatalf("REST search = %d, want 200 (body %s)", code, body)
		}
		if !strings.Contains(body, granted) {
			t.Fatalf("REST search does NOT show the granted block %s — this is the E03-1 = A change (body %s)", granted, body)
		}
		if !strings.Contains(body, own) {
			t.Fatalf("REST search lost the caller's own block %s (body %s)", own, body)
		}
	})

	t.Run("mcp_search_sees_the_granted_foreign_block", func(t *testing.T) {
		isErr, body := mcpSearch(granteeAR, searchInput{Limit: 50})
		if isErr || !strings.Contains(body, granted) {
			t.Fatalf("MCP search lost the granted block %s (isError=%t, body %s)", granted, isErr, body)
		}
	})

	t.Run("neither_surface_shows_the_archived_grant", func(t *testing.T) {
		_, restBody := restSearch(granteeAR, `{"limit":50}`)
		_, mcpBody := mcpSearch(granteeAR, searchInput{Limit: 50})
		if strings.Contains(restBody, archived) {
			t.Errorf("REST search surfaced the ARCHIVED granted block %s (fail-closed broken)", archived)
		}
		if strings.Contains(mcpBody, archived) {
			t.Errorf("MCP search surfaced the ARCHIVED granted block %s (fail-closed broken)", archived)
		}
	})

	t.Run("neither_surface_shows_the_ungranted_foreign_block", func(t *testing.T) {
		_, restBody := restSearch(granteeAR, `{"limit":50}`)
		_, mcpBody := mcpSearch(granteeAR, searchInput{Limit: 50})
		if strings.Contains(restBody, ungranted) {
			t.Errorf("REST search leaked the ungranted foreign block %s", ungranted)
		}
		if strings.Contains(mcpBody, ungranted) {
			t.Errorf("MCP search leaked the ungranted foreign block %s", ungranted)
		}
	})

	t.Run("mcp_search_honours_the_legacy_alias", func(t *testing.T) {
		// Control: without any exclude the audit block is in the answer.
		if _, body := mcpSearch(granteeAR, searchInput{Limit: 50}); !strings.Contains(body, audit) {
			t.Fatalf("control: MCP search does not show %s at all (body %s)", audit, body)
		}
		_, viaAlias := mcpSearch(granteeAR, searchInput{Limit: 50, BlockRolesExclude: []string{"audit-trail"}})
		if strings.Contains(viaAlias, audit) {
			t.Errorf("MCP search ignored block_roles_exclude — the alias must narrow exactly like types_exclude (body %s)", viaAlias)
		}
		_, viaCanonical := mcpSearch(granteeAR, searchInput{Limit: 50, TypesExclude: []string{"audit-trail"}})
		if viaAlias != viaCanonical {
			t.Errorf("alias and canonical name answer differently:\nalias     %s\ncanonical %s", viaAlias, viaCanonical)
		}
	})

	t.Run("rest_search_honours_the_legacy_alias", func(t *testing.T) {
		_, viaAlias := restSearch(granteeAR, `{"limit":50,"block_roles_exclude":["audit-trail"]}`)
		if strings.Contains(viaAlias, audit) {
			t.Errorf("REST search ignored block_roles_exclude (body %s)", viaAlias)
		}
		// The echo names the EFFECTIVE filter, so the alias shows up under the
		// canonical key.
		var resp map[string]any
		if err := json.Unmarshal([]byte(viaAlias), &resp); err != nil {
			t.Fatalf("decode REST body: %v", err)
		}
		filters, _ := resp["filters"].(map[string]any)
		got, _ := json.Marshal(filters["types_exclude"])
		if string(got) != `["audit-trail"]` {
			t.Errorf("filters.types_exclude = %s, want the effective union [\"audit-trail\"]", got)
		}
	})

	t.Run("rest_search_refuses_an_over_bound_grant_set", func(t *testing.T) {
		code, body := restSearch(arFor(scopeBig, big), `{"limit":10}`)
		if code != http.StatusInternalServerError {
			t.Fatalf("REST search over the bound = %d, want 500 (a silent scope-only 200 is the failure mode under test; body %s)", code, body)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode REST body: %v", err)
		}
		if got, _ := resp["error"].(string); got != tooManyGrantsMsg {
			t.Fatalf("REST search error = %q, want the one rejection prose %q", got, tooManyGrantsMsg)
		}
	})

	t.Run("mcp_search_refuses_an_over_bound_grant_set", func(t *testing.T) {
		isErr, body := mcpSearch(arFor(scopeBig, big), searchInput{})
		if !isErr {
			t.Fatalf("MCP search over the bound succeeded (%s), want an error result", body)
		}
		if body != tooManyGrantsMsg {
			t.Fatalf("MCP search error = %q, want the one rejection prose %q", body, tooManyGrantsMsg)
		}
	})

	t.Run("rejected_type_filter_still_costs_no_grant_lookup", func(t *testing.T) {
		// V-W6 order: the type rejection is ahead of every pool touch, so the
		// over-bound tenant sees the TYPE error, not the grant refusal — the
		// registry is unwired here, which is itself a refusal (fail-closed).
		isErr, body := mcpSearch(arFor(scopeBig, big), searchInput{Types: []string{"knowledge"}})
		if !isErr || body != mcpTypeFilterUnwired {
			t.Fatalf("MCP search with an unwired registry = (isError %t) %q, want %q", isErr, body, mcpTypeFilterUnwired)
		}
	})

	// The store's own sentinel is what the transports translate — if this ever
	// stops being the single error class resolveGrants hands up, both mappings
	// above become guesses.
	t.Run("resolve_grants_returns_only_the_bound_sentinel", func(t *testing.T) {
		if _, err := resolveGrants(ctx, pool, arFor(scopeBig, big)); !errors.Is(err, store.ErrTooManyBlockGrants) {
			t.Fatalf("resolveGrants over the bound = %v, want store.ErrTooManyBlockGrants", err)
		}
		if got, err := resolveGrants(ctx, pool, granteeAR); err != nil || len(got) != 2 {
			t.Fatalf("resolveGrants below the bound = %v, %v; want the two grants and no error", got, err)
		}
	})
}
