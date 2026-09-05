package handler

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The two search transports (REST /api/search, MCP `search`) now build ONE
// argument set (search_core.go). These tests hold the two mappings against each
// other without a database, because the one way the surfaces can still drift is
// a field one of them forgets to fill — and a forgotten field is silent: the
// caller gets an answer, just not the one it asked for.

// t039Body is one request in the wire vocabulary BOTH surfaces accept: the
// canonical exclude name, the legacy alias, and a positive type filter.
const t039Body = `{"query":"frage","category":"learnings","tags":["a","b"],` +
	`"types":["knowledge"],"types_exclude":["audit-trail"],` +
	`"block_roles_exclude":["synthesis","audit-trail"],` +
	`"cluster":"11111111-2222-4333-8444-555555555555"}`

// TestSearchTransportsAgreeOnTheAliasUnion is the parity gate of E03-3 = A.
//
// Before T03-9 the MCP `search` tool dropped block_roles_exclude entirely: the
// same body that narrowed a REST search came back UNFILTERED from the tool, with
// no error. The union now lives in searchArgs.effectiveTypesExclude, so a
// transport can only diverge by not filling the field — which is exactly what
// this test reads.
//
// Mutation probe (documented): delete `BlockRolesExclude:` from either coreArgs
// and this test goes red; delete the fold from effectiveTypesExclude and it goes
// red for both surfaces at once.
func TestSearchTransportsAgreeOnTheAliasUnion(t *testing.T) {
	var rest searchRequest
	if err := json.Unmarshal([]byte(t039Body), &rest); err != nil {
		t.Fatalf("decode REST body: %v", err)
	}
	var tool searchInput
	if err := json.Unmarshal([]byte(t039Body), &tool); err != nil {
		t.Fatalf("decode MCP arguments: %v", err)
	}

	restArgs := rest.coreArgs(10, true, nil, true)
	toolArgs := tool.coreArgs(true)

	// Order-preserving union, deduped: the alias's repeated "audit-trail" folds
	// into the canonical one rather than binding twice.
	want := []string{"audit-trail", "synthesis"}
	if got := restArgs.effectiveTypesExclude(); !reflect.DeepEqual(got, want) {
		t.Errorf("REST effective types_exclude = %v, want %v", got, want)
	}
	if got := toolArgs.effectiveTypesExclude(); !reflect.DeepEqual(got, want) {
		t.Errorf("MCP effective types_exclude = %v, want %v (the tool ignored the alias before T03-9)", got, want)
	}

	// Everything the two surfaces are supposed to read identically off the wire.
	for _, f := range []struct {
		name       string
		rest, tool any
	}{
		{"Query", restArgs.Query, toolArgs.Query},
		{"Category", restArgs.Category, toolArgs.Category},
		{"Cluster", restArgs.Cluster, toolArgs.Cluster},
		{"Tags", restArgs.Tags, toolArgs.Tags},
		{"Types", restArgs.Types, toolArgs.Types},
		{"TypesExclude", restArgs.TypesExclude, toolArgs.TypesExclude},
		{"BlockRolesExclude", restArgs.BlockRolesExclude, toolArgs.BlockRolesExclude},
		{"effective", restArgs.effectiveTypesExclude(), toolArgs.effectiveTypesExclude()},
	} {
		if !reflect.DeepEqual(f.rest, f.tool) {
			t.Errorf("%s diverges: REST %#v, MCP %#v", f.name, f.rest, f.tool)
		}
	}
}

// TestSearchAliasAloneBindsLikeTheCanonicalName: a caller who sends ONLY the
// legacy name must get the same filter as one who sends only the canonical one —
// that is what "documented alias" means. unionExcludes returns the non-empty
// side untouched, so the bind parameter is identical, duplicates included.
func TestSearchAliasAloneBindsLikeTheCanonicalName(t *testing.T) {
	canonical := searchArgs{TypesExclude: []string{"audit-trail", "audit-trail"}}
	alias := searchArgs{BlockRolesExclude: []string{"audit-trail", "audit-trail"}}
	if !reflect.DeepEqual(canonical.effectiveTypesExclude(), alias.effectiveTypesExclude()) {
		t.Fatalf("alias-only %v != canonical-only %v",
			alias.effectiveTypesExclude(), canonical.effectiveTypesExclude())
	}
	// Neither name present ⇒ nil, i.e. no filter conjunct at all (pre-alias
	// bytes on the wire to the store layer).
	if got := (searchArgs{}).effectiveTypesExclude(); got != nil {
		t.Fatalf("no exclude fields ⇒ %#v, want nil (no filter conjunct)", got)
	}
}

