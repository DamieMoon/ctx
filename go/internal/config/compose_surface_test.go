package config

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"testing"
)

// composeDecisions is the complete list of registry env vars whose value the
// compose file is allowed to carry — the deployment's deliberate deviations
// from the registry default, written out with their reason. Deliberately a
// literal table and not a rule: a deviation is a decision somebody made, and
// a decision that nobody had to write down is indistinguishable from a
// forgotten default copy.
//
// Two of them are a PAIR, and the pair is a rule in the code: V3
// (validate.go) warns when rerank.blend_weight == 1.0 while graph.enabled is
// true, so a deployment that turns graph expansion on has to pull the blend
// weight off 1.0. Changing one without the other re-arms that warning.
var composeDecisions = map[string]string{
	"CTX_RERANK_BLEND_WEIGHT":   "0.5 instead of 1.0 — the V3 pair to graph.enabled=true below",
	"CTX_GRAPH_EXPAND_ENABLED":  "true instead of false — graph expansion is on in this deployment",
	"CTX_GRAPH_OVERVIEW_ENGINE": "ctx instead of gonum — the in-tree Louvain engine serves the map",
	"CTX_READ_SCOPES":           "private,shared instead of private,shared,work",
}

// TestComposeDeclaresEveryRegistryKey is the reachability gate: a knob that
// the container cannot receive is not a knob. It generalises the gate that
// used to hold only the 21 cluster.* keys (TestClusterComposeDeclaresEveryKey,
// cluster_c0_test.go, retired with wave T05-5) to the whole registry, because
// the failure it guarded against was never specific to one namespace: the
// documented legacy class is exactly this — docker-compose.yml declared only
// 3 of the 5 graph_overview.* env vars, so max_nodes/rebuild_timeout, the two
// liveness guards that have to be reachable FIRST at scale, were unreachable
// through compose. Holding one namespace to that standard while 122 other
// names had no gate at all is what let the gap open in the first place.
//
// The block is generated (artefakte/T05-5-compose-env.py in the wave corpus):
// a new registry key reaches the container by regenerating it, and this test
// is what says the regeneration was not skipped.
//
// One operational property, measured, not assumed: docker-compose.yml lives
// OUTSIDE the module, so `go test` does not invalidate its result cache when
// only that file changed — a stale PASS is served. Run the two compose gates
// with -count=1 after touching the block.
func TestComposeDeclaresEveryRegistryKey(t *testing.T) {
	declared := ctxServiceEnvNames(t)
	var missing []string
	for _, want := range EnvVars() {
		if !declared[want] {
			missing = append(missing, want)
		}
	}
	for _, name := range missing {
		t.Errorf("%s missing from the ctx service environment: block — the key is unreachable through compose",
			name)
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d registry env vars are undeclared; regenerate the block with artefakte/T05-5-compose-env.py",
			len(missing), len(EnvVars()))
	}
}

// TestComposeDeclaresEveryEnvOnlyServerName is TestComposeDeclaresEveryRegistryKey's
// sibling for the other half of the reachable surface: the seventeen names in
// EnvOnlyServerNames() carry no settings key and no registry default, so
// FromEnv never puts them within reach — the ctx service `environment:` block
// is the ONLY declaration that can. A name missing here is not a stale
// default copy (that class belongs to the registry gate above); it is a knob
// nobody can turn from .env at all, silently, because envonly.go's own
// comment names the class without a test enforcing it.
//
// Found by measurement, not assumed: seven of seventeen were undeclared
// before this gate existed (the six CTX_OAUTH_* lifetime/rate/mode knobs plus
// CTX_TRUSTED_PROXY) — none of them wired since EnvOnlyServerNames() grew
// past the ten names T05-5 carried forward byte-for-byte from the block that
// existed before the registry gate was built.
func TestComposeDeclaresEveryEnvOnlyServerName(t *testing.T) {
	declared := ctxServiceEnvNames(t)
	var missing []string
	for _, want := range EnvOnlyServerNames() {
		if !declared[want] {
			missing = append(missing, want)
		}
	}
	for _, name := range missing {
		t.Errorf("%s missing from the ctx service environment: block — an env-only server name with no registry fallback is unreachable through compose",
			name)
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d env-only server names are undeclared; add them to the env-only section of docker-compose.yml",
			len(missing), len(EnvOnlyServerNames()))
	}
}

