// The DB-access fence for the operator tooling (design/06 §8 E06-2 = A,
// masterplan A-36).
//
// The rule this pins is one sentence of docs/operations.md: DB-direct tools
// open READ ONLY, and the single writing exception is ctx-distillreset.
//
// E06-2 established that the three DB-direct binaries hold no privilege the
// API lacks — all of them dial with CONTEXT_DB_USER, the very role ctxd uses.
// What actually separates them from the product is a WRITE CONTRACT:
// internal/distillreset/reset.go:278 runs a bare UPDATE ... RETURNING because
// store.UpdateBlock would stamp type_source='manual' and updated_at=now(), and
// reset.go:37-39 says why that stamp would turn the exact reversal of a
// documented retype into a new corpus movement. Entscheid E4-5 ("Retype bleibt
// Mess-Werkzeug", reset.go:11-12) keeps that path outside the product.
//
// A contract that is written down in one package's doc comment is not pinned.
// This file pins it: every DB call in the tool-only graph is either
//
//   - inside a pgxdb.Read/Write closure, where the receiver is a pgx.Tx and
//     the transaction mode was decided at the opener, or
//   - a raw handle call that stands in directDBAccess with the reason.
//
// It is the fourth fence of the run, and it is built like the three of T06-7:
// a directory sweep instead of a remembered list, a reason as the entire
// content of an exception, and a control test so that a matcher which stopped
// resolving fails loudly instead of passing vacuously.
//
// # Two deviations from the briefed shape, both fail-closed
//
//  1. The briefing asks for "the receiver resolves to *pgxpool.Pool". That
//     direction is fail-OPEN: cmd/ctx-armcost/main.go:130 already holds the
//     pool as sess.Pool, a struct field of another package (internal/toolboot),
//     and no local resolver types that expression — sess.Pool.Exec(ctx,
//     "UPDATE ...") would walk straight past such a fence. The burden is
//     therefore inverted: a DB-shaped call is a finding UNLESS its receiver
//     resolves to pgx.Tx. That is still type resolution and not a name match —
//     the allowance is decided by the DECLARED type of the receiver, through
//     the import path of pgx, so a variable called tx that is not a pgx.Tx
//     stays a finding and a pgx.Tx called anything else is allowed.
//
//  2. The fence covers *pgx.Conn as well as *pgxpool.Pool, because
//     internal/goldset holds its handle as a single *pgx.Conn (db.go:13). A
//     fence that a one-word type change walks past is not a fence — the same
//     argument T06-7 used for os.LookupEnv. Inversion (1) delivers this for
//     free: goldset's thirteen sites are findings and stand in the list with
//     the reason that makes them safe (server-side
//     default_transaction_read_only=on, db.go:24).
//
// # Known limit, measured and left open
//
// A METHOD VALUE walks past: `exec := pool.Exec` binds the method without
// calling it, and the later `exec(ctx, "UPDATE …")` is a call on a plain
// identifier, not a selector. Catching it needs the := right-hand side to be
// typed, which is the inference this file deliberately does not do (see
// deviation 1 — the fence resolves the ALLOWANCE by type and reports
// everything else, rather than trying to type every expression). There is no
// such binding in the tool-only graph today; if one appears, the rule needs a
// selector-valued-identifier pass, not a patch here.
//
// # What is deliberately NOT in the scan area
//
// internal/llmlog. cmd/ctx-llmlog-export runs its export through it, but ctxd
// writes log rows through the same package (llmlog.go:160,208), so it is a
// server package and its DB calls answer to the server's rules, not to the
// tooling rule. The consequence is honest and worth naming: the DB access of
// ctx-llmlog-export is unfenced HERE because it does not live in a tool-only
// package. TestDBFenceScanAreaIsToolOnly pins that exclusion so it stays a
// decision.
package cmd

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
)

const (
	pgxImportPath   = "github.com/jackc/pgx/v5"
	pgxdbImportPath = "github.com/GottZ/ctx/internal/pgxdb"
)

// dbCallMethods are the pgx entry points that put a statement on the wire,
// with the smallest argument count each of them can legally have. The arity
// floor is what keeps unrelated method names out: (*url.URL).Query() takes
// none and is not a database call.
var dbCallMethods = map[string]int{
	"Exec":      2,
	"Query":     2,
	"QueryRow":  2,
	"SendBatch": 2,
	"CopyFrom":  2,
	"Begin":     1,
	"BeginTx":   2,
}

