package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

func lp(p float64) float64 { return math.Log(p) }

func TestDecideChoice_Distribution(t *testing.T) {
	top := []TokenLogprob{
		{Token: "D", Logprob: lp(0.60)},
		{Token: " D", Logprob: lp(0.10)}, // tokenizer spelling variant, same label
		{Token: "E", Logprob: lp(0.20)},
		{Token: "C", Logprob: lp(0.05)},
		{Token: "<think>", Logprob: lp(0.04)}, // not a label: excluded from mass
	}
	d, err := DecideChoice(top, []string{"A", "B", "C", "D", "E"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Best != "D" {
		t.Fatalf("Best = %q, want D", d.Best)
	}
	if got := d.Mass; math.Abs(got-0.95) > 1e-9 {
		t.Fatalf("Mass = %v, want 0.95", got)
	}
	// renormalised over labels: D 0.70/0.95, E 0.20/0.95, C 0.05/0.95, A/B 0
	if got := d.Probs["D"]; math.Abs(got-0.70/0.95) > 1e-9 {
		t.Fatalf("P(D) = %v", got)
	}
	if got := d.Probs["A"]; got != 0 {
		t.Fatalf("P(A) = %v, want 0", got)
	}
	var sum float64
	for _, p := range d.Probs {
		sum += p
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("probs sum = %v, want 1", sum)
	}
	// confidence (n·peak−1)/(n−1) with n=5, peak≈0.7368 → ≈0.671
	peak := 0.70 / 0.95
	if want := (5*peak - 1) / 4; math.Abs(d.Confidence-want) > 1e-9 {
		t.Fatalf("Confidence = %v, want %v", d.Confidence, want)
	}
}

func TestDecideChoice_ConfidenceEdges(t *testing.T) {
	one, _ := DecideChoice([]TokenLogprob{{Token: "Yes", Logprob: lp(0.9)}}, []string{"Yes", "No"})
	if one.Confidence != 1 || one.Best != "Yes" || one.Probs["Yes"] != 1 {
		t.Fatalf("all mass on one label: %+v", one)
	}
	uni, _ := DecideChoice([]TokenLogprob{
		{Token: "A", Logprob: lp(0.3)}, {Token: "B", Logprob: lp(0.3)}, {Token: "C", Logprob: lp(0.3)},
	}, []string{"A", "B", "C"})
	if math.Abs(uni.Confidence) > 1e-9 {
		t.Fatalf("uniform confidence = %v, want 0", uni.Confidence)
	}
	if uni.Best != "A" {
		t.Fatalf("tie must resolve to first label, got %q", uni.Best)
	}
	single, _ := DecideChoice([]TokenLogprob{{Token: "Yes", Logprob: lp(0.6)}}, []string{"Yes"})
	if single.Confidence != 1 {
		t.Fatalf("n=1 confidence is the renormalised peak (1), got %v", single.Confidence)
	}
}

func TestDecideChoice_Errors(t *testing.T) {
	if _, err := DecideChoice(nil, []string{"A"}); !errors.Is(err, ErrNoLogprobs) {
		t.Fatalf("nil top: %v", err)
	}
	if _, err := DecideChoice([]TokenLogprob{{Token: "<think>", Logprob: lp(0.99)}}, []string{"A", "B"}); !errors.Is(err, ErrNoLabelMass) {
		t.Fatalf("no label mass: %v", err)
	}
	// A sliver of label mass under prose is not a decision either.
	if _, err := DecideChoice([]TokenLogprob{{Token: "The", Logprob: lp(0.90)}, {Token: "D", Logprob: lp(0.09)}, {Token: "E", Logprob: lp(0.01)}}, []string{"D", "E"}); !errors.Is(err, ErrNoLabelMass) {
		t.Fatalf("mass 0.10 must be a fallback, got %v", err)
	}
	if d, err := DecideChoice([]TokenLogprob{{Token: "D", Logprob: lp(0.45)}, {Token: "E", Logprob: lp(0.06)}, {Token: "x", Logprob: lp(0.49)}}, []string{"D", "E"}); err != nil || d.Best != "D" {
		t.Fatalf("mass 0.51 is above the floor: %+v %v", d, err)
	}
	if _, err := DecideChoice([]TokenLogprob{{Token: "A", Logprob: 0}}, nil); err == nil {
		t.Fatal("no labels must error")
	}
	if _, err := DecideChoice([]TokenLogprob{{Token: "A", Logprob: 0}}, []string{"A", "A"}); err == nil {
		t.Fatal("duplicate labels must error")
	}
	if _, err := DecideChoice([]TokenLogprob{{Token: "A", Logprob: 0}}, []string{"A", ""}); err == nil {
		t.Fatal("empty label must error")
	}
	if !IsDecideFallback(ErrNoLogprobs) || !IsDecideFallback(ErrNoLabelMass) || IsDecideFallback(errors.New("wire")) {
		t.Fatal("IsDecideFallback classification wrong")
	}
	if _, err := Decide(nil, []string{"A"}); !errors.Is(err, ErrNoLogprobs) {
		t.Fatalf("nil response: %v", err)
	}
}

// TestChatOpenAI_TopLogprobsWire proves the request carries logprobs/
// top_logprobs exactly when asked, and that the decode reads content[0]'s
// alternatives (plus the sampled token when the server lists it only there).
func TestChatOpenAI_TopLogprobsWire(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"D"},
			"finish_reason":"length",
			"logprobs":{"content":[{"token":"D","logprob":-0.1,
				"top_logprobs":[{"token":"E","logprob":-2.5},{"token":"C","logprob":-4.0}]}]}}],
			"usage":{"completion_tokens":1,"prompt_tokens":420}}`))
	}))
	t.Cleanup(srv.Close)
	b := backends.Backend{Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m", Trust: backends.TrustFull}

	opts := DecideOptions()
	resp, err := Chat(context.Background(), b, "sys", "usr", opts, 5*time.Second)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotBody["logprobs"] != true || gotBody["top_logprobs"] != float64(20) || gotBody["max_tokens"] != float64(1) {
		t.Fatalf("wire body missing decide fields: %v", gotBody)
	}
	if len(resp.TopLogprobs) != 3 {
		t.Fatalf("TopLogprobs = %+v, want 3 entries (2 alternatives + sampled token)", resp.TopLogprobs)
	}
	d, err := Decide(resp, []string{"A", "B", "C", "D", "E"})
	if err != nil || d.Best != "D" {
		t.Fatalf("decide: %+v %v", d, err)
	}

	// Ordinary chat: the pair must be absent from the wire, and the decode
	// must not fabricate logprobs.
	gotBody = nil
	plain, err := Chat(context.Background(), b, "sys", "usr", SynthesisOptions(0), 5*time.Second)
	if err != nil {
		t.Fatalf("plain chat: %v", err)
	}
	if _, has := gotBody["logprobs"]; has {
		t.Fatalf("plain chat must not send logprobs: %v", gotBody)
	}
	if _, has := gotBody["top_logprobs"]; has {
		t.Fatalf("plain chat must not send top_logprobs: %v", gotBody)
	}
	// the mock always answers with logprobs; decode is allowed to keep them
	_ = plain
}

// TestChatOpenAI_NoLogprobsInResponse: a server that ignores the request
// yields nil TopLogprobs and Decide reports ErrNoLogprobs — the fallback
// signal, not a fabricated distribution.
func TestChatOpenAI_NoLogprobsInResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"D"}}],
			"usage":{"completion_tokens":1,"prompt_tokens":420}}`))
	}))
	t.Cleanup(srv.Close)
	b := backends.Backend{Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m", Trust: backends.TrustFull}
	resp, err := Chat(context.Background(), b, "sys", "usr", DecideOptions(), 5*time.Second)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.TopLogprobs != nil {
		t.Fatalf("TopLogprobs = %+v, want nil", resp.TopLogprobs)
	}
	if _, err := Decide(resp, []string{"A", "B"}); !errors.Is(err, ErrNoLogprobs) {
		t.Fatalf("want ErrNoLogprobs, got %v", err)
	}
}

