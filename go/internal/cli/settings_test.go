package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The settings envelope cases (TestCheckSettingsEnvelope) moved to
// envelope_test.go when the two checkers became one (T03-13).

// toJSONValue decides the transport shape; the server normalizes the type.
// Literal scalars pass through, arbitrary text becomes a JSON string.
func TestToJSONValue(t *testing.T) {
	cases := map[string]string{
		"0.7":          `0.7`,
		"42":           `42`,
		"true":         `true`,
		`"quoted"`:     `"quoted"`,
		"qwen3.5:9b":   `"qwen3.5:9b"`,
		"45d":          `"45d"`,
		"hello world":  `"hello world"`,
		"[1,2]":        `"[1,2]"`, // arrays are not scalars — stringified, server 422s
		`{"a":1}`:      `"{\"a\":1}"`,
		"private,work": `"private,work"`,
	}
	for in, want := range cases {
		if got := string(toJSONValue(in)); got != want {
			t.Errorf("toJSONValue(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRenderCell(t *testing.T) {
	if got := renderCell([]any{"private", "shared"}); got != "private,shared" {
		t.Errorf("slice cell = %q", got)
	}
	if got := renderCell(0.7); got != "0.7" {
		t.Errorf("float cell = %q", got)
	}
	if got := renderCell("(set via env)"); got != "(set via env)" {
		t.Errorf("string cell = %q", got)
	}
}

// Client.Do carries method, path, auth header and body to the REST surface
// and returns body + status untouched.
func TestClientDo(t *testing.T) {
	var gotMethod, gotPath, gotKey, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey = r.Method, r.URL.Path, r.Header.Get("X-Context-Key")
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"success":false,"error":"validation: nope"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/", Key: "test-key", HTTPClient: srv.Client()}
	resp, status, err := c.Do(http.MethodPut, "/api/settings/rerank.blend_weight",
		map[string]any{"value": toJSONValue("1.5")})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/settings/rerank.blend_weight" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotKey != "test-key" {
		t.Errorf("auth header = %q", gotKey)
	}
	if gotBody != `{"value":1.5}` {
		t.Errorf("body = %s", gotBody)
	}
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", status)
	}
	// The 422 envelope must turn into a command error downstream.
	if err := checkEnvelope(resp, envelopeRequired); err == nil || !strings.Contains(err.Error(), "validation") {
		t.Errorf("envelope err = %v", err)
	}
}