// txOpeners are the four pgxdb entry points that open a transaction, mapped to
// "opens it READ WRITE". Read (tx.go:126) is the form the rule asks for; Write
// and WriteOpts open READ WRITE, and so does Probe (tx.go:153) even though it
// never commits — the rule is about the MODE the opener asks the database for,
// and a READ WRITE transaction that rolls back is still not a read-only one.
// pgxdb.At is a label builder and opens nothing, which is why it is absent.
var txOpeners = map[string]bool{
	"Read":      false,
	"Write":     true,
	"WriteOpts": true,
	"Probe":     true,
}

const (
	distillresetWriteReason = "THE write contract of the tooling (E06-2, Entscheid E4-5). The bare UPDATE ... RETURNING " +
		"is the exact reversal of a documented retype; store.UpdateBlock would add type_source='manual' and " +
		"updated_at=now(), and reset.go:37-39 states that this stamp would make a restoration look like a corpus " +
		"movement to the drift watermark. Three gates run before it (reset.go:41+), the first being instance_kind = " +
		"measurement copy without override. If this line moves, re-read that reasoning before moving the entry"
	distillresetReadReason = "read-only listing of the candidate rows in the same run as the write above; no transaction " +
		"is opened because the two SELECTs are the report of what the UPDATE did and did not touch, and a shared " +
		"snapshot with the write would only hide a concurrent change instead of showing it"
	goldsetReadOnlyConnReason = "internal/goldset dials ONE dedicated connection with default_transaction_read_only=on " +
		"as a server-side guard (db.go:15-24), enforced by PostgreSQL rather than by the caller. Note the exact " +
		"strength: it is a SESSION DEFAULT, so an explicit BEGIN READ WRITE (or a SET) on that connection would " +
		"still write — no code here does either, and that is the thing to check before adding a site. Every site " +
		"in this list is a SELECT that draws the gold sample"
)

// directDBAccess is the exception list: every raw handle call in the tool-only
// graph, keyed by the slash-written site and carrying the reason it may bypass
// pgxdb.Read. A site without a reason has been silenced, not excepted; a site
// that no longer exists is a stale permission and fails too.
var directDBAccess = map[string]string{
	"internal/distillreset/reset.go:229": distillresetReadReason,
	"internal/distillreset/reset.go:250": distillresetReadReason,
	"internal/distillreset/reset.go:278": distillresetWriteReason,

	"internal/goldset/db.go:58":      goldsetReadOnlyConnReason,
	"internal/goldset/db.go:72":      goldsetReadOnlyConnReason,
	"internal/goldset/db.go:98":      goldsetReadOnlyConnReason,
	"internal/goldset/db.go:120":     goldsetReadOnlyConnReason,
	"internal/goldset/db.go:145":     goldsetReadOnlyConnReason,
	"internal/goldset/db.go:164":     goldsetReadOnlyConnReason,
	"internal/goldset/db.go:178":     goldsetReadOnlyConnReason,
	"internal/goldset/slices.go:120": goldsetReadOnlyConnReason,
	"internal/goldset/slices.go:151": goldsetReadOnlyConnReason,
	"internal/goldset/slices.go:249": goldsetReadOnlyConnReason,
	"internal/goldset/slices.go:303": goldsetReadOnlyConnReason,
	"internal/goldset/slices.go:340": goldsetReadOnlyConnReason,
	"internal/goldset/slices.go:377": goldsetReadOnlyConnReason,
}

// theWritingSite is the one entry of directDBAccess whose SQL is write-shaped.
// docs/operations.md names ctx-distillreset as the single writing exception;
// TestTheOnlyWritingDirectSiteIsDistillreset is what makes that sentence true
// rather than merely written.
const theWritingSite = "internal/distillreset/reset.go:278"

// dbFinding is one DB call the fence saw: the site, what was called on what,
// and — when the statement starts with a string literal — whether the SQL
// writes. The SQL shape is evidence, never the rule: goldset builds two of its
// statements in a variable (db.go:72,145), and a fence that judged by SQL
// alone would be blind there. The rule is the exception list.
type dbFinding struct {
	Site     string
	Call     string
	SQLShape string // "read", "write" or "unknown"
}

func (f dbFinding) String() string { return f.Site + " " + f.Call + " (sql: " + f.SQLShape + ")" }

