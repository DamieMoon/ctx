//go:build integration

package backends_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestMigration155ProtocolCheckAdmitsAnthropic runs the real migration chain
// on a prod-parity PostgreSQL and pins what Migration 155 changed: the
// (formerly unnamed) CHECK on context_backends.protocol now carries its
// explicit name, admits 'anthropic' next to the three original wires, and
// still rejects anything else with a check_violation (SQLSTATE 23514) — so a
// typo'd protocol cannot reach the pool and fall onto the Ollama path.
func TestMigration155ProtocolCheckAdmitsAnthropic(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = 'context_backends'::regclass AND conname = 'context_backends_protocol_check'`,
	).Scan(&def); err != nil {
		t.Fatalf("constraint context_backends_protocol_check not found after the chain: %v", err)
	}
	for _, want := range []string{"openai", "ollama", "rerank", "anthropic"} {
		if !strings.Contains(def, "'"+want+"'") {
			t.Errorf("constraint %q lacks %q", def, want)
		}
	}

	// Exactly one CHECK mentions the column — the DO block's catalog lookup
	// stays unambiguous.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		  WHERE conrelid = 'context_backends'::regclass AND contype = 'c'
		    AND pg_get_constraintdef(oid) LIKE '%protocol%'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("CHECKs mentioning protocol = %d (err %v), want exactly 1", n, err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_backends (name, base_url, protocol, api_key_ref, locality, num_ctx)
		 VALUES ('anthropic-it', 'https://api.anthropic.com', 'anthropic', 'anthropic-chat', 'external', 100000)`); err != nil {
		t.Fatalf("anthropic row rejected by the widened CHECK: %v", err)
	}

	_, err := pool.Exec(ctx,
		`INSERT INTO context_backends (name, base_url, protocol) VALUES ('grpc-it', 'http://x', 'grpc')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "context_backends_protocol_check" {
		t.Fatalf("unknown protocol must fail the named CHECK with 23514, got %v", err)
	}
}
