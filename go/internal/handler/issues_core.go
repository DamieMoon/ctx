// issues_core.go — the ONE issue write logic behind the three issue transports
// (design/03-oberflaeche.md §4.10, Naht 4; decision E03-4 = A "three transports
// stay, thin"). The REST W7 surface (project_issues_write.go), the manage
// issue-* family (context_manage_issues.go) and the MCP issue tools
// (mcp_issues.go) all run the three functions below. What stays in each
// transport is exactly what DIFFERS between them:
//
//   - The scope source. Three different answers (Naht 4), none of them a
//     duplicate: REST binds to the path-resolved PROJECT scope and passes
//     writableScopes = [that scope] ("scope purity" — a block of another,
//     even writable, project reads as not-found through THIS project's path);
//     manage takes p.Scope with an ar.HomeScope fallback and passes
//     writableBlockScopes(ar); MCP takes ar.HomeScope fixed and likewise
//     writableBlockScopes(ar). They arrive as issueWriteEnv — the core never
//     derives a scope of its own.
//   - The response envelope. REST answers {success, render, issue}, manage the
//     same plus "action", MCP mcpIssueResult. The core returns a *store.Block.
//   - The error prose, and that is deliberate. The two EXISTING mappers stay in
//     their transports (writeIssueStoreError, context_manage_issues.go; and
//     mcpIssueError, mcp_issues.go) because they word the SAME class
//     differently on purpose: "comment requires a parent_id" (REST/manage)
//     against "comment requires an issue_id" (MCP), "Internal server error"
//     against "issue write failed" (design/03 §5.3). One message per class in
//     the core would break one of the two wires. The core therefore returns
//     TYPED SENTINELS — the store.ErrIssue*/blocktype.Err* that already exist,
//     plus errIssueRegistryUnavailable below — and never a rendered message.
//   - The REST-only write throttle (writeRateBlocked) and access booking
//     (logIssueWrite). E03-5 = B — rate-limit and access logging stay per
//     transport (REST only today); the core exposes the hook point but does not
//     wire the gates; T03-10b withdrawn. So the core takes them as a PARAMETER,
//     not as a call: issueWriteEnv.ApiKeyID is the metering identity, and a
//     transport that wants to throttle or book runs its own gate around the core
//     call (REST does, see project_issues_write.go). The core itself invokes no
//     gate — wiring one here would drop the E03-5 verdict into a refactor.
//
// The doctrine test for this file (design/03 §4.10, last line): no signature in
// here names the HTTP response writer or the MCP tool-result type — the gate
// greps this very file for both type names and must find zero, so they are not
// spelled out even in prose. That is the provable difference between a core and
// moved code.
package handler

import (
	"context"
	"errors"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// issueWriteEnv carries the three transport decisions the core must NOT make
// itself (Naht 4). It is a value, never a source of truth the core re-derives:
// whatever a transport puts in here IS the write's authority.
type issueWriteEnv struct {
	// Scope is the scope a CREATE writes into (REST: the project's; manage:
	// p.Scope or ar.HomeScope; MCP: ar.HomeScope). Update and comment ignore
	// it — an update targets a block by id and a comment always inherits the
	// PARENT's scope (the comment-scope invariant lives in the store).
	Scope string
	// WritableScopes is the set the store re-asserts the write against in the
	// same transaction. REST passes []string{Scope} (scope purity), manage and
	// MCP pass writableBlockScopes(ar) (the single block-write formula).
	WritableScopes []string
	// ApiKeyID is the metering identity of the caller and the documented hook
	// point for the two REST-only gates (E03-5 B, see the file header): the
	// throttle that CheckRateLimit counts per api_key_id and the "write" row
	// LogAccess books. The core reads neither — a transport that meters runs
	// its gate around the core call and carries the key here so the decision
	// stays visible at the core boundary instead of being re-derived.
	ApiKeyID string
}

// errIssueRegistryUnavailable is the core's registry-nil sentinel. The block-type
// registry is transport-resolved (h.issueSet / cfg.issueSet, each with its own
// WARN line), but the fail-closed VERDICT belongs to the core — a nil set means
// no policy data, so no workflow status may be invented. The two mappers render
// it in their own envelope, byte-for-byte what the three transports answered
// before this file existed: 503 {"success":false,"error":"type registry
// unavailable"} on REST/manage, errResult("type registry unavailable") on MCP.
var errIssueRegistryUnavailable = errors.New("type registry unavailable")

// issueCreateCore inserts an issue. It resolves the initial workflow status from
// policy DATA (set.WorkflowInitial) and validates a caller-supplied wantStatus as
// an ENTRY transition ("" → wantStatus), so swapping the registry status set
// changes the verdict with no Go rebuild (design/03 §4.3). f.Scope and f.Status
// are FILLED here from env and policy — a caller-set value in those two fields is
// overwritten, which is what keeps the scope decision at the env boundary.
func issueCreateCore(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set,
	env issueWriteEnv, f store.IssueFields, wantStatus string) (*store.Block, error) {
	if set == nil {
		return nil, errIssueRegistryUnavailable
	}
	status := set.WorkflowInitial(store.IssueTypeName)
	if wantStatus != "" {
		if err := set.ValidateTransition(store.IssueTypeName, "", wantStatus); err != nil {
			return nil, err
		}
		status = wantStatus
	}
	f.Scope = env.Scope
	f.Status = status
	return issueTx(ctx, pool, func(tx pgx.Tx) (*store.Block, error) {
		return store.InsertIssueBlock(ctx, tx, f, env.WritableScopes)
	})
}

// issueUpdateCore applies a by-id field/state change. The transition itself is
// validated inside store.UpdateIssueBlock against the same policy set, so the
// core only has to guarantee that a set EXISTS (an update with a nil registry
// could not validate a status change and must not fall through). The MCP
// issue_state tool fills exactly one field of u (Status) — that is the cut of the
// tool surface, not a missing feature (design/03 §4.10 "issue_state is no
// update").
func issueUpdateCore(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set,
	env issueWriteEnv, blockID string, u store.IssueUpdate) (*store.Block, error) {
	if set == nil {
		return nil, errIssueRegistryUnavailable
	}
	return issueTx(ctx, pool, func(tx pgx.Tx) (*store.Block, error) {
		return store.UpdateIssueBlock(ctx, tx, blockID, u, set, env.WritableScopes)
	})
}

// issueCommentCore appends a comment to the issue parentID. It takes NO
// *blocktype.Set and carries no registry guard, because a comment has no workflow
// status: none of the three transports consults the registry on this path today,
// and adding a guard here would invent a 503 no wire ever answered. The store
// forces the comment into the PARENT's scope and re-asserts env.WritableScopes in
// the same transaction, so env.Scope is deliberately unused on this path.
func issueCommentCore(ctx context.Context, pool *pgxpool.Pool,
	env issueWriteEnv, parentID string, f store.CommentFields) (*store.Block, error) {
	return issueTx(ctx, pool, func(tx pgx.Tx) (*store.Block, error) {
		return store.InsertCommentBlock(ctx, tx, parentID, f, env.WritableScopes)
	})
}
