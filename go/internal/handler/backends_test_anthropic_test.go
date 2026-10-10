package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestBackendTestAnthropicProbe: an anthropic-protocol backend-test probes
// GET /v1/models with the Messages API headers (x-api-key +
// anthropic-version, no Bearer) and reports base_url/auth ok on a 2xx — the
// generic branch would GET the bare root with a Bearer header, which the
// API answers 404 (unreachable) or 401.
func TestBackendTestAnthropicProbe(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		hdr = r.Header.Clone()
		mu.Unlock()
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{{
		ID: "0190-ant", Name: "anthropic", Host: srv.URL,
		Protocol: backends.ProtocolAnthropic, ProviderClass: backends.ProviderGeneric,
		Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal,
		Roles: []string{backends.RoleSynthesis}, NumCtx: 100000,
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "claude-opus-5-5"}},
		APIKey:   "sk-ant-test", APIKeyRef: "anthropic-chat", Enabled: true,
	}})

	rec := manageReqWithPool(t, adminAR(), bp, map[string]any{
		"action": "backend-test", "id": "0190-ant",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("backend-test: status %d (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"base_url":"ok"`, `"auth":"ok"`} {
		if !strings.Contains(body, want) {
			t.Errorf("backend-test response lacks %s: %s", want, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/v1/models" {
		t.Errorf("probe paths = %v, want exactly /v1/models", paths)
	}
	if hdr.Get("x-api-key") != "sk-ant-test" || hdr.Get("anthropic-version") == "" {
		t.Errorf("probe headers = %v, want x-api-key + anthropic-version", hdr)
	}
	if hdr.Get("Authorization") != "" {
		t.Errorf("probe sent a Bearer header on the anthropic wire: %q", hdr.Get("Authorization"))
	}
}
