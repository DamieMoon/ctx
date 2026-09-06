//go:build integration

// issues_core_integration_test.go — the standing integration guards of the
// T03-10 core. First: the ONE
// thing T03-10 must NOT unify (design/03-oberflaeche.md Naht 4, §5.3, Gate b):
// the three issue transports answer "which scope does this write go to"
// differently, and that difference is fachlich, not accidental.
//
// The three answers are only distinguishable when home scope, project scope and
// requested scope are three different things. Every existing issue test builds a
// key whose home scope IS the project scope, so all three answers coincide and a
// core that quietly picked one of them would pass. This test gives ONE key
//
//	home = t310:home, allowed+write = {the project scope}
//
// and drives the same create through all three transports:
//
//	REST   POST /api/project/{id}/issues   ⇒ the PROJECT scope (path-resolved)
//	manage issue-create with data.scope    ⇒ the REQUESTED scope
//	manage issue-create without a scope    ⇒ the HOME scope (fallback)
//	MCP    issue_create                    ⇒ the HOME scope, always
//
// Run: `go test -tags=integration -p 1 -run TestIssueScopeSources ./internal/handler/`.
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// t310BlockScope reads back the scope a block actually landed in — the wire
// envelope is the transport's word, this is the store's.
func t310BlockScope(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var scope string
	if err := pool.QueryRow(context.Background(),
		`SELECT scope FROM context_blocks WHERE title LIKE '%' || $1 ORDER BY created_at DESC LIMIT 1`,
		title).Scan(&scope); err != nil {
		t.Fatalf("read back scope of %q: %v", title, err)
	}
	return scope
}

func TestIssueScopeSourcesStayThree_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	pid, projScope := w6SeedProject(t, pool, "t310scope")

	// ONE key for all three transports. Its home scope is NOT the project scope,
	// and the project scope is writable through write_scopes ∩ allowed — so
	// writableBlockScopes(ar) holds BOTH, and every transport could pick either.
	const homeScope = "t310:home"
	ar := &auth.AuthResult{
		IsValid: true, TenantRole: auth.RoleMember,
		HomeScope:     homeScope,
		AllowedScopes: []string{projScope},
		ReadScopes:    []string{homeScope, projScope},
		WriteScopes:   []string{projScope},
		TenantID:      store.DefaultTenantID,
	}
	if got := writableBlockScopes(ar); len(got) != 2 {
		t.Fatalf("precondition: the key must be able to write BOTH scopes, got %v", got)
	}

	t.Run("rest_writes_the_project_scope", func(t *testing.T) {
		rec := w7Do(t, pool, reg, nil, ar, http.MethodPost,
			"/api/project/"+pid+"/issues", `{"title":"t310 rest"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := w6DecodeBody(t, rec)
		issue, _ := body["issue"].(map[string]any)
		if issue["scope"] != projScope {
			t.Errorf("REST envelope scope = %v, want the PROJECT scope %q", issue["scope"], projScope)
		}
		if got := t310BlockScope(t, pool, "t310 rest"); got != projScope {
			t.Errorf("REST wrote into %q, want the PROJECT scope %q (home was %q)", got, projScope, homeScope)
		}
	})

	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)

	t.Run("manage_honours_the_requested_scope", func(t *testing.T) {
		code, resp := callManage(t, h, ar, "issue-create", "",
			map[string]any{"title": "t310 manage explicit", "scope": projScope})
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("manage issue-create: code=%d resp=%v", code, resp)
		}
		if got := t310BlockScope(t, pool, "t310 manage explicit"); got != projScope {
			t.Errorf("manage wrote into %q, want the REQUESTED scope %q", got, projScope)
		}
	})

	t.Run("manage_falls_back_to_the_home_scope", func(t *testing.T) {
		code, resp := callManage(t, h, ar, "issue-create", "",
			map[string]any{"title": "t310 manage fallback"})
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("manage issue-create: code=%d resp=%v", code, resp)
		}
		if got := t310BlockScope(t, pool, "t310 manage fallback"); got != homeScope {
			t.Errorf("manage wrote into %q, want the HOME scope %q", got, homeScope)
		}
	})

	t.Run("mcp_is_pinned_to_the_home_scope", func(t *testing.T) {
		cfg := MCPConfig{Pool: pool, Blocktypes: reg}
		res, _, err := mcpIssueCreateHandler(cfg)(w12Ctx(ar), nil, issueCreateInput{Title: "t310 mcp"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if res.IsError {
			t.Fatalf("mcp issue_create failed: %s", mcpText(res))
		}
		if !strings.Contains(mcpText(res), `"scope": "`+homeScope+`"`) {
			t.Errorf("MCP envelope does not carry the HOME scope %q: %s", homeScope, mcpText(res))
		}
		// The sharp half: the project scope is writable for this key, and MCP
		// still must not choose it — the tools carry no project dimension.
		if got := t310BlockScope(t, pool, "t310 mcp"); got != homeScope {
			t.Errorf("MCP wrote into %q, want the HOME scope %q", got, homeScope)
		}
	})
}

// TestIssueCreateEntryTransitionIsPolicyGated_Integration guards the one piece of
// policy logic the core OWNS on the create path: a caller-supplied status has to
// be a valid ENTRY transition ("" → status), or the create is refused. Measured
// on 2026-09-05: dropping set.ValidateTransition out of issueCreateCore leaves
// the entire existing suite green — the two invalid_transition_422 subtests
// (project_issues_w7_integration_test.go, context_manage_issues_integration_test.go)
// both exercise the UPDATE transition, which store.UpdateIssueBlock validates on
// its own. Without this test the core's entry check would be unwatched (W10) and
// an out-of-policy status would silently become a 200.
//
// Run: `go test -tags=integration -p 1 -run TestIssueCreateEntryTransition ./internal/handler/`.
func TestIssueCreateEntryTransitionIsPolicyGated_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	pid, projScope := w6SeedProject(t, pool, "t310entry")
	writer := w7Writer(projScope)
	manageAR := &auth.AuthResult{IsValid: true, HomeScope: projScope,
		ReadScopes: []string{projScope}, TenantID: store.DefaultTenantID}
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)

	t.Run("rest_refuses_an_out_of_policy_entry", func(t *testing.T) {
		rec := w7Do(t, pool, reg, nil, writer, http.MethodPost,
			"/api/project/"+pid+"/issues", `{"title":"t310 entry rest","status":"nonsense"}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("manage_refuses_an_out_of_policy_entry", func(t *testing.T) {
		code, resp := callManage(t, h, manageAR, "issue-create", "",
			map[string]any{"title": "t310 entry manage", "status": "nonsense"})
		if code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422 (resp=%v)", code, resp)
		}
	})

	t.Run("mcp_refuses_an_out_of_policy_entry", func(t *testing.T) {
		cfg := MCPConfig{Pool: pool, Blocktypes: reg}
		res, _, err := mcpIssueCreateHandler(cfg)(w12Ctx(manageAR), nil,
			issueCreateInput{Title: "t310 entry mcp", Status: "nonsense"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if !res.IsError {
			t.Errorf("mcp issue_create accepted an out-of-policy entry status: %s", mcpText(res))
		}
	})

	// The refusal must be a refusal, not a downgrade to the default status: no
	// block may exist under any of the three titles.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE title LIKE '%t310 entry%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d block(s) written despite an out-of-policy entry status", n)
	}
}
