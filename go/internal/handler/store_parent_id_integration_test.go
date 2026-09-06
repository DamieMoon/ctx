//go:build integration

// NZ-3 point 8 — the follow-up T02-11 named in its Übergabe 1: POST /api/store
// carries an optional `parent_id`, so parent.mode=required is a CONDITION on
// that route instead of a total refusal.
//
// Before this wave the gate could only refuse: no request shape on the claim
// chain carried a parent, so a registry row that demanded one made its type
// unwritable over REST — orphan prevention by way of "nobody may write this
// type at all". The field turns the same gate into "this write carries the
// parent the type demands", and the write path hands that parent to
// store.PutBlockParent, the ONE parent_id write path (structlinks.go).
//
// Subtests, RED before the wave unless marked:
//
//	a with_parent_stored      — {type: <required>, parent_id: <live block>} ⇒ 200
//	                            and the stored row carries parent_id. RED: 422
//	                            parent_required — the field did not exist.
//	b without_parent_refused  — the T02-11 verdict, byte-identical, on the same
//	                            registry. GREEN before AND after: a regression
//	                            pin, not a gate (declared as such in the wave
//	                            protocol, W19).
//	c bad_parent_uniform      — unknown, foreign-scope and malformed parent_id
//	                            answer ONE class with ONE prose (no existence
//	                            oracle, §5.2) and NOTHING is written. RED: 200,
//	                            block stored parentless.
//	d parent_is_type_agnostic — a parent.mode=none type may carry a parent too:
//	                            the field is a property of the WRITE, not of the
//	                            type. RED: 200 but parent_id NULL. It also pins
//	                            that this wave does NOT build the optional-vs-none
//	                            distinction (briefing point 14, deliberately open).
//	e response_shape_frozen   — the 200 answer is byte-shaped as before: {success,
//	                            block} and no parent_id key on the wire.
//	f self_parent_refused     — naming the very block (category, title, scope)
//	                            addresses is refused BEFORE the upsert, so the
//	                            one deterministic post-write link failure cannot
//	                            leave an orphan. RED: 200 and the block became a
//	                            parentless block of the required type.
//
//	go test -tags=integration ./internal/handler/ -run TestStoreParentID -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStoreParentID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, seed := range []struct{ name, mode string }{
		{"np-required", blocktype.ParentModeRequired},
		{"np-none", blocktype.ParentModeNone},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
			 VALUES ($1::text, '_global', $1::text, false, false,
			         jsonb_build_object('v', 1, 'parent', jsonb_build_object('mode', $2::text)))`,
			seed.name, seed.mode); err != nil {
			t.Fatalf("seed fixture type %s: %v", seed.name, err)
		}
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s — a fixture row does not decode", reg.Health())
	}
	// Premises, asserted rather than assumed: every verdict below is about a
	// MODE, so a fixture that resolved differently would prove something else.
	for _, p := range []struct{ name, want string }{
		{"np-required", blocktype.ParentModeRequired},
		{"np-none", blocktype.ParentModeNone},
	} {
		if got := reg.Snapshot().ParentMode(p.name); got != p.want {
			t.Fatalf("fixture ParentMode(%s) = %q, want %q — premise of this file", p.name, got, p.want)
		}
	}

	cfg := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}

	_, plain, err := store.CreateApiKey(ctx, pool, "np-rest", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v (valid=%v)", err, ar != nil && ar.IsValid)
	}
	keyCtx := context.WithValue(ctx, authResultKey, ar)

	mkBlock := func(title, scope string) *store.Block {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "test", title, "parent payload "+title,
			nil, nil, scope, true, store.SensitivityWrite{Value: backends.SensPublic}, "")
		if err != nil {
			t.Fatalf("seed block %q: %v", title, err)
		}
		return b
	}
	parent := mkBlock("np-parent-private", "private")
	foreign := mkBlock("np-parent-foreign", "shared")

	type answer struct {
		status int
		code   string
		msg    string
		body   map[string]any
	}
	post := func(payload map[string]any) answer {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw)))
		req = req.WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)
		var decoded map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		out := answer{status: rec.Code, body: decoded}
		if s, ok := decoded["code"].(string); ok {
			out.code = s
		}
		if s, ok := decoded["error"].(string); ok {
			out.msg = s
		}
		return out
	}
	parentOf := func(title string) string {
		t.Helper()
		var pid string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(parent_id::text, '') FROM context_blocks WHERE title = $1`,
			title).Scan(&pid); err != nil {
			t.Fatalf("read parent_id of %q: %v", title, err)
		}
		return pid
	}
	blockCount := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count blocks %q: %v", title, err)
		}
		return n
	}

	t.Run("a_with_parent_stored", func(t *testing.T) {
		const title = "np-a-required-with-parent"
		got := post(map[string]any{
			"category": "test", "title": title, "content": "a required-parent type WITH its parent",
			"type": "np-required", "parent_id": parent.ID,
		})
		if got.status != http.StatusOK {
			t.Fatalf("status = %d (%s/%q), want 200 — a write that names the parent the type "+
				"demands satisfies parent.mode=required", got.status, got.code, got.msg)
		}
		if n := blockCount(title); n != 1 {
			t.Fatalf("%d block(s) titled %q, want exactly 1", n, title)
		}
		if pid := parentOf(title); pid != parent.ID {
			t.Errorf("parent_id = %q, want %q — a 200 that drops the parent would write the "+
				"very orphan the mode forbids", pid, parent.ID)
		}
	})

	t.Run("b_without_parent_refused", func(t *testing.T) {
		const title = "np-b-required-no-parent"
		wantMsg := `type: "np-required" requires a parent (parent.mode=required) — ` +
			`this write surface carries no parent, so a block of that type is created ` +
			`through its own domain path, not claimed here`
		got := post(map[string]any{
			"category": "test", "title": title, "content": "a required-parent type without a parent",
			"type": "np-required",
		})
		if got.status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 — the T02-11 verdict stays for a write that names no parent",
				got.status)
		}
		if got.code != "parent_required" {
			t.Errorf("code = %q, want parent_required (byte-exact machine class)", got.code)
		}
		if got.msg != wantMsg {
			t.Errorf("prose = %q, want %q — the T02-11 message stays byte-identical across arms",
				got.msg, wantMsg)
		}
		if n := blockCount(title); n != 0 {
			t.Errorf("%d block(s) titled %q stored, want the payload never written", n, title)
		}
	})

	t.Run("c_bad_parent_uniform", func(t *testing.T) {
		// One class for every unlinkable id — unknown, foreign-scope, malformed,
		// the nil uuid: a client must not be able to probe a foreign block id for
		// existence through this field (§5.2, the rule the issue arms answer with
		// their uniform 404). The self-referencing case has its own subtest below,
		// because it also has to prove that nothing was written.
		cases := []struct{ name, title, parentID string }{
			{"unknown", "np-c-unknown", "0199ffff-ffff-7fff-8fff-ffffffffffff"},
			{"foreign_scope", "np-c-foreign", foreign.ID},
			{"malformed", "np-c-malformed", "not-a-uuid"},
			{"nil_uuid", "np-c-nil-uuid", "00000000-0000-0000-0000-000000000000"},
		}
		var first answer
		for i, c := range cases {
			got := post(map[string]any{
				"category": "test", "title": c.title, "content": "bad parent probe " + c.name,
				"type": "np-none", "parent_id": c.parentID,
			})
			if got.status == http.StatusOK {
				t.Errorf("[%s] accepted parent_id %q — a parent that cannot be linked must not "+
					"buy a 200 (body %v)", c.name, c.parentID, got.body)
			}
			if n := blockCount(c.title); n != 0 {
				t.Errorf("[%s] %d block(s) titled %q stored — the refusal must land BEFORE the "+
					"write, or the answer is an error and the block exists anyway",
					c.name, n, c.title)
			}
			if i == 0 {
				first = got
				continue
			}
			if got.code != first.code || got.msg != first.msg || got.status != first.status {
				t.Errorf("[%s] answers %d/%s/%q, unknown answers %d/%s/%q — the cases must "+
					"be indistinguishable (no existence oracle)",
					c.name, got.status, got.code, got.msg, first.status, first.code, first.msg)
			}
		}
	})

	t.Run("d_parent_is_type_agnostic", func(t *testing.T) {
		// parent.mode=none does NOT mean "may carry no parent" — the mode axis
		// only decides whether a parent is DEMANDED. Distinguishing optional from
		// none is the open half of E02-4 and deliberately not built here.
		const title = "np-d-none-with-parent"
		got := post(map[string]any{
			"category": "test", "title": title, "content": "a none-mode type WITH a parent",
			"type": "np-none", "parent_id": parent.ID,
		})
		if got.status != http.StatusOK {
			t.Fatalf("status = %d (%s/%q), want 200", got.status, got.code, got.msg)
		}
		if pid := parentOf(title); pid != parent.ID {
			t.Errorf("parent_id = %q, want %q — the field is a property of the write, not of the type",
				pid, parent.ID)
		}
	})

	t.Run("f_self_parent_refused_before_the_write", func(t *testing.T) {
		// The one deterministic way a link could fail AFTER the upsert: the named
		// parent is the very block (category, title, scope) addresses. The upsert
		// would have written the content and the claimed type, and
		// PutBlockParent's childID == parentID line would then refuse — leaving a
		// block of a required-parent type with parent_id NULL, the orphan the gate
		// exists to prevent. The pre-check compares identities, so the refusal
		// lands before the write.
		const title = "np-f-self"
		self := mkBlock(title, "private")
		got := post(map[string]any{
			"category": "test", "title": title, "content": "changed content",
			"type": "np-required", "parent_id": self.ID,
		})
		if got.status == http.StatusOK {
			t.Fatalf("accepted a block as its own parent (body %v)", got.body)
		}
		if got.code != "unknown_parent" {
			t.Errorf("code = %q, want unknown_parent — the same class as every other "+
				"unlinkable parent, or the difference tells a caller which case it hit", got.code)
		}
		var content, typeName string
		if err := pool.QueryRow(ctx,
			`SELECT content, COALESCE(type_name, '') FROM context_blocks WHERE title = $1`,
			title).Scan(&content, &typeName); err != nil {
			t.Fatalf("read %q: %v", title, err)
		}
		if content != "parent payload "+title {
			t.Errorf("content = %q — the refused write must not have upserted anything", content)
		}
		if typeName == "np-required" {
			t.Errorf("type_name = %q — the refused write claimed the type anyway, which is the "+
				"orphan this check prevents", typeName)
		}
		if pid := parentOf(title); pid != "" {
			t.Errorf("parent_id = %q, want empty", pid)
		}
	})

	t.Run("e_response_shape_frozen", func(t *testing.T) {
		// The wave is additive on the REQUEST only. A parent_id key on the answer
		// would be a new wire contract for every existing client.
		const title = "np-e-shape"
		got := post(map[string]any{
			"category": "test", "title": title, "content": "shape probe",
			"type": "np-required", "parent_id": parent.ID,
		})
		if got.status != http.StatusOK {
			t.Fatalf("status = %d (%s/%q), want 200", got.status, got.code, got.msg)
		}
		keys := make([]string, 0, len(got.body))
		for k := range got.body {
			keys = append(keys, k)
		}
		if len(keys) != 2 || got.body["success"] != true || got.body["block"] == nil {
			t.Fatalf("top-level keys = %v, want exactly {success, block}", keys)
		}
		block, ok := got.body["block"].(map[string]any)
		if !ok {
			t.Fatalf("block is %T, want an object", got.body["block"])
		}
		if _, present := block["parent_id"]; present {
			t.Errorf("the 200 answer carries parent_id — the response shape must stay unchanged")
		}
	})
}