// TestOllamaOptions_TopLogprobsNotOnWire pins the json:"-" tag: the Ollama
// options block must never carry the ctx-internal field.
func TestOllamaOptions_TopLogprobsNotOnWire(t *testing.T) {
	raw, _ := json.Marshal(DecideOptions())
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"top_logprobs", "logprobs", "TopLogprobs"} {
		if _, has := m[k]; has {
			t.Fatalf("Options marshal leaks %q: %s", k, raw)
		}
	}
}

func TestFirstTokenLogprobs_DropsNullAndPositive(t *testing.T) {
	neg := -0.2
	pos := 0.3
	got := FirstTokenLogprobs("B", nil, []TokenLogprob{{Token: "A", Logprob: -1.0}, {Token: "C", Logprob: pos}})
	if len(got) != 1 || got[0].Token != "A" {
		t.Fatalf("null sampled + positive alt must be dropped: %+v", got)
	}
	got = FirstTokenLogprobs("B", &neg, []TokenLogprob{{Token: "A", Logprob: -1.0}})
	if len(got) != 2 || got[1].Token != "B" || got[1].Logprob != neg {
		t.Fatalf("sampled token appended once: %+v", got)
	}
	got = FirstTokenLogprobs("A", &neg, []TokenLogprob{{Token: "A", Logprob: -1.0}})
	if len(got) != 1 {
		t.Fatalf("sampled token already listed must not duplicate: %+v", got)
	}
}

// TestChatOpenAI_NullLogprobIsNotProbabilityOne pins the llama.cpp shape: a
// masked token arrives as logprob null and must vanish, not swamp the
// distribution as exp(0) = 1.
func TestChatOpenAI_NullLogprobIsNotProbabilityOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"D"},
			"logprobs":{"content":[{"token":"D","logprob":-0.1,
				"top_logprobs":[{"token":"D","logprob":-0.1},{"token":"B","logprob":null},{"token":"E","logprob":-2.5}]}]}}],
			"usage":{"completion_tokens":1,"prompt_tokens":10}}`))
	}))
	t.Cleanup(srv.Close)
	b := backends.Backend{Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m", Trust: backends.TrustFull}
	resp, err := Chat(context.Background(), b, "sys", "usr", DecideOptions(), 5*time.Second)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(resp.TopLogprobs) != 2 {
		t.Fatalf("null entry must be dropped: %+v", resp.TopLogprobs)
	}
	d, err := Decide(resp, []string{"A", "B", "C", "D", "E"})
	if err != nil || d.Best != "D" || d.Probs["B"] != 0 {
		t.Fatalf("decide: %+v %v", d, err)
	}
}
