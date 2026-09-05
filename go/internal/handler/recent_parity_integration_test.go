//go:build integration

// T03-8b parity gate (DECISIONS K26): POST /api/recent and the MCP `recent`
// tool are two doors to ONE store read. This test pins that they stay that way:
// for the same filters they must return the same block ids in the same order,
// including the block-grant OR-arm, the legacy alias union, the store-side limit
// clamp and the V-11 untrusted framing.
//
// Why a parity test and not two independent expectation tests: an expectation
// test drifts silently when only one surface is touched (that is exactly how
// recent grew a REST-shaped hole in the first place). The fixture is fully
// deterministic (fixed ids, distinct updated_at ⇒ total order), so an order
// comparison is valid and no tie-break flake can hide a real divergence.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	rpHome  = "rp-home"
	rpOwner = "rp-owner"
	rpCatA  = "rp-cat-a"
	rpCatB  = "rp-cat-b"
)

// rpIDRe pulls the block ids out of the MCP tool's rendered text
// ("[1] title (cat, date) id:<uuid>[ [untrusted]]").
var rpIDRe = regexp.MustCompile(`id:([0-9a-f-]{36})`)

// rpSeed inserts one block with an explicit id and updated_at so both surfaces
// see one total order — a tie would make an order comparison meaningless.
func rpSeed(t *testing.T, pool *pgxpool.Pool, n int, scope, category, typeName, title, ts string, archived bool) string {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-9000-%012d", n)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, type_name, updated_at, created_at, is_archived)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::timestamptz, $7::timestamptz, $8)`,
		id, category, title, "content of "+title+" — padding ößü", scope, typeName, ts, archived); err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return id
}

// rpRESTResponse is the /api/recent envelope as a caller sees it.
type rpRESTResponse struct {
	Success bool `json:"success"`
	Count   int  `json:"count"`
	Filters struct {
		Category     any `json:"category"`
		Types        any `json:"types"`
		TypesExclude any `json:"types_exclude"`
	} `json:"filters"`
	Results []store.BlockPreview `json:"results"`
}

// rpCallREST runs one request against the real handler and returns the decoded
// envelope plus the raw body (the raw body is what a shape regression shows up
// in, so failures print it).
func rpCallREST(t *testing.T, h *RecentHandler, ar *auth.AuthResult, body string) (rpRESTResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/recent", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleRecent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST %s: status %d, body %s", body, rec.Code, rec.Body.String())
	}
	var out rpRESTResponse
	raw := rec.Body.String()
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("REST %s: decode %v (body %s)", body, err, raw)
	}
	if !out.Success {
		t.Fatalf("REST %s: success=false, body %s", body, raw)
	}
	if out.Count != len(out.Results) {
		t.Fatalf("REST %s: count %d but %d rows", body, out.Count, len(out.Results))
	}
	return out, raw
}

