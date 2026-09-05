// The Bench* seam (design/06 §4.7, masterplan K18/A-15).
//
// Four production packages — dream, llm, rrf, topiclabel — each export a
// bench_exports.go shim file whose only purpose is to hand an unexported
// prompt builder or parser to the gold bench (22 symbols on 2026-09-05, all
// 25 call sites in internal/goldbench). The shims are NOT build-tagged, so
// they are part of the public surface of every ctx and ctxd build; the only
// thing that keeps them a bench seam rather than a second, unversioned API of
// those four packages is that nobody else calls them.
//
// That "nobody else" was a convention until this file. The gate resolves the
// package qualifier through the IMPORT PATH rather than through the spelling
// of the identifier, so an aliased import (tl "…/internal/topiclabel") is
// caught the same way, and it walks the AST rather than the file text, so a
// mention in a comment or a string is not a call.
package goldbench_test

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const benchSeamModulePath = "github.com/GottZ/ctx"

// benchShimPackages are the four packages whose Bench* surface belongs to the
// gold bench alone.
var benchShimPackages = []string{
	benchSeamModulePath + "/internal/dream",
	benchSeamModulePath + "/internal/llm",
	benchSeamModulePath + "/internal/rrf",
	benchSeamModulePath + "/internal/topiclabel",
}

// benchSeamOwner is the one package the shims exist for; its own directory is
// the single exception of the sweep below.
const benchSeamOwner = "internal/goldbench"

// benchSeamRoots are the two source trees that are swept: everything that is
// compiled into a ctx binary or into a test of one.
var benchSeamRoots = []string{"internal", "cmd"}

// benchRef is one call of a Bench* shim outside its owner.
type benchRef struct {
	File   string
	Line   int
	Pkg    string
	Symbol string
}

// TestBenchShimsAreCalledOnlyByGoldbench is the seam: a Bench* symbol used
// anywhere but in internal/goldbench means one of the four packages has grown
// a second API through the back door.
func TestBenchShimsAreCalledOnlyByGoldbench(t *testing.T) {
	start := time.Now()
	// path.Dir, not filepath.Dir: rel is slash-normalised (scanBenchRefs), and
	// on Windows filepath.Dir would hand back "internal\goldbench", so the
	// owner comparison would fail and the gate would report every living call
	// (same reason as internal/llm/exec_ban_test.go:198).
	refs, files := scanBenchRefs(t, func(rel string) bool { return path.Dir(rel) == benchSeamOwner })
	t.Logf("scanned %d .go files in %s", files, time.Since(start).Round(time.Millisecond))
	// The sweep has to be a sweep: with a broken root the loop below would be
	// vacuously green (1384 files outside the owner package on 2026-09-05).
	if files < 800 {
		t.Fatalf("bench seam sweep saw only %d files — the walk lost the tree", files)
	}
	for _, ref := range refs {
		t.Errorf("%s:%d calls %s.%s outside %s — the Bench* shims of dream, llm, rrf and topiclabel "+
			"exist for the gold bench alone; a second caller makes them an unversioned API of a "+
			"production package (design/06 §4.7)", ref.File, ref.Line, ref.Pkg, ref.Symbol, benchSeamOwner)
	}
}

// TestBenchSeamMatcherSeesTheLivingCalls is the positive control of the gate
// above. A matcher that resolves nothing — wrong import path, wrong selector
// shape — would report zero violations forever; here the SAME matcher is run
// over the owner package, where the calls it must be able to see live.
func TestBenchSeamMatcherSeesTheLivingCalls(t *testing.T) {
	refs, _ := scanBenchRefs(t, func(rel string) bool { return path.Dir(rel) != benchSeamOwner })
	byPkg := map[string]int{}
	for _, ref := range refs {
		byPkg[ref.Pkg]++
	}
	t.Logf("%d Bench* calls inside %s: %v", len(refs), benchSeamOwner, byPkg)
	if len(refs) < 5 {
		t.Fatalf("matcher found only %d Bench* calls in %s — it cannot be trusted to find one elsewhere",
			len(refs), benchSeamOwner)
	}
	if len(byPkg) != len(benchShimPackages) {
		t.Errorf("the gold bench calls %d of the %d shim packages — if a package no longer has a bench "+
			"caller, its bench_exports.go has no reason to exist (that is a finding, not a gate to widen)",
			len(byPkg), len(benchShimPackages))
	}
}

// scanBenchRefs walks the source trees and returns every Bench* call whose
// file passes skip == false, plus the number of files parsed.
func scanBenchRefs(t *testing.T, skip func(rel string) bool) ([]benchRef, int) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	fset := token.NewFileSet()
	var out []benchRef
	files := 0
	for _, sub := range benchSeamRoots {
		// The callback parameter is NOT called path: the package of that name
		// resolves the owner comparison above, and shadowing it here would
		// make a later filepath/path mix-up invisible.
		walkErr := filepath.WalkDir(filepath.Join(root, sub), func(abs string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(abs, ".go") {
				return nil
			}
			rel, err := filepath.Rel(root, abs)
			if err != nil {
				return err
			}
			// Slash-normalised from here on — every consumer of rel (the skip
			// closure, benchRefsIn, the failure message) is path-, not
			// filepath-shaped, so the gate reads the same on every OS.
			rel = filepath.ToSlash(rel)
			if skip(rel) {
				return nil
			}
			files++
			file, err := goparser.ParseFile(fset, abs, nil, goparser.SkipObjectResolution)
			if err != nil {
				return err
			}
			out = append(out, benchRefsIn(fset, file, rel)...)
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", sub, walkErr)
		}
	}
	return out, files
}

// benchRefsIn collects the Bench* selector expressions of one parsed file
// whose qualifier resolves to one of the four shim packages.
func benchRefsIn(fset *token.FileSet, file *ast.File, rel string) []benchRef {
	qualifiers := map[string]string{}
	for _, importPath := range benchShimPackages {
		for name := range benchImportedAs(file, importPath) {
			qualifiers[name] = importPath[strings.LastIndex(importPath, "/")+1:]
		}
	}
	if len(qualifiers) == 0 {
		return nil
	}
	var out []benchRef
	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || !isBenchSymbol(sel.Sel.Name) {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		pkg, ok := qualifiers[ident.Name]
		if !ok {
			return true
		}
		out = append(out, benchRef{File: rel, Line: fset.Position(sel.Pos()).Line, Pkg: pkg, Symbol: sel.Sel.Name})
		return true
	})
	return out
}

// isBenchSymbol reports whether a selector names an exported Bench* symbol —
// "Bench" followed by an upper-case rune, so a field called Benchmark counts
// and one called Benched does not go unnoticed either, while "Bench" alone or
// "Benchmarking" in lower case does not match.
func isBenchSymbol(name string) bool {
	rest, ok := strings.CutPrefix(name, "Bench")
	return ok && rest != "" && rest[0] >= 'A' && rest[0] <= 'Z'
}

// benchImportedAs returns the local names under which one import path is
// bound in a file. Blank and dot imports are skipped: neither can carry a
// qualified call.
func benchImportedAs(file *ast.File, importPath string) map[string]bool {
	out := map[string]bool{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		switch {
		case spec.Name == nil:
			out[importPath[strings.LastIndex(importPath, "/")+1:]] = true
		case spec.Name.Name == "_" || spec.Name.Name == ".":
			continue
		default:
			out[spec.Name.Name] = true
		}
	}
	return out
}
