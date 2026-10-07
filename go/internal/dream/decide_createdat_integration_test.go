//go:build integration

package dream

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestLoadCandidateCreatedAt pins the exact created_at lookup the causal
// direction check runs on before the tie distance: candidates arrive from the
// search with UpdatedAt only, and a block whose updated_at is past the source
// but whose created_at is before it is exactly the case the UpdatedAt
// approximation gets wrong (12 of 254 live wrong-direction causal decisions).
func TestLoadCandidateCreatedAt(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const id = "019d0000-0000-7000-9000-00000000ca01"
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, created_at, updated_at)
		 VALUES ($1::uuid, 'decisions', 'cand', 'candidate content', 'private', $2, $3)`,
		id, created, updated); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cands := []BlockInfo{{ID: id, UpdatedAt: updated}, {ID: "019d0000-0000-7000-9000-00000000ca02"}}
	loadCandidateCreatedAt(ctx, pool, cands)
	if !cands[0].CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v", cands[0].CreatedAt, created)
	}
	if !cands[1].CreatedAt.IsZero() {
		t.Fatalf("unknown candidate got CreatedAt %v, want zero", cands[1].CreatedAt)
	}

	// The check the lookup exists for: source created between the candidate's
	// created_at and updated_at — acceptCausal rejects, so it must downgrade.
	src := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	out, n := enforceCausalDirection([]Link{{TargetID: id, Relationship: "causal"}}, src, cands)
	if n != 1 || out[0].Relationship != "topical" {
		t.Fatalf("want downgrade on exact created_at, got %+v (n=%d)", out, n)
	}
}