// TestEveryDirectDBAccessIsListed is the tripwire. A new raw handle call, or a
// pgxdb.Write, in a tool-only package is a decision with one line here — not a
// line without a decision.
func TestEveryDirectDBAccessIsListed(t *testing.T) {
	pkgs := dbFencePackages(t)
	start := time.Now()
	findings, reads, files := scanDBAccess(t, pkgs)
	t.Logf("scanned %d tool-only packages, %d non-test files in %s (%d pgxdb.Read openers seen)",
		len(pkgs), files, time.Since(start).Round(time.Millisecond), reads)
	for _, f := range findings {
		if _, ok := directDBAccess[f.Site]; ok {
			continue
		}
		t.Errorf("%s bypasses the read-only rule for DB-direct tools — open the transaction with "+
			"pgxdb.Read, or add the site to directDBAccess with the reason it may write or go bare", f)
	}
}

// TestEveryDirectDBAccessEntryCarriesAReasonAndASite keeps the list from
// rotting in both directions: an entry without a reason is a silencing, and an
// entry whose line no longer holds a raw call is a permission that would cover
// whatever moves onto that line next.
func TestEveryDirectDBAccessEntryCarriesAReasonAndASite(t *testing.T) {
	findings, _, _ := scanDBAccess(t, dbFencePackages(t))
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.Site] = true
	}
	for site, reason := range directDBAccess {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is excepted without a reason — say why it may bypass pgxdb.Read", site)
		}
		if !seen[site] {
			t.Errorf("%s is excepted but holds no raw DB call any more — the site moved or went away; "+
				"drop the entry, or move it to the line the call sits on now", site)
		}
	}
	t.Logf("%d raw DB call sites in the tool-only graph, %d exceptions", len(findings), len(directDBAccess))
}

// TestTheOnlyWritingDirectSiteIsDistillreset checks the claim the doc sentence
// makes. Sites whose SQL is not a literal cannot be classified and are logged
// rather than judged — that limit is why the exception list, and not the SQL
// shape, is the rule.
func TestTheOnlyWritingDirectSiteIsDistillreset(t *testing.T) {
	findings, _, _ := scanDBAccess(t, dbFencePackages(t))
	writers, unknown := 0, 0
	for _, f := range findings {
		switch f.SQLShape {
		case "write":
			writers++
			if f.Site != theWritingSite {
				t.Errorf("%s writes through a raw handle — docs/operations.md names ctx-distillreset as the "+
					"ONLY writing DB-direct tool; either that sentence is wrong now or this call is", f)
			}
		case "unknown":
			unknown++
			t.Logf("%s: SQL is not a leading literal, shape not classifiable — covered by the exception list only", f)
		}
	}
	if writers == 0 {
		t.Errorf("no write-shaped raw call found at all — %s was the one; either it is gone (then the "+
			"doc sentence and this fence need rewriting) or the SQL classifier stopped working", theWritingSite)
	}
	t.Logf("%d raw sites, %d write-shaped, %d unclassifiable", len(findings), writers, unknown)
}

// TestDBFenceSeesTheLivingCalls is the control against a silent fence. A
// matcher that resolved nothing — a renamed pgx import, a changed AST shape —
// would let every test above pass while seeing an empty tree.
func TestDBFenceSeesTheLivingCalls(t *testing.T) {
	findings, reads, files := scanDBAccess(t, dbFencePackages(t))
	if files < 40 {
		t.Errorf("only %d non-test files in the tool-only graph — the sweep lost the tree", files)
	}
	if reads < 7 {
		t.Errorf("only %d pgxdb.Read openers seen; cmd/ctx-armcost alone holds seven (report.go, pertopic.go) — "+
			"the pgxdb qualifier no longer resolves", reads)
	}
	if len(findings) < len(directDBAccess) {
		t.Errorf("%d raw call sites found but %d are excepted — the matcher sees less than the list", len(findings), len(directDBAccess))
	}
	if _, ok := directDBAccess[theWritingSite]; !ok {
		t.Errorf("%s left the exception list without this fence being rewritten", theWritingSite)
	}
}