func TestRecentParityMCPvsREST_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	ownerTenant := bgTenant(t, pool, "rp-owner-tenant")
	granteeTenant := bgTenant(t, pool, "rp-grantee-tenant")
	bgMapScope(t, pool, rpOwner, ownerTenant)
	bgMapScope(t, pool, rpHome, granteeTenant)

	// 55 home blocks, category A, oldest timestamps — proves the >50 clamp.
	for i := 1; i <= 55; i++ {
		rpSeed(t, pool, i, rpHome, rpCatA, "knowledge",
			fmt.Sprintf("rp bulk %02d", i), fmt.Sprintf("2025-01-01T00:%02d:00Z", i), false)
	}
	// Category B, newest, one per type axis (checkpoint carries the V-11 flag).
	rpSeed(t, pool, 101, rpHome, rpCatB, "knowledge", "rp b knowledge", "2026-01-05T10:00:00Z", false)
	rpSeed(t, pool, 102, rpHome, rpCatB, "audit-trail", "rp b audit", "2026-01-04T10:00:00Z", false)
	rpSeed(t, pool, 103, rpHome, rpCatB, "checkpoint", "rp b checkpoint", "2026-01-03T10:00:00Z", false)
	// Foreign scope: granted (visible), granted+archived (must NEVER leak), ungranted.
	grantedID := rpSeed(t, pool, 201, rpOwner, rpCatB, "knowledge", "rp granted visible", "2026-01-06T10:00:00Z", false)
	grantedArchivedID := rpSeed(t, pool, 202, rpOwner, rpCatB, "knowledge", "rp granted ARCHIVED", "2026-01-07T10:00:00Z", true)
	ungrantedID := rpSeed(t, pool, 203, rpOwner, rpCatB, "knowledge", "rp ungranted", "2026-01-08T10:00:00Z", false)
	bgGrant(t, pool, grantedID, granteeTenant)
	bgGrant(t, pool, grantedArchivedID, granteeTenant)

	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	cfg := MCPConfig{Pool: pool, Blocktypes: reg}
	restH := NewRecentHandler(pool, staticConfigStore{cfg: &config.Config{}}, reg)
	callerAR := func() *auth.AuthResult {
		return &auth.AuthResult{
			IsValid: true, TenantRole: auth.RoleMember,
			HomeScope: rpHome, ReadScopes: []string{rpHome}, TenantID: granteeTenant,
		}
	}
	mcpCtx := func() context.Context {
		return context.WithValue(context.Background(), authResultKey, callerAR())
	}

	variants := []struct {
		name string
		in   recentInput
		body string
	}{
		{"plain", recentInput{}, `{}`},
		{"category-b", recentInput{Category: rpCatB, Limit: 20}, `{"category":"rp-cat-b","limit":20}`},
		{
			"excl-alias-union",
			recentInput{Category: rpCatB, Limit: 20, TypesExclude: []string{"audit-trail"}, BlockRolesExclude: []string{"checkpoint", "audit-trail"}},
			`{"category":"rp-cat-b","limit":20,"types_exclude":["audit-trail"],"block_roles_exclude":["checkpoint","audit-trail"]}`,
		},
		{"types-checkpoint", recentInput{Types: []string{"checkpoint"}, Limit: 20}, `{"types":["checkpoint"],"limit":20}`},
		{"limit-zero", recentInput{Limit: 0}, `{"limit":0}`},
		{"limit-999", recentInput{Limit: 999}, `{"limit":999}`},
		{"limit-negative", recentInput{Limit: -5}, `{"limit":-5}`},
		{"empty-result", recentInput{Category: "rp-nothing-here"}, `{"category":"rp-nothing-here"}`},
		{"category-a-limit-3", recentInput{Category: rpCatA, Limit: 3}, `{"category":"rp-cat-a","limit":3}`},
	}

	restCounts := map[string]int{}
	for _, v := range variants {
		res, _, err := mcpRecentHandler(cfg)(mcpCtx(), nil, v.in)
		if err != nil {
			t.Fatalf("%s: MCP transport error: %v", v.name, err)
		}
		if res.IsError {
			t.Fatalf("%s: MCP tool error: %s", v.name, mcpResultText(res))
		}
		mcpText := mcpResultText(res)
		mcpIDs := []string{}
		for _, m := range rpIDRe.FindAllStringSubmatch(mcpText, -1) {
			mcpIDs = append(mcpIDs, m[1])
		}

		rest, rawBody := rpCallREST(t, restH, callerAR(), v.body)
		restIDs := make([]string, 0, len(rest.Results))
		for _, bp := range rest.Results {
			restIDs = append(restIDs, bp.ID)
		}
		restCounts[v.name] = len(restIDs)

		// THE parity assertion: same ids, same order, same length.
		if strings.Join(mcpIDs, ",") != strings.Join(restIDs, ",") {
			t.Fatalf("%s: surfaces diverged\n  MCP  (%d): %v\n  REST (%d): %v\n  MCP text:\n%s\n  REST body:\n%s",
				v.name, len(mcpIDs), mcpIDs, len(restIDs), restIDs, mcpText, rawBody)
		}

		// V-11: the MCP marker and the REST field must agree row by row.
		for _, bp := range rest.Results {
			marked := strings.Contains(mcpText, "id:"+bp.ID+" "+untrustedMarker)
			if marked != bp.Untrusted {
				t.Fatalf("%s: untrusted framing diverged for %s (MCP marked=%t, REST untrusted=%t)\n%s",
					v.name, bp.ID, marked, bp.Untrusted, mcpText)
			}
		}

		// Fail-closed, both surfaces: a granted but ARCHIVED block never shows,
		// and neither does a foreign ungranted one.
		for _, forbidden := range []string{grantedArchivedID, ungrantedID} {
			if strings.Contains(mcpText, forbidden) {
				t.Fatalf("%s: MCP leaked %s:\n%s", v.name, forbidden, mcpText)
			}
			if strings.Contains(rawBody, forbidden) {
				t.Fatalf("%s: REST leaked %s:\n%s", v.name, forbidden, rawBody)
			}
		}
	}

	// The store-side clamp reaches the REST surface unchanged (no second clamp
	// in the handler): 0/negative ⇒ 10, 999 ⇒ 50, an explicit small limit stands.
	for _, c := range []struct {
		name string
		want int
	}{
		{"plain", 10}, {"limit-zero", 10}, {"limit-negative", 10},
		{"limit-999", 50}, {"category-a-limit-3", 3}, {"empty-result", 0},
	} {
		if got := restCounts[c.name]; got != c.want {
			t.Fatalf("%s: REST returned %d rows, want %d (clamp changed)", c.name, got, c.want)
		}
	}

	// The grant OR-arm is alive on the REST arm too — without it the parity
	// above would still hold only if BOTH surfaces lost the granted block.
	plain, plainRaw := rpCallREST(t, restH, callerAR(), `{}`)
	found := false
	for _, bp := range plain.Results {
		if bp.ID == grantedID {
			found = true
		}
	}
	if !found {
		t.Fatalf("grant OR-arm dead on REST — granted block %s missing:\n%s", grantedID, plainRaw)
	}

	// Shape: the echoed filters are the EFFECTIVE ones (alias already unioned)
	// and no "limit" key claims a number the store did not use.
	aliased, aliasedRaw := rpCallREST(t, restH, callerAR(),
		`{"category":"rp-cat-b","types_exclude":["audit-trail"],"block_roles_exclude":["checkpoint"]}`)
	excl, ok := aliased.Filters.TypesExclude.([]any)
	if !ok || len(excl) != 2 {
		t.Fatalf("filters.types_exclude is not the 2-element union: %v (body %s)", aliased.Filters.TypesExclude, aliasedRaw)
	}
	if strings.Contains(aliasedRaw, `"limit"`) {
		t.Fatalf("response echoes a limit the store may have clamped: %s", aliasedRaw)
	}
	if !strings.Contains(aliasedRaw, `"content_preview"`) || !strings.Contains(aliasedRaw, `"lifecycle_state"`) {
		t.Fatalf("result rows lost the search-shaped fields: %s", aliasedRaw)
	}
}

