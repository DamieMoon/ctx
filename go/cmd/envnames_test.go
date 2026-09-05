// The tooling env-name fence (design/06 §4.7, masterplan K10/A-12).
//
// This file is the SECOND caller of the one env-name scanner that T05-6 built
// in internal/config; the first is the server-runtime gate
// (internal/config/envonly_test.go). Same scanner, two parameter sets: there
// the ctxd package closure with the three classified name sets, here the
// cmd/ctx-* packages with the tooling budget and a wider name pattern.
//
// Why a test file in a directory that holds no package: cmd/ itself is only a
// container of binaries, and the fence is about the SET of those binaries —
// putting it inside one of them would make that binary the owner of a rule
// that concerns its six siblings. `go test ./cmd/` runs it, `go vet` and
// golangci-lint see it, and `go build ./...` ignores a test-only directory.
//
// The fence has two halves, because a name can reach a process two ways:
//
//  1. every env-name-shaped string LITERAL in cmd/ctx-* has to stand in the
//     budget — either as a registered config name or with a written reason;
//  2. every env read whose name is NOT a literal (os.Getenv(key) with a
//     variable) has to stand in a named exception list, because the first
//     half is blind to it.
//
// Half 2 is deliberately not a syntax pin of the reading call (an os.Getenv
// pin is bypassed by os.LookupEnv or by a helper, as
// internal/config/envonly_test.go:200-206 states for the server runtime): it
// is a list of the FILES that may read a computed name at all. Today it has
// exactly one entry, the dsnFromProcessEnv get-closure of ctx-goldset, whose
// names all appear as literals two lines above it and are budgeted there.
package cmd

import (
	"go/ast"
	"go/build"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
)

const envScanModulePath = "github.com/GottZ/ctx"

// toolingNamePattern is the server pattern of internal/config widened by the
// one prefix that exists only out here: GOLDBENCH_API_KEY
// (cmd/ctx-goldbench/main.go). It is anchored the same way — a literal that
// IS an env name, never one that merely contains one.
var toolingNamePattern = regexp.MustCompile(`^(CTX|CONTEXT|GOLDBENCH)_[A-Z0-9_]+$`)

// toolingBudget lists every env name of cmd/ctx-* that is NOT in the config
// registry, each with the reason it may bypass it. The reason is the entire
// content of an entry: a name without one has been silenced, not budgeted.
var toolingBudget = map[string]string{
	"CTX_GOLDSET_DIR": "the gold directory lives in the private .project submodule and is a workstation path, " +
		"not a server setting — no ctxd process reads it (cmd/ctx-goldset defaultGoldDir, cmd/ctx-armsweep)",
	"CTX_ARMSWEEP_REPORT_DIR": "the sweep report target is a workstation path of the operator running the sweep, " +
		"the same class as CTX_GOLDSET_DIR (cmd/ctx-armsweep)",
	"GOLDBENCH_API_KEY": "the bench client authenticates against a REMOTE ctx instance; the key is the caller's " +
		"credential, not a setting of the instance being measured (cmd/ctx-goldbench resolveAPIKey, flag > env > empty)",
	"CONTEXT_GOLDSET_DB_HOST": "retired tool-local second name for CONTEXT_DB_HOST, kept as a fail-closed tombstone: " +
		"reading CONTEXT_DB_HOST instead would silently connect elsewhere (cmd/ctx-goldset dsnFromProcessEnv)",
}

// nonLiteralEnvReaders names the files that may read an env name that is not
// a string literal, with the reason. Every other such read is a finding —
// that is the half of the fence the literal budget cannot see.
var nonLiteralEnvReaders = map[string]string{
	"cmd/ctx-goldset/main.go": "the dsnFromProcessEnv get-closure takes the name as a parameter; all five names " +
		"it is called with are literals in the same function and stand in the budget above",
}

// toolPackages returns the cmd/ctx-* packages — the scan area of this fence.
// It is a directory sweep rather than a fixed list on purpose: a new tooling
// binary joins the fence by existing, not by being remembered here.
func toolPackages(t *testing.T) []config.ScanPackage {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	cmdDir := filepath.Join(root, "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		t.Fatalf("read %s: %v", cmdDir, err)
	}
	var out []config.ScanPackage
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "ctx-") {
			continue
		}
		dir := filepath.Join(cmdDir, entry.Name())
		if _, err := build.ImportDir(dir, 0); err != nil {
			t.Fatalf("resolve package cmd/%s: %v", entry.Name(), err)
		}
		out = append(out, config.ScanPackage{ImportPath: envScanModulePath + "/cmd/" + entry.Name(), Dir: dir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImportPath < out[j].ImportPath })
	// A lower bound, not a fixed count: the tool set grows and shrinks with
	// legitimate work (6 on 2026-09-05), but a sweep that lost the directory
	// collapses to nothing and would make every gate below vacuously green.
	if len(out) < 5 {
		t.Fatalf("tooling scan area has only %d packages — the cmd/ctx-* sweep lost the tree", len(out))
	}
	return out
}

// nonTestGoFiles lists the .go files of one directory, test files excluded —
// the same file set config.ScanEnvNames walks, so both halves of the fence
// judge the same code.
func nonTestGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out
}

// budgetedNames is the registry plus the budget: what a literal may be.
func budgetedNames() map[string]bool {
	out := map[string]bool{}
	for _, name := range config.EnvVars() {
		out[name] = true
	}
	for name := range toolingBudget {
		out[name] = true
	}
	return out
}