// TestDBFenceScanAreaIsToolOnly pins the scan area. The set is computed, not
// remembered — a new tool-only package joins the fence by existing — but the
// members below are the ones the rule was written for, and the non-members are
// the packages that must NOT be judged by it: the server and the CLI have
// their own rules, and internal/llmlog is a server package that a tool happens
// to call.
func TestDBFenceScanAreaIsToolOnly(t *testing.T) {
	paths := map[string]bool{}
	names := make([]string, 0, 16)
	for _, pkg := range dbFencePackages(t) {
		paths[pkg.ImportPath] = true
		names = append(names, strings.TrimPrefix(pkg.ImportPath, envScanModulePath+"/"))
	}
	sort.Strings(names)
	t.Logf("tool-only graph: %d packages — %s", len(names), strings.Join(names, ", "))
	for _, want := range []string{
		"cmd/ctx-armcost", "cmd/ctx-armsweep", "cmd/ctx-distillreset",
		"cmd/ctx-goldbench", "cmd/ctx-goldset", "cmd/ctx-llmlog-export",
		"internal/armsweep", "internal/distillreset", "internal/goldbench", "internal/goldset",
	} {
		if !paths[envScanModulePath+"/"+want] {
			t.Errorf("tool-only graph misses %s — its DB calls would be unfenced", want)
		}
	}
	for _, unwanted := range []string{
		"cmd/ctxd", "cmd/ctx", "internal/llmlog", "internal/handler", "internal/store",
		"internal/toolboot", "internal/pgxdb", "internal/clientconfig",
	} {
		if paths[envScanModulePath+"/"+unwanted] {
			t.Errorf("tool-only graph contains %s — it is reachable from the server or the CLI and answers "+
				"to the product's rules, not to the tooling rule", unwanted)
		}
	}
}

