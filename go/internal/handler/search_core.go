package handler

import (
	"context"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The ONE rule set of the block search (T03-9, design/03 §4.9).
//
// Two transports serve the same surface — REST POST /api/search
// (context_search.go) and the MCP `search` tool (mcp.go) — and until this file
// existed each carried its own copy of four rules. Three of the four had
// drifted apart, and each drift was invisible from the other side:
//
//   - GRANTS: the MCP arm resolved the caller-tenant block grants and threaded
//     them into the visibility OR; REST passed a literal nil (T40a wired four
//     paths, /api/search was not one of them). A block a tenant had been
//     granted was therefore readable through the model's door and NOT through
//     the operator's — one authorisation answer per transport for one block.
//     E03-1 = A closes that: the grant set is resolved HERE, so a transport can
//     no longer forget it. The per-request bound of T04-20 is what makes this
//     affordable on the highest-frequency read surface.
//   - ALIAS: `block_roles_exclude` is the documented legacy alias of
//     `types_exclude` (seam 17). REST unioned it, MCP `recent` unions it, MCP
//     `search` ignored it — a client that sent the alias to the search tool got
//     an unfiltered answer with no error, i.e. MORE than it asked for. E03-3 = A
//     makes the union a property of the shared argument set (additive; nobody
//     who sends the canonical name notices).
//   - FACET: the C6 cluster gate (flag, then form, never an existence check)
//     stood twice, character for character.
//
// The fourth rule, the LIMIT, deliberately stays split (E03-2 = B / K28): REST
// caps at 50, the MCP tool does not cap at all. Only the default (10) is shared,
// as defaultSearchLimit below — a cap in here would silently narrow a surface
// the decision says to leave unbounded.
//
// What stays in the transports, by design: decoding, the read rate-limit
// preamble (T03-6), the config snapshot (see FacetEnabled), the response
// envelope, and the rendering of the rejections this file returns. The core
// never touches http.ResponseWriter and never reads config.

// searchArgs is one search request in the vocabulary both transports share.
// Every field that differs between them is a FIELD, not a branch on "who is
// calling": a documented difference stays visible and greppable, an implicit
// one does not.
type searchArgs struct {
	Query    string
	Category string
	// Cluster is the raw C6 handle as the caller sent it. Whether it is a
	// filter at all is decided by FacetEnabled below, never here.
	Cluster string
	Tags    []string
	Types   []string
	// TypesExclude and BlockRolesExclude are the canonical name and its legacy
	// alias. They stay SEPARATE in the argument set and are folded exactly once,
	// in effectiveTypesExclude — a transport that folded them itself would be
	// free to fold them differently, which is the drift this wave removes.
	TypesExclude      []string
	BlockRolesExclude []string
	Limit             int
	Compact           bool
	After             *store.SearchCursor
	// VisibleTypesOnly is the MCP-only strictness of `types` (V-W6,
	// mcp.go:111-129): on the tool surface a type name CUTS against the
	// caller's retrieval-visible set and an unknown or non-visible name is a
	// refusal, while on the REST browse route the same name is a pure opt-in
	// bind parameter and retrieval-excluded types stay browseable (D5). That is
	// a documented asymmetry, not a bug, so it is carried as a field rather
	// than levelled — levelling it would either widen the model's retrieval
	// surface or take browseability away from the operator.
	VisibleTypesOnly bool
	// FacetEnabled is the cluster.facet_enabled flag AS A VALUE, taken by the
	// transport from the ONE config snapshot it already holds for the request
	// (context_search.go: "ONE config snapshot per request: the rate limit and
	// the C6 facet gate must never come from two different generations"). The
	// core must not read the config store itself: it would either skip the gate
	// or evaluate it against a SECOND generation, and a request whose rate
	// limit came from generation N while its facet gate came from N+1 is
	// exactly the split this comment forbids. Pinned by
	// TestSearchCoreReadsNoConfigSnapshot.
	FacetEnabled bool
}

// effectiveTypesExclude folds the canonical exclude list with its legacy alias
// (E03-3 = A). unionExcludes is order-preserving, deduping, monotone-restrictive
// and returns the OTHER list untouched when one side is empty — so a caller
// that never heard of the alias binds byte-identically to before, duplicates
// included.
func (a searchArgs) effectiveTypesExclude() []string {
	return unionExcludes(a.TypesExclude, a.BlockRolesExclude)
}

// searchRejected is a caller error the core already holds the prose for. It is
// a typed sentinel and deliberately NOT one of the coded write rejections:
// errcode.go limits the rejectClass vocabulary to the WRITE entry points ("Read
// handlers … keep their uncoded envelopes; coding them is a separate sweep, not
// this one"), and inventing a read class here would take that sweep (E03-7) in
// the direction it explicitly does not recommend. Both transports render it as
// they always did — REST as a 400 with the text, MCP as an errResult with the
// text.
type searchRejected struct{ msg string }

func (e *searchRejected) Error() string { return e.msg }

// errSearchClusterForm is the ONE prose for a malformed facet handle. Same text
// both transports emitted before this file existed.
var errSearchClusterForm = &searchRejected{msg: "cluster must be a full UUID"}

// defaultSearchLimit is the shared default of both surfaces: absent or
// non-positive means 10. The REST CAP (50) is not here on purpose — K28 keeps
// the handler side of the MCP tool uncapped (E03-2 = B), and a cap in the shared
// core would be a silent narrowing of that decision.
//
// Measured caveat, so nobody reads more freedom into this than exists: the
// STATEMENT builder clamps anyway — store.searchBlocksSQL runs
// ClampLimit(limit, 10, 50) over every caller (store/blocks.go), and it did so
// before this wave. The tool surface is therefore already bounded at 50 ROWS in
// the store; what E03-2 = B keeps open is the handler layer only. The REST cap is
// still needed here and not redundant: /api/search ECHOES its effective limit
// and derives the next-page cursor from it, so the two would drift apart if the
// route left the clamping to the store.
func defaultSearchLimit(limit int) int {
	if limit <= 0 {
		return 10
	}
	return limit
}

// clusterFacetOf is the FIRST stage of the C6 gate: the flag decides whether the
// field exists for this request at all. While the flag is off the value is
// ignored COMPLETELY — not rejected, not echoed — so the dark state stays
// byte-identical to the time before C6, when an unknown JSON key was simply
// dropped by the decoder. It is a named function because the REST envelope has
// to answer the same question a second time (it echoes the facet only when it
// was APPLIED), and two hand-written copies of "flag AND value" are exactly how
// the two transports drifted apart in the first place.
func clusterFacetOf(enabled bool, cluster string) *string {
	if !enabled || cluster == "" {
		return nil
	}
	return &cluster
}

// executeSearch runs one search. The order of its four steps is load-bearing and
// is the order both transports had before:
//
//  1. the type filter, ahead of every pool touch — a rejected filter must not
//     cost a grant lookup or a search statement;
//  2. the facet FORM, before any DB roundtrip (pattern handler/graph.go
//     fullUUIDRe). Without it the value reaches `$n::uuid` and comes back as
//     SQLSTATE 22P02, i.e. a 500 that tells the caller "this was not a handle".
//     Never an existence check: a well-formed handle is ALWAYS a 200 with a
//     possibly empty list, because unknown, foreign and member-less have to be
//     one answer or the handle space becomes enumerable (design/03 §5.7);
//  3. the grant set, refused loudly when it is over the per-request bound
//     (T04-20: degrading to scope-only would hide every granted block silently
//     and for as long as the condition lasts);
//  4. the statement.
//
// It takes *auth.AuthResult and does NOT check it: the validity wache stays a
// transport concern (REST answers 401 with its envelope, MCP fails closed with
// its own prose), so it can be changed once per transport rather than once per
// tool. ar must be non-nil — both transports return before this point when it
// is not.
//
// The three error classes it returns are distinguishable by the caller:
// *searchRejected (caller error, prose included), store.ErrTooManyBlockGrants
// (structural refusal, rendered with tooManyGrantsMsg), and the raw store error
// (server error). The store error is returned UNWRAPPED so the MCP surface can
// keep printing "search failed: %v" byte-for-byte.
func executeSearch(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, ar *auth.AuthResult, a searchArgs) ([]store.BlockPreview, error) {
	types := a.Types
	if a.VisibleTypesOnly {
		cut, rejection := resolveMCPVisibleTypes(set, a.Types)
		if rejection != "" {
			return nil, &searchRejected{msg: rejection}
		}
		types = cut
	}

	facet := clusterFacetOf(a.FacetEnabled, a.Cluster)
	if facet != nil && !fullUUIDRe.MatchString(*facet) {
		return nil, errSearchClusterForm
	}

	// resolveGrants is fail-closed-and-quiet for a transient resolver failure
	// (log, empty set, read proceeds scope-only) and returns exactly one error
	// class, store.ErrTooManyBlockGrants — which is why passing it up untouched
	// is safe: there is no second class for a caller to mistake for it.
	grants, err := resolveGrants(ctx, pool, ar)
	if err != nil {
		return nil, err
	}

	return store.SearchBlocks(ctx, pool, set, a.Query, ar.ReadScopes, a.Category, a.Tags,
		defaultSearchLimit(a.Limit), a.Compact, a.After, grants, types, a.effectiveTypesExclude(), facet)
}