// TestRecentParityOverGrantBound_Integration is the parity half of T04-20: a
// tenant whose row-level grant set exceeds the per-request bound is REFUSED on
// both recent surfaces, with the one shared prose (tooManyGrantsMsg) — never
// answered from a quietly narrowed, scope-only view on one of them. Without the
// mapping in context_recent.go the REST arm would return a normal 200 while MCP
// refuses, which is exactly the divergence this file exists to prevent.
//
// Fixture pattern from block_grant_bound_t0420_integration_test.go; the bulk
// seeder (t0420HBulkGrants) is reused rather than copied so the two tests
// cannot drift over what "over the bound" means.
func TestRecentParityOverGrantBound_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	const scopeBulk = "rp-bound-bulk"
	const scopeBig = "rp-bound-big"
	const scopeSmall = "rp-bound-small"
	owner := bgTenant(t, pool, "rp-bound-owner")
	big := bgTenant(t, pool, "rp-bound-big")
	small := bgTenant(t, pool, "rp-bound-small")
	bgMapScope(t, pool, scopeBulk, owner)
	bgMapScope(t, pool, scopeBig, big)
	bgMapScope(t, pool, scopeSmall, small)
	rpSeed(t, pool, 401, scopeSmall, rpCatA, "knowledge", "rp bound own block", "2026-02-01T10:00:00Z", false)
	t0420HBulkGrants(t, pool, scopeBulk, big, t0420HBound+1)

	arFor := func(scope, tenant string) *auth.AuthResult {
		return &auth.AuthResult{
			IsValid: true, TenantRole: auth.RoleMember,
			HomeScope: scope, ReadScopes: []string{scope}, TenantID: tenant,
		}
	}
	restCall := func(ar *auth.AuthResult) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/recent", strings.NewReader(`{}`))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		NewRecentHandler(pool, staticConfigStore{cfg: &config.Config{}}, nil).HandleRecent(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /api/recent: %v (body %s)", err, rec.Body.String())
		}
		return rec.Code, resp
	}
	mcpCall := func(ar *auth.AuthResult) (bool, string) {
		res, _, err := mcpRecentHandler(MCPConfig{Pool: pool})(
			context.WithValue(context.Background(), authResultKey, ar), nil, recentInput{})
		if err != nil {
			t.Fatalf("mcp recent: transport error %v, want a tool-level answer", err)
		}
		return res.IsError, mcpResultText(res)
	}

	// Control: below the bound BOTH surfaces answer normally, so a green
	// refusal below cannot come from broken wiring.
	t.Run("below_bound_both_answer", func(t *testing.T) {
		code, resp := restCall(arFor(scopeSmall, small))
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("REST below the bound = %d %v, want 200 success", code, resp)
		}
		isErr, text := mcpCall(arFor(scopeSmall, small))
		if isErr {
			t.Fatalf("MCP below the bound refused: %s", text)
		}
	})

	t.Run("over_bound_both_refuse_with_one_prose", func(t *testing.T) {
		code, resp := restCall(arFor(scopeBig, big))
		if code != http.StatusInternalServerError {
			t.Fatalf("REST over the bound = %d, want 500 (a silent scope-only 200 is the failure mode under test)", code)
		}
		if got, _ := resp["error"].(string); got != tooManyGrantsMsg {
			t.Fatalf("REST error = %q, want the one rejection prose %q", got, tooManyGrantsMsg)
		}
		if resp["success"] != false {
			t.Fatalf("REST refusal must carry success:false, got %v", resp["success"])
		}
		isErr, text := mcpCall(arFor(scopeBig, big))
		if !isErr {
			t.Fatalf("MCP over the bound succeeded (%s), want an error result", text)
		}
		if text != tooManyGrantsMsg {
			t.Fatalf("MCP error = %q, want %q", text, tooManyGrantsMsg)
		}
		// The point of the test: ONE prose, both surfaces.
		if got, _ := resp["error"].(string); got != text {
			t.Fatalf("rejection prose diverged:\n  REST: %q\n  MCP:  %q", got, text)
		}
	})
}