// dbFencePackages is the scan area: the packages reachable from cmd/ctx-* and
// from nowhere else in the module. Both subtractions matter — cmd/ctxd is the
// server, cmd/ctx is the shipped CLI, and a package either of them can reach
// is part of the product no matter who else calls it.
//
// The graph is built from the import declarations rather than from `go list`
// so that the fence needs no toolchain subprocess and no build of the module.
func dbFencePackages(t *testing.T) []config.ScanPackage {
	t.Helper()
	root := moduleRoot(t)
	graph := importGraph(t, root)

	tools := map[string]bool{}
	for _, pkg := range toolPackages(t) {
		reachable(graph, pkg.ImportPath, tools)
	}
	product := map[string]bool{}
	for _, entry := range []string{"/cmd/ctxd", "/cmd/ctx"} {
		reachable(graph, envScanModulePath+entry, product)
	}

	var out []config.ScanPackage
	for path := range tools {
		if product[path] {
			continue
		}
		rel := strings.TrimPrefix(path, envScanModulePath+"/")
		out = append(out, config.ScanPackage{ImportPath: path, Dir: filepath.Join(root, filepath.FromSlash(rel))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImportPath < out[j].ImportPath })
	// A lower bound, not a count: the set grows and shrinks with legitimate
	// work (13 on 2026-09-05). A subtraction that swallowed the set would make
	// every gate above vacuously green.
	if len(out) < 10 {
		t.Fatalf("tool-only graph has only %d packages — the reachability subtraction lost the tree", len(out))
	}
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	return root
}

// importGraph maps every module package that holds non-test Go files to the
// module packages it imports.
func importGraph(t *testing.T, root string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	graph := map[string][]string{}
	err := filepath.WalkDir(root, func(abs string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDir(entry.Name()) && abs != root {
				return fs.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		self, err := importPathOf(root, filepath.Dir(abs))
		if err != nil {
			return err
		}
		file, err := goparser.ParseFile(fset, abs, nil, goparser.ImportsOnly|goparser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if _, ok := graph[self]; !ok {
			graph[self] = nil
		}
		for _, spec := range file.Imports {
			path, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil || !strings.HasPrefix(path, envScanModulePath) {
				continue
			}
			graph[self] = append(graph[self], path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("build import graph under %s: %v", root, err)
	}
	return graph
}

func skipDir(name string) bool {
	return name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func importPathOf(root, dir string) (string, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return envScanModulePath, nil
	}
	return envScanModulePath + "/" + filepath.ToSlash(rel), nil
}

func reachable(graph map[string][]string, from string, seen map[string]bool) {
	if seen[from] {
		return
	}
	seen[from] = true
	for _, next := range graph[from] {
		reachable(graph, next, seen)
	}
}

// scanDBAccess walks the non-test files of the scan area and returns every DB
// call whose receiver is NOT a pgx.Tx, plus the number of pgxdb.Read openers
// and of files seen (both for the control test).
func scanDBAccess(t *testing.T, pkgs []config.ScanPackage) (findings []dbFinding, reads, files int) {
	t.Helper()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
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
			// Slash-normalised and kept that way: the only consumers are the
			// directDBAccess key, the failure message, and the FromSlash round
			// trip below. filepath.Dir on a normalised path is the Windows trap
			// T06-7 hit — it is not applied here.
			got, r := scanDBAccessFile(fset, file, filepath.ToSlash(rel))
			findings = append(findings, got...)
			reads += r
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Site < findings[j].Site })
	return findings, reads, files
}

// importedAsPkg is importedAs with the package's declared name supplied for
// the unnamed case. importedAs derives that name from the last path segment,
// which holds for "os" and for internal/pgxdb but NOT for a versioned module
// path: the last segment of github.com/jackc/pgx/v5 is "v5", the package is
// pgx. A fence that resolved the qualifier to "v5" would never recognise a
// pgx.Tx and would report every correct call site as a violation.
func importedAsPkg(file *ast.File, importPath, pkgName string) map[string]bool {
	out := importedAs(file, importPath)
	last := importPath[strings.LastIndex(importPath, "/")+1:]
	if last != pkgName && out[last] {
		delete(out, last)
		out[pkgName] = true
	}
	return out
}

// scanUnit is one top-level declaration that can hold executable code: a
// function with its signature, or one value of a var/const declaration whose
// expression may contain function literals — directly (var f = func(…){…}) or
// nested in a composite literal (var m = map[string]func(…){"k": func(…){…}}).
//
// The unit is the scope in which receiver types are resolved, so it must be
// the whole declaration and not just a function body: iterating only
// *ast.FuncDecl was the fail-open the review found — a package variable
// holding a closure that calls pool.Exec was never walked at all.
type scanUnit struct {
	sig  []*ast.FieldList // receiver, parameters, results of a FuncDecl
	code []ast.Node       // the nodes to walk for bindings and for calls
}

// scanUnits splits a file into the declarations that can hold a DB call.
// Import and type declarations hold none and are skipped.
func scanUnits(file *ast.File) []scanUnit {
	var out []scanUnit
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			out = append(out, scanUnit{
				sig:  []*ast.FieldList{d.Recv, d.Type.Params, d.Type.Results},
				code: []ast.Node{d.Body},
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, expr := range value.Values {
					out = append(out, scanUnit{code: []ast.Node{expr}})
				}
			}
		}
	}
	return out
}

func scanDBAccessFile(fset *token.FileSet, file *ast.File, rel string) (findings []dbFinding, reads int) {
	pgxNames := importedAsPkg(file, pgxImportPath, "pgx")
	pgxdbNames := importedAsPkg(file, pgxdbImportPath, "pgxdb")
	for _, unit := range scanUnits(file) {
		txNames := txIdentifiers(unit, pgxNames)
		walk := func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			site := rel + ":" + strconv.Itoa(fset.Position(call.Pos()).Line)
			if write, ok := pgxdbOpener(sel, pgxdbNames); ok {
				if !write {
					reads++
					return true
				}
				findings = append(findings, dbFinding{Site: site, Call: "pgxdb." + sel.Sel.Name, SQLShape: "write"})
				return true
			}
			if !isDBCall(call, sel) || receiverIsTx(sel.X, txNames) {
				return true
			}
			findings = append(findings, dbFinding{
				Site:     site,
				Call:     exprName(sel.X) + "." + sel.Sel.Name,
				SQLShape: sqlShape(call, sel.Sel.Name),
			})
			return true
		}
		for _, node := range unit.code {
			ast.Inspect(node, walk)
		}
	}
	return findings, reads
}

// pgxdbOpener reports whether the selector is a pgxdb transaction opener and
// whether that opener is a writing one. The package qualifier is resolved
// through the import path, so an aliased import cannot walk past the fence.
func pgxdbOpener(sel *ast.SelectorExpr, pgxdbNames map[string]bool) (write, ok bool) {
	ident, isIdent := sel.X.(*ast.Ident)
	if !isIdent || !pgxdbNames[ident.Name] {
		return false, false
	}
	write, known := txOpeners[sel.Sel.Name]
	return write, known
}

// isDBCall reports whether the call has the shape of a pgx statement call. The
// arity floor is deliberate: (*url.URL).Query() shares the name and takes no
// arguments.
func isDBCall(call *ast.CallExpr, sel *ast.SelectorExpr) bool {
	minArgs, ok := dbCallMethods[sel.Sel.Name]
	return ok && len(call.Args) >= minArgs
}

// txIdentifiers returns the identifiers inside one scan unit whose EVERY
// binding declares pgx.Tx — the declaration's own receiver, parameters and
// results, the same for every nested function literal, and every var
// declaration. A name bound to pgx.Tx in one place and to something else in
// another is deliberately NOT returned: the fence resolves fail-closed, and a
// := binding (whose type this file does not infer) counts as "something else".
func txIdentifiers(unit scanUnit, pgxNames map[string]bool) map[string]bool {
	isTx := map[string]bool{}
	conflict := map[string]bool{}
	bind := func(name string, tx bool) {
		if name == "" || name == "_" {
			return
		}
		if seen, ok := isTx[name]; ok && seen != tx {
			conflict[name] = true
		}
		isTx[name] = isTx[name] || tx
		if !tx {
			conflict[name] = true
		}
	}
	bindFields := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, field := range list.List {
			tx := isPgxTx(field.Type, pgxNames)
			if len(field.Names) == 0 {
				continue
			}
			for _, name := range field.Names {
				bind(name.Name, tx)
			}
		}
	}
	for _, list := range unit.sig {
		bindFields(list)
	}
	bindings := func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.FuncLit:
			bindFields(x.Type.Params)
			bindFields(x.Type.Results)
		case *ast.ValueSpec:
			for _, name := range x.Names {
				bind(name.Name, isPgxTx(x.Type, pgxNames))
			}
		case *ast.AssignStmt:
			if x.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range x.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok {
					bind(ident.Name, false)
				}
			}
		}
		return true
	}
	for _, node := range unit.code {
		ast.Inspect(node, bindings)
	}
	out := map[string]bool{}
	for name, tx := range isTx {
		if tx && !conflict[name] {
			out[name] = true
		}
	}
	return out
}