// TestComposeCopiesNoRegistryDefault is the single-source gate, the negative
// twin of the one above: the compose block declares every knob and repeats no
// default. A `${NAME:-<value>}` whose value equals the registry default is a
// second source of truth for that default — it survives a registry change
// silently and then feeds the container a value the code no longer believes
// in. The declared form is `${NAME:-}`, which sets the variable empty, and
// empty is "not set" to every reader (FromEnv in load.go, and the two direct
// readers overview.WorkerMemLimitBytes and schemacontract.ResolveMode).
//
// Only the composeDecisions names may carry a value, and even there an
// alignment with the registry default is red: a "decision" that says what the
// default already says is a copy that stopped being visible as one. Names the
// registry does not know are not judged — CTX_CANONICAL_ISSUER carries a
// compose-side derivation (${…:-https://${CTX_HOSTNAME}}), which is no
// registry copy.
func TestComposeCopiesNoRegistryDefault(t *testing.T) {
	values := ctxServiceEnvValues(t)
	defaults := map[string]string{}
	for _, e := range registry() {
		if e.EnvVar != "-" {
			defaults[e.EnvVar] = e.defRaw
		}
	}

	var copies int
	for _, name := range sortedKeys(values) {
		def, isRegistry := defaults[name]
		if !isRegistry {
			continue
		}
		value, ok := composeDefaultLiteral(name, values[name])
		if !ok || value == "" {
			continue // wiring form or the declared `${NAME:-}` — carries no default
		}
		if value == def {
			copies++
			reason := "the registry is the only place a default belongs"
			if _, decided := composeDecisions[name]; decided {
				reason = "a deviation aligned with the default is no longer a decision"
			}
			t.Errorf("%s: compose repeats the registry default %q — %s", name, def, reason)
			continue
		}
		if _, decided := composeDecisions[name]; !decided {
			t.Errorf("%s: compose pins %q while the registry default is %q, and the line is not in composeDecisions — an undocumented deviation is indistinguishable from a stale copy",
				name, value, def)
		}
	}
	if copies > 0 {
		t.Errorf("%d registry defaults are copied into docker-compose.yml; regenerate the block with artefakte/T05-5-compose-env.py",
			copies)
	}

	// The table is a two-sided pin: a decision that quietly turned back into
	// `${NAME:-}` would pass every check above and lose the deployment's
	// choice without a single red line.
	for _, name := range sortedKeys(composeDecisions) {
		value, ok := composeDefaultLiteral(name, values[name])
		if !ok || value == "" {
			t.Errorf("%s is listed in composeDecisions (%s) but carries no value in docker-compose.yml — either the deployment changed its mind, then drop the row, or the line was regenerated over",
				name, composeDecisions[name])
		}
	}
}

// composeDefaultLiteral extracts the <value> of a `${NAME:-<value>}` compose
// assignment. ok=false for every other form — the CONTEXT_DB*/LISTEN_ADDR
// wiring lines carry container topology, not a registry default, and are not
// judged by either gate.
func composeDefaultLiteral(name, raw string) (string, bool) {
	prefix := "${" + name + ":-"
	if !strings.HasPrefix(raw, prefix) || !strings.HasSuffix(raw, "}") {
		return "", false
	}
	return raw[len(prefix) : len(raw)-1], true
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ctxServiceEnvValues returns the env var names declared in the `ctx`
// service's `environment:` block of the repo-root docker-compose.yml, mapped
// to their raw right-hand side. Line-scanned on purpose: pulling in a YAML
// dependency for one block would promote an indirect module to a direct one,
// and the block is a flat map of scalars.
func ctxServiceEnvValues(t *testing.T) map[string]string {
	t.Helper()
	const path = "../../../docker-compose.yml" // go/internal/config -> repo root
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v (the compose gate needs the repo root checkout)", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	values := map[string]string{}
	inCtx, inEnv := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 2: // a service name
			inCtx, inEnv = trimmed == "ctx:", false
		case indent == 4 && inCtx: // a service-level key
			inEnv = trimmed == "environment:"
		case indent >= 6 && inCtx && inEnv:
			if name, value, ok := strings.Cut(trimmed, ":"); ok {
				values[strings.TrimSpace(name)] = strings.TrimSpace(value)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if len(values) == 0 {
		t.Fatalf("no environment names found in the ctx service block of %s — the scanner lost the block", path)
	}
	return values
}

// ctxServiceEnvNames is the name-only view of ctxServiceEnvValues, for the
// gates that only ask whether a name is reachable at all
// (TestDigestModeComposeDeclared, TestRootMapComposeDeclaresEveryKey,
// TestComposeDeclaresNoRetiredVar).
func ctxServiceEnvNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for name := range ctxServiceEnvValues(t) {
		names[name] = true
	}
	return names
}