// TestRecentRESTRateLimitParity_Integration pins that /api/recent shares the
// read budget of /api/search byte for byte: same bucket ("query"), same noun,
// same limit key — alternating the two routes must not double a caller's reads.
func TestRecentRESTRateLimitParity_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	tenant := bgTenant(t, pool, "rp-rl-tenant")
	bgMapScope(t, pool, rpHome, tenant)
	keyID := bgKey(t, pool, "rp-rl-key", rpHome)
	rpSeed(t, pool, 301, rpHome, rpCatA, "knowledge", "rp rl block", "2026-01-02T10:00:00Z", false)

	ar := &auth.AuthResult{
		IsValid: true, TenantRole: auth.RoleMember, ApiKeyID: keyID,
		HomeScope: rpHome, ReadScopes: []string{rpHome}, TenantID: tenant,
	}
	cfgOne := staticConfigStore{cfg: &config.Config{Query: config.QueryConfig{RateLimitRead: 1}}}
	recentH := NewRecentHandler(pool, cfgOne, nil)
	searchH := NewSearchHandler(pool, cfgOne, nil)

	// One read already booked in the shared "query" bucket ⇒ the budget of 1 is
	// spent for BOTH routes.
	if err := store.LogAccess(context.Background(), pool, keyID, "", "query"); err != nil {
		t.Fatalf("log access: %v", err)
	}

	call := func(h http.HandlerFunc, path, body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		h(rec, req)
		var out struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: decode %v (body %s)", path, err, rec.Body.String())
		}
		return rec.Code, out.Error
	}

	recentCode, recentErr := call(recentH.HandleRecent, "/api/recent", `{}`)
	searchCode, searchErr := call(searchH.HandleSearch, "/api/search", `{"query":""}`)
	if recentCode != http.StatusTooManyRequests {
		t.Fatalf("/api/recent: status %d, want 429 (error %q)", recentCode, recentErr)
	}
	if searchCode != http.StatusTooManyRequests {
		t.Fatalf("/api/search: status %d, want 429 — fixture broken (error %q)", searchCode, searchErr)
	}
	if recentErr != searchErr {
		t.Fatalf("429 prose diverged:\n  recent: %q\n  search: %q", recentErr, searchErr)
	}
	if recentErr != "Rate limit exceeded: max 1 reads per 60 seconds" {
		t.Fatalf("unexpected 429 prose: %q", recentErr)
	}

	// With the limit off (0) the route answers normally — the gate is the
	// config value, not a hardcoded ceiling.
	openH := NewRecentHandler(pool, staticConfigStore{cfg: &config.Config{}}, nil)
	code, errMsg := call(openH.HandleRecent, "/api/recent", `{}`)
	if code != http.StatusOK {
		t.Fatalf("rate limit off: status %d (error %q)", code, errMsg)
	}
}