// TestSearchTypeStrictnessStaysATransportProperty pins the ONE documented
// difference between the two surfaces (V-W6, design/03 §4.9): on the tool
// surface `types` CUTS against the caller's retrieval-visible set and an
// unknown/non-visible name is a refusal; on the REST browse route the same name
// is a pure opt-in bind parameter and retrieval-excluded types stay browseable
// (D5). It is a field, not a hidden branch — and it must not flip by accident in
// either direction: flipping it on would take browseability away from operators,
// flipping it off would widen a model's retrieval surface without an admin gate.
func TestSearchTypeStrictnessStaysATransportProperty(t *testing.T) {
	if (searchRequest{}).coreArgs(10, true, nil, false).VisibleTypesOnly {
		t.Error("REST /api/search must NOT cut against the visible set (D5 browse asymmetry)")
	}
	if !(searchInput{}).coreArgs(false).VisibleTypesOnly {
		t.Error("the MCP search tool MUST cut against the visible set (V-W6)")
	}
	// The tool answers compact rows and never paginates; both are surface
	// properties, not caller choices.
	toolArgs := (searchInput{}).coreArgs(false)
	if !toolArgs.Compact || toolArgs.After != nil {
		t.Errorf("tool args = compact %t, after %v; want compact true, after nil", toolArgs.Compact, toolArgs.After)
	}
}

// TestDefaultSearchLimitHasNoCap: the shared default is 10; the cap is NOT
// shared. E03-2 = B (K28) leaves the tool surface unbounded, so a cap inside the
// core would silently narrow it — the REST 50 lives in HandleSearch and only
// there.
func TestDefaultSearchLimitHasNoCap(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{0, 10}, {-1, 10}, {1, 1}, {50, 50}, {51, 51}, {999, 999},
	} {
		if got := defaultSearchLimit(c.in); got != c.want {
			t.Errorf("defaultSearchLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
	// The REST transport applies the cap on top of the same default.
	if got := (searchRequest{Limit: 999}).coreArgs(50, true, nil, false).Limit; got != 50 {
		t.Errorf("REST args carry limit %d, want the capped 50", got)
	}
	if got := (searchInput{Limit: 999}).coreArgs(false).Limit; got != 999 {
		t.Errorf("tool args carry limit %d, want the uncapped 999 (E03-2 = B)", got)
	}
}

// TestClusterFacetGateIsOneRule: the first stage of the C6 gate (flag, then
// value) stood character for character in both transports before T03-9. Now it
// is one function, and while the flag is off the field does not exist for the
// request — not rejected, not echoed, byte-identical to the time before C6.
func TestClusterFacetGateIsOneRule(t *testing.T) {
	const handle = "11111111-2222-4333-8444-555555555555"
	if got := clusterFacetOf(false, handle); got != nil {
		t.Errorf("facet off with a handle ⇒ %v, want nil (dark state ignores the field)", *got)
	}
	if got := clusterFacetOf(true, ""); got != nil {
		t.Errorf("facet on without a handle ⇒ %v, want nil", *got)
	}
	got := clusterFacetOf(true, handle)
	if got == nil || *got != handle {
		t.Fatalf("facet on with a handle ⇒ %v, want %q", got, handle)
	}
}

// TestSearchRejectedCarriesItsProse: the core's caller errors are a typed
// sentinel class, NOT a *writeReject — errcode.go keeps rejectClass on the write
// entry points, and inventing a read class here would take the errcode sweep
// (E03-7) in the direction it explicitly does not recommend. The transports
// render the prose unchanged, so the text has to survive the trip.
func TestSearchRejectedCarriesItsProse(t *testing.T) {
	if errSearchClusterForm.Error() != "cluster must be a full UUID" {
		t.Fatalf("cluster rejection prose = %q, want the byte-identical text both surfaces sent before", errSearchClusterForm.Error())
	}
	var err error = &searchRejected{msg: `unknown block type "nope"`}
	if err.Error() != `unknown block type "nope"` {
		t.Fatalf("rejection prose = %q", err.Error())
	}
}