// isPgxTx reports whether a type expression denotes pgx.Tx, with the package
// qualifier resolved through the import path of pgx rather than through the
// name "pgx".
func isPgxTx(expr ast.Expr, pgxNames map[string]bool) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Tx" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && pgxNames[ident.Name]
}

func receiverIsTx(recv ast.Expr, txNames map[string]bool) bool {
	ident, ok := recv.(*ast.Ident)
	return ok && txNames[ident.Name]
}

// exprName renders a receiver expression for the failure message. It is
// documentation of the finding, never part of the rule.
func exprName(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprName(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprName(x.Fun) + "()"
	case *ast.IndexExpr:
		return exprName(x.X) + "[…]"
	case *ast.StarExpr:
		return "*" + exprName(x.X)
	case *ast.ParenExpr:
		return exprName(x.X)
	default:
		return "<expr>"
	}
}

// sqlShape classifies the statement of a DB call by its leading string
// literal — "unknown" when the SQL is assembled elsewhere. Evidence for
// TestTheOnlyWritingDirectSiteIsDistillreset, not a gate on its own.
func sqlShape(call *ast.CallExpr, method string) string {
	pos, ok := map[string]int{"Exec": 1, "Query": 1, "QueryRow": 1}[method]
	if !ok || len(call.Args) <= pos {
		return "unknown"
	}
	lit := leadingLiteral(call.Args[pos])
	if lit == "" {
		return "unknown"
	}
	fields := strings.Fields(strings.ToUpper(lit))
	if len(fields) == 0 {
		return "unknown"
	}
	switch fields[0] {
	case "SELECT", "WITH", "TABLE", "VALUES", "SHOW", "EXPLAIN":
		return "read"
	case "UPDATE", "INSERT", "DELETE", "CREATE", "DROP", "ALTER", "TRUNCATE", "COPY", "GRANT", "REVOKE", "SET", "REFRESH":
		return "write"
	default:
		return "unknown"
	}
}

// leadingLiteral unwraps the left spine of a string concatenation and returns
// the first literal, which is where an SQL verb sits.
func leadingLiteral(expr ast.Expr) string {
	for {
		switch x := expr.(type) {
		case *ast.ParenExpr:
			expr = x.X
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return ""
			}
			expr = x.X
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return ""
			}
			value, err := strconv.Unquote(x.Value)
			if err != nil {
				return ""
			}
			return value
		default:
			return ""
		}
	}
}

// TestDirectDBAccessSitesExist keeps the list keyed to real files: a path that
// no longer exists cannot hold the call the reason talks about.
func TestDirectDBAccessSitesExist(t *testing.T) {
	root := moduleRoot(t)
	for site := range directDBAccess {
		file, _, ok := strings.Cut(site, ":")
		if !ok {
			t.Errorf("%s is not a file:line site", site)
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(file))); err != nil {
			t.Errorf("%s is excepted but does not exist: %v", site, err)
		}
	}
}
