//go:build integration

// NZ-3 point 9 — the fourth store.InsertCommentBlock site (apply.go, the forge
// pull-create of a comment) runs past the handler claim gates, and this file is
// the BELEG that it may: the arm carries its parent BY CONSTRUCTION, so the
// parent.mode=required gate has no condition to decide there.
//
// The construction, in three links, one probe each:
//
//  1. ApplyComments reaches applyComment ONLY through a mapped parent issue
//     (`parent, hasParent := imaps[c.IssueNumber]; if !hasParent { continue }`,
//     apply.go). A comment whose issue is not pulled yet is skipped — no block,
//     no mapping, no write at all.
//  2. The parent it then passes is context_project_sync_map.block_id, a column
//     declared `UUID NOT NULL REFERENCES context_blocks(id)` (113_baseline.sql):
//     a row that exists names a block that exists. The pulled comment therefore
//     carries a non-NULL parent_id.
//  3. Even if a caller could hand it an empty parent, store.InsertCommentBlock
//     refuses that itself with ErrCommentParentRequired BEFORE any write — the
//     fail-closed floor under both links above.
//
// This test is GREEN before this wave and after it: it is a Beleg, not a gate,
// and is declared as such in the wave protocol (W19). What the wave changes at
// this site is the DOC line, not the code — see the comment at the call site.
//
//	go test -tags=integration ./internal/forge/ -run TestApply_CommentParent -count=1 -v
package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestApply_CommentParentByConstruction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := seedIFProject(t, pool, "nz3parent")
	a := applierWithRegistry(pool)

	commentBlocks := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE type_name = 'comment' AND scope = $1`,
			proj.Scope).Scan(&n); err != nil {
			t.Fatalf("count comment blocks: %v", err)
		}
		return n
	}

	c := CommentRemote{ID: 4711, IssueNumber: 7, Body: "a remote comment", UpdatedAt: time.Now().UTC()}

	t.Run("unmapped_parent_is_skipped", func(t *testing.T) {
		res, err := a.ApplyComments(ctx, proj, []CommentRemote{c})
		if err != nil {
			t.Fatalf("apply comments without a parent mapping: %v", err)
		}
		if res.Applied != 0 || res.Conflicts != 0 {
			t.Errorf("res = %+v, want Applied 0 Conflicts 0 — a comment whose issue is not "+
				"pulled yet must be skipped, not written parentless", res)
		}
		if n := commentBlocks(); n != 0 {
			t.Errorf("%d comment block(s) written, want 0", n)
		}
	})

	t.Run("mapped_parent_is_written_as_parent_id", func(t *testing.T) {
		iss := IssueRemote{Number: 7, Title: "the parent issue", Body: "body", State: "open",
			UpdatedAt: time.Now().UTC()}
		if res, err := a.ApplyIssues(ctx, proj, []IssueRemote{iss}); err != nil || res.Applied != 1 {
			t.Fatalf("pull-create the parent issue: res=%+v err=%v", res, err)
		}
		_, _, issueBlockID, _ := readMapping(t, pool, proj.ID, "issue", 7)

		res, err := a.ApplyComments(ctx, proj, []CommentRemote{c})
		if err != nil || res.Applied != 1 {
			t.Fatalf("apply the comment: res=%+v err=%v, want Applied 1", res, err)
		}
		_, _, commentBlockID, _ := readMapping(t, pool, proj.ID, "comment", c.ID)
		var parentID string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(parent_id::text, '') FROM context_blocks WHERE id = $1::uuid`,
			commentBlockID).Scan(&parentID); err != nil {
			t.Fatalf("read parent_id of the pulled comment: %v", err)
		}
		if parentID != issueBlockID {
			t.Errorf("parent_id = %q, want the mapped issue block %q — the arm's parent is the "+
				"NOT NULL block_id of the sync mapping, never a client claim", parentID, issueBlockID)
		}
	})

	t.Run("store_floor_refuses_an_empty_parent", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		before := commentBlocks()
		_, err = store.InsertCommentBlock(ctx, tx, "",
			store.CommentFields{Content: "no parent"}, []string{proj.Scope})
		if !errors.Is(err, store.ErrCommentParentRequired) {
			t.Fatalf("InsertCommentBlock with an empty parent = %v, want ErrCommentParentRequired — "+
				"this is the floor under every one of the four comment write sites", err)
		}
		if n := commentBlocks(); n != before {
			t.Errorf("comment blocks %d → %d, want unchanged: the refusal is the FIRST statement "+
				"of the primitive, before any write", before, n)
		}
	})
}