// TestToolingScanAreaIsCtxStar pins the scan area itself. Without it the
// gates below could pass by scanning nothing, or fail with the wrong subject
// by scanning the server (cmd/ctxd) or the CLI (cmd/ctx), which are covered
// by the registry and by the T05-6 gate instead.
func TestToolingScanAreaIsCtxStar(t *testing.T) {
	paths := map[string]bool{}
	for _, pkg := range toolPackages(t) {
		paths[pkg.ImportPath] = true
	}
	for _, want := range []string{"cmd/ctx-goldset", "cmd/ctx-goldbench", "cmd/ctx-armsweep", "cmd/ctx-armcost"} {
		if !paths[envScanModulePath+"/"+want] {
			t.Errorf("tooling scan area misses %s — its env names would be unfenced", want)
		}
	}
	for _, unwanted := range []string{"cmd/ctxd", "cmd/ctx"} {
		if paths[envScanModulePath+"/"+unwanted] {
			t.Errorf("tooling scan area contains %s — the server runtime has its own gate "+
				"(internal/config/envonly_test.go) and a second one here would judge it by the wrong budget", unwanted)
		}
	}
}

// TestEveryToolingEnvNameIsBudgeted is the tripwire: a new env name in a
// tooling binary is a decision with one line in toolingBudget, not a line
// without a decision.
func TestEveryToolingEnvNameIsBudgeted(t *testing.T) {
	pkgs := toolPackages(t)
	start := time.Now()
	refs, err := config.ScanEnvNames(pkgs, budgetedNames(), config.WithNamePattern(toolingNamePattern))
	if err != nil {
		t.Fatalf("scan tooling packages: %v", err)
	}
	t.Logf("scanned %d packages in %s", len(pkgs), time.Since(start).Round(time.Millisecond))
	for _, ref := range refs {
		t.Errorf("unbudgeted tooling env name %s — register it (internal/config) or add it to "+
			"toolingBudget with the reason it may bypass the registry", ref)
	}
}

// TestEveryBudgetEntryCarriesAReasonAndACallSite keeps the budget from
// degenerating into a name list in two directions: an entry without a reason
// is a silencing, and an entry whose name no longer appears anywhere in
// cmd/ctx-* is a stale permission that would cover the NEXT use of that name.
func TestEveryBudgetEntryCarriesAReasonAndACallSite(t *testing.T) {
	refs, err := config.ScanEnvNames(toolPackages(t), nil, config.WithNamePattern(toolingNamePattern))
	if err != nil {
		t.Fatalf("scan tooling packages: %v", err)
	}
	present := map[string]bool{}
	for _, ref := range refs {
		present[ref.Name] = true
	}
	for name, reason := range toolingBudget {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is budgeted without a reason — say why it may bypass the config registry", name)
		}
		if !present[name] {
			t.Errorf("%s is budgeted but appears in no cmd/ctx-* literal any more — "+
				"drop the entry in the commit that removed its call site", name)
		}
	}
	t.Logf("%d distinct env literals across the tooling packages, %d budget entries", len(present), len(toolingBudget))
}

// TestEveryNonLiteralEnvReadIsNamed is the second half of the fence: a name
// assembled at runtime never becomes a literal, so the budget above cannot
// see it. Both stdlib entry points count — pinning only os.Getenv would be
// bypassed by os.LookupEnv, which is the reason the server gate pins a SET of
// readers rather than a call syntax (internal/config/envonly_test.go:200-206).
// The package qualifier is resolved through the IMPORT PATH, not through the
// name "os", so an aliased import is caught too.
func TestEveryNonLiteralEnvReadIsNamed(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	fset := token.NewFileSet()
	files := 0
	for _, pkg := range toolPackages(t) {
		for _, abs := range nonTestGoFiles(t, pkg.Dir) {
			files++
			file, err := goparser.ParseFile(fset, abs, nil, goparser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", abs, err)
			}
			rel, err := filepath.Rel(root, abs)
			if err != nil {
				t.Fatalf("relative path of %s: %v", abs, err)
			}
			// Slash-normalised, and it stays that way: the only consumers of
			// rel are the map key of nonLiteralEnvReaders (written with
			// slashes), the failure message, and the FromSlash round trip in
			// TestNonLiteralExceptionsPointAtRealFiles. No filepath.Dir or
			// filepath.Base is applied to it — that pair is the Windows trap
			// (filepath.Dir would hand back cmd\ctx-goldset).
			rel = filepath.ToSlash(rel)
			for _, pos := range nonLiteralEnvReads(fset, file) {
				if _, ok := nonLiteralEnvReaders[rel]; ok {
					continue
				}
				t.Errorf("%s:%d reads an env name that is not a literal — the name budget cannot see it; "+
					"pass a literal, or name the file in nonLiteralEnvReaders with the reason", rel, pos.Line)
			}
		}
	}
	t.Logf("checked %d non-test files, %d named exception(s)", files, len(nonLiteralEnvReaders))
}

// TestNonLiteralExceptionsPointAtRealFiles keeps the exception list from
// outliving its files — a stale entry would silently permit a future
// computed read in a path that once had a reason.
func TestNonLiteralExceptionsPointAtRealFiles(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	for rel, reason := range nonLiteralEnvReaders {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is exempt without a reason — say why the name may be computed there", rel)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s is exempt but does not exist: %v", rel, err)
		}
	}
}

// nonLiteralEnvReads returns the positions of os.Getenv/os.LookupEnv calls in
// one file whose name argument is not a string literal.
func nonLiteralEnvReads(fset *token.FileSet, file *ast.File) []token.Position {
	names := importedAs(file, "os")
	if len(names) == 0 {
		return nil
	}
	var out []token.Position
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || !names[ident.Name] {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			return true
		}
		out = append(out, fset.Position(call.Pos()))
		return true
	})
	return out
}

// importedAs returns the local names under which one import path is bound in
// a file — the qualifier resolution both fences use, so a renamed import
// cannot walk past a gate.
func importedAs(file *ast.File, importPath string) map[string]bool {
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
