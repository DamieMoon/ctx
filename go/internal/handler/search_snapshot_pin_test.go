package handler

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"testing"
)

// The pin under the ONE-snapshot rule of the search surfaces (T03-9,
// design/03 §4.9).
//
// context_search.go states the rule the whole wave rests on: "ONE config
// snapshot per request: the rate limit and the C6 facet gate must never come
// from two different generations." A request whose rate limit was decided by
// generation N and whose facet gate was decided by N+1 is not a consistent
// answer, and nothing in the type system stops a future reader from taking a
// second snapshot — the config store is reachable from both transports.
//
// The shared core therefore receives the flag as a VALUE (searchArgs.
// FacetEnabled) and must not be able to look it up itself. That is the
// negative probe this file automates: give search_core.go a ConfigStore and
// read the flag inside it, and this test goes red before any behaviour test
// notices — the second snapshot usually agrees with the first, so a race that
// only shows up under a config reload would otherwise ship green.
//
// Why an AST walk and not a grep: comments in these files legitimately talk
// about the snapshot ("out of the ONE snapshot this request took above"), and a
// text pin would count the prose that explains the rule as a violation of it.
// Parsing without ParseComments keeps comments out of the tree entirely.
//
// Known limit, stated rather than hidden: this is a SYNTAX pin over three named
// files. It sees any selector call named SnapshotForRequest/SnapshotForTenant/
// Snapshot and any mention of the ConfigStore type inside the core, but a
// detour through a helper in a fourth file leaves nothing here to match. The
// same trade internal/config/contractmode_pin_test.go makes deliberately.

// t039SnapshotSelectors are the ConfigStore/Registry accessors that produce a
// per-request generation.
var t039SnapshotSelectors = map[string]bool{
	"Snapshot":           true,
	"SnapshotForRequest": true,
	"SnapshotForTenant":  true,
}

func t039Parse(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, path, nil, goparser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, f
}

// t039SnapshotCalls lists the source positions of snapshot accessor calls under
// node.
func t039SnapshotCalls(fset *token.FileSet, node ast.Node) []string {
	var out []string
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && t039SnapshotSelectors[sel.Sel.Name] {
			out = append(out, fset.Position(sel.Sel.Pos()).String()+" "+sel.Sel.Name)
		}
		return true
	})
	return out
}

// TestSearchCoreReadsNoConfigSnapshot: the shared core takes the C6 flag as a
// value and has no way to ask the config store for a second opinion.
func TestSearchCoreReadsNoConfigSnapshot(t *testing.T) {
	fset, f := t039Parse(t, "search_core.go")

	if calls := t039SnapshotCalls(fset, f); len(calls) != 0 {
		t.Errorf("search_core.go reads a snapshot itself (%v) — FacetEnabled must arrive as a value from the transport's ONE snapshot", calls)
	}

	// Not even the type may be named here: a ConfigStore parameter or field is
	// the door through which the second snapshot walks in.
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "ConfigStore" {
			t.Errorf("search_core.go names ConfigStore at %s — the core must not be able to read config", fset.Position(id.Pos()))
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cfg" {
			t.Errorf("search_core.go reaches for MCPConfig.Cfg at %s — same door, same rule", fset.Position(sel.Sel.Pos()))
		}
		return true
	})
}

// TestSearchTransportsTakeExactlyOneSnapshot: the other half of the rule. Each
// transport reads the per-request config generation once and hands the derived
// flag on; two reads in one request path are the split the prose forbids.
//
// The registry snapshot (h.typeSnapshot / cfg.mcpTypeSnapshot) is a different
// generation with its own one-per-request rule and is deliberately NOT counted
// here — it is reached through those two named methods, not through a Snapshot*
// selector in these bodies.
func TestSearchTransportsTakeExactlyOneSnapshot(t *testing.T) {
	for _, c := range []struct{ file, fn string }{
		{"context_search.go", "HandleSearch"},
		{"mcp.go", "mcpSearchHandler"},
	} {
		fset, f := t039Parse(t, c.file)
		var fn *ast.FuncDecl
		for _, d := range f.Decls {
			if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == c.fn {
				fn = d
				break
			}
		}
		if fn == nil {
			t.Fatalf("%s: func %s not found — the pin must be re-aimed, not deleted", c.file, c.fn)
		}
		if calls := t039SnapshotCalls(fset, fn); len(calls) != 1 {
			t.Errorf("%s: %s takes %d config snapshots %v, want exactly 1", c.file, c.fn, len(calls), calls)
		}
	}
}
