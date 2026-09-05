package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The two budget PAIRS of the CLI. Each pair is one product decision, and the
// pairs are deliberately different (design/03 §5.6): 120 s / 10 MB carries LLM
// answer times on the main client, 500 ms / 1 MB is the statusline's per-prompt
// budget. A helper that unified either half would be a behaviour change at both
// ends, so this test reads BOTH halves, not just the timeout.
func TestClientBudgetPairs(t *testing.T) {
	cfg := Config{BaseURL: "http://example.invalid", Key: "k"}

	main := NewClient(cfg)
	if got, want := main.HTTPClient.Timeout, 120*time.Second; got != want {
		t.Errorf("main client timeout = %v, want %v", got, want)
	}
	if got, want := main.readLimit(), int64(10*1024*1024); got != want {
		t.Errorf("main client read cap = %d, want %d", got, want)
	}

	status := NewClientWithTimeout(cfg, 500*time.Millisecond, maxStatuslineResponse)
	if got, want := status.HTTPClient.Timeout, 500*time.Millisecond; got != want {
		t.Errorf("statusline client timeout = %v, want %v", got, want)
	}
	if got, want := status.readLimit(), int64(1*1024*1024); got != want {
		t.Errorf("statusline client read cap = %d, want %d", got, want)
	}

	// The init server probe keeps its own 5 s budget.
	if got, want := newHTTPClient(5*time.Second).Timeout, 5*time.Second; got != want {
		t.Errorf("init probe timeout = %v, want %v", got, want)
	}

	// A bare literal (the shape the package tests use) reads with the default
	// cap instead of returning empty bodies.
	if got, want := (&Client{}).readLimit(), int64(maxResponseSize); got != want {
		t.Errorf("zero-value read cap = %d, want %d", got, want)
	}
}

// TestClientReadCapIsEnforced is the behavioural half: the cap is not a
// decorative field, it truncates. Verzehnfachen der Kappe macht diesen Test rot.
func TestClientReadCapIsEnforced(t *testing.T) {
	const cap1MiB = 1 * 1024 * 1024
	payload := bytes.Repeat([]byte("x"), 3*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: "k"}, 5*time.Second, cap1MiB)
	body, err := c.Post("manage", map[string]any{"action": "stats"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(body) != cap1MiB {
		t.Errorf("capped read returned %d bytes, want %d", len(body), cap1MiB)
	}

	big := NewClient(Config{BaseURL: srv.URL, Key: "k"})
	body, err = big.Post("manage", map[string]any{"action": "stats"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(body) != len(payload) {
		t.Errorf("10 MB client returned %d bytes, want the full %d", len(body), len(payload))
	}
}

// TestForeignHostClientCarriesNoKey guards the ONE security invariant of this
// surface (design/03 §5.6): the version check talks to a foreign host, and the
// package Client sets the user's API key on every request. Whoever merges the
// two clients "for tidiness" ships the key to a third party. The gate is on the
// source, because the call cannot be exercised offline.
func TestForeignHostClientCarriesNoKey(t *testing.T) {
	src, err := os.ReadFile("init.go")
	if err != nil {
		t.Fatalf("read init.go: %v", err)
	}
	fn := functionBody(t, string(src), "func stepVersion()")
	for _, forbidden := range []string{"X-Context-Key", "c.Get(", "c.Post(", "c.Do(", "NewClient("} {
		if strings.Contains(fn, forbidden) {
			t.Errorf("stepVersion contains %q — the foreign-host request must not go through the keyed Client", forbidden)
		}
	}
	if !strings.Contains(fn, "api.github.com") {
		t.Fatal("stepVersion no longer names api.github.com — this gate lost its subject, re-point it")
	}
}

// functionBody returns the source between the given func header and the next
// line that is exactly "}" at column 0.
func functionBody(t *testing.T, src, header string) string {
	t.Helper()
	i := strings.Index(src, header)
	if i < 0 {
		t.Fatalf("function %q not found", header)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// TestPrintJSONMatchesMarshalIndent pins the substitution T03-12 made in
// project.go: the two hand-written pipe branches marshalled with
// json.MarshalIndent and printed with fmt.Println; they now marshal compactly
// and hand the bytes to renderOrJSON, which prints them with PrintJSON. The two
// forms must be byte-identical, otherwise `ctx project detect | jq` changes.
func TestPrintJSONMatchesMarshalIndent(t *testing.T) {
	values := []any{
		map[string]any{"success": true, "identity": "github:GottZ/ctx", "source": "git", "projects": []string{"a"}},
		map[string]any{"success": true, "identity": "manual:x", "source": "flag", "projects": []any{}},
		struct {
			Identity string `json:"identity"`
			Source   string `json:"source"`
		}{"github:GottZ/ctx", "git"},
	}
	for _, v := range values {
		want, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatalf("MarshalIndent: %v", err)
		}
		compact, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		got := captureStdout(t, func() { PrintJSON(compact) })
		if got != string(want)+"\n" {
			t.Errorf("PrintJSON(Marshal(v)) = %q, want MarshalIndent+newline %q", got, string(want)+"\n")
		}
	}
}

// TestRenderOrJSONPipeBranch pins the machine contract: with stdout redirected
// (the test's own state), renderOrJSON prints the server bytes and never calls
// the human renderer.
func TestRenderOrJSONPipeBranch(t *testing.T) {
	called := false
	out := captureStdout(t, func() {
		err := renderOrJSON([]byte(`{"success":true}`), func([]byte) error {
			called = true
			return nil
		})
		if err != nil {
			t.Errorf("renderOrJSON: %v", err)
		}
	})
	if called {
		t.Error("human renderer ran although stdout is not a terminal")
	}
	if want := "{\n  \"success\": true\n}\n"; out != want {
		t.Errorf("piped output = %q, want %q", out, want)
	}
}
