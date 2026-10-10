// Native Anthropic Messages API wire (anthropic.go), non-streaming path:
// request shape, headers, effort resolution, extra_body merge, response
// decode (text/thinking blocks, usage incl. cache counters, stop-reason
// mapping, message id) and the error → failover-class mapping. Every probe
// runs against an httptest fake of api.anthropic.com — no live key in CI.
// What this file CANNOT prove (live only): an actual cache hit, the effort
// level's effect on output_tokens, 529 behaviour under load.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llmlog"
)

// anthropicRecorder captures path, decoded body and headers of one
// /v1/messages call and answers with a fixed JSON (or a fixed status).
type anthropicRecorder struct {
	mu     sync.Mutex
	path   string
	body   map[string]any
	header http.Header
	srv    *httptest.Server
}

func newAnthropicRecorder(t *testing.T, status int, respJSON string, respHeader map[string]string) *anthropicRecorder {
	t.Helper()
	rec := &anthropicRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		rec.mu.Lock()
		rec.path = r.URL.Path
		rec.body = m
		rec.header = r.Header.Clone()
		rec.mu.Unlock()
		for k, v := range respHeader {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respJSON))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *anthropicRecorder) recorded() (string, map[string]any, http.Header) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.path, rec.body, rec.header
}

// anthropicRespOK is a complete Messages API answer: a thinking block (empty
// text, the default display), two text blocks, cache counters on usage.
const anthropicRespOK = `{"id":"msg_01abc","type":"message","role":"assistant","model":"claude-opus-5-5",
  "content":[{"type":"thinking","thinking":"","signature":"sig"},
             {"type":"text","text":"Hel"},{"type":"text","text":"lo"}],
  "stop_reason":"end_turn","stop_sequence":null,
  "usage":{"input_tokens":10,"output_tokens":7,
           "cache_read_input_tokens":400,"cache_creation_input_tokens":100}}`

func anthropicTestBackend(host string) backends.Backend {
	return backends.Backend{
		Host: host, Protocol: backends.ProtocolAnthropic,
		ProviderClass: backends.ProviderGeneric, Model: "claude-opus-5-5",
		Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal,
		APIKey: "sk-ant-test", APIKeyRef: "anthropic-chat", NumCtx: 100000,
	}
}

// sampling keys the Claude 5 models answer 400 to, plus every openai-wire
// field that has no meaning on this wire. None may ever reach the body.
var anthropicForbiddenKeys = []string{
	"temperature", "top_p", "top_k", "min_p", "frequency_penalty", "presence_penalty",
	"repeat_penalty", "logprobs", "top_logprobs", "response_format", "reasoning", "seed",
	"thinking", "tool_choice", "stream_options", "options", "num_ctx", "effort",
}

func TestAnthropicRequestWire(t *testing.T) {
	rec := newAnthropicRecorder(t, 200, anthropicRespOK, nil)
	b := anthropicTestBackend(rec.srv.URL)
	b.Think = "false" // ignored on this wire — thinking cannot be disabled
	b.ExtraHeaders = map[string]string{"X-Title": "ctx", "anthropic-version": "1999-01-01"}
	opts := Options{Temperature: 0.1, TopP: 0.9, TopK: 40, RepeatPenalty: 1.1, NumPredict: 5, NumCtx: 100000, TopLogprobs: 20}
	if _, err := Chat(context.Background(), b, "sys prompt", "user prompt", opts, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	path, body, hdr := rec.recorded()

	if path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", path)
	}
	if got := hdr.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q, want the resolved api_key_ref", got)
	}
	if got := hdr.Get("anthropic-version"); got != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q (core header must beat extra_headers)", got, anthropicVersion)
	}
	if got := hdr.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want none (the Messages API authenticates via x-api-key)", got)
	}
	if got := hdr.Get("X-Title"); got != "ctx" {
		t.Errorf("X-Title = %q, want extra_headers to reach the wire", got)
	}
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}

	if body["model"] != "claude-opus-5-5" {
		t.Errorf("model = %v", body["model"])
	}
	if body["max_tokens"] != float64(5) {
		t.Errorf("max_tokens = %v, want NumPredict 5", body["max_tokens"])
	}
	if _, has := body["stream"]; has {
		t.Errorf("stream key present on the non-streaming path: %v", body["stream"])
	}
	sys, _ := body["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v, want one cached text block", body["system"])
	}
	blk, _ := sys[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != "sys prompt" {
		t.Errorf("system block = %v", blk)
	}
	if cc, _ := blk["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("system cache_control = %v, want {type: ephemeral}", blk["cache_control"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want exactly the user turn", body["messages"])
	}
	if m, _ := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "user prompt" {
		t.Errorf("messages[0] = %v", msgs[0])
	}
	if oc, _ := body["output_config"].(map[string]any); oc["effort"] != backends.AnthropicDefaultEffort {
		t.Errorf("output_config = %v, want effort %q by default", body["output_config"], backends.AnthropicDefaultEffort)
	}
	for _, k := range anthropicForbiddenKeys {
		if v, has := body[k]; has {
			t.Errorf("body carries %q = %v — the Claude 5 models reject sampling fields and nothing else here is a Messages API field", k, v)
		}
	}
}

// TestAnthropicEffortResolution: model_map params (Options.Extra) beat the
// row's extra_body, which beats the default; the key itself never lands
// top-level.
func TestAnthropicEffortResolution(t *testing.T) {
	effortOf := func(t *testing.T, b backends.Backend, opts Options) (string, map[string]any) {
		t.Helper()
		rec := newAnthropicRecorder(t, 200, anthropicRespOK, nil)
		b.Host = rec.srv.URL
		if _, err := Chat(context.Background(), b, "s", "u", opts, 5*time.Second); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		_, body, _ := rec.recorded()
		oc, _ := body["output_config"].(map[string]any)
		e, _ := oc["effort"].(string)
		return e, body
	}

	b := anthropicTestBackend("")
	b.ExtraBody = map[string]any{"effort": "high"}
	if e, body := effortOf(t, b, Options{NumPredict: 5}); e != "high" {
		t.Errorf("extra_body effort: got %q, want high", e)
	} else if _, leaked := body["effort"]; leaked {
		t.Error("effort merged top-level — it must be lifted into output_config only")
	}

	if e, _ := effortOf(t, b, Options{NumPredict: 5, Extra: map[string]any{"effort": "low"}}); e != "low" {
		t.Errorf("model_map effort must beat extra_body: got %q, want low", e)
	}

	if e, _ := effortOf(t, anthropicTestBackend(""), Options{NumPredict: 5}); e != backends.AnthropicDefaultEffort {
		t.Errorf("default effort: got %q, want %q", e, backends.AnthropicDefaultEffort)
	}
}

// TestAnthropicExtraBodyMerge: everything but effort merges top-level with
// the row winning over model_map params (openai-wire parity) — the
// operator's hatch for fields this wire does not model.
func TestAnthropicExtraBodyMerge(t *testing.T) {
	rec := newAnthropicRecorder(t, 200, anthropicRespOK, nil)
	b := anthropicTestBackend(rec.srv.URL)
	b.ExtraBody = map[string]any{
		"thinking": map[string]any{"type": "adaptive", "display": "summarized"},
		"metadata": map[string]any{"user_id": "row"},
	}
	opts := Options{NumPredict: 5, Extra: map[string]any{"metadata": map[string]any{"user_id": "params"}, "stop_sequences": []any{"END"}}}
	if _, err := Chat(context.Background(), b, "s", "u", opts, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, body, _ := rec.recorded()
	if th, _ := body["thinking"].(map[string]any); th["display"] != "summarized" {
		t.Errorf("extra_body.thinking did not reach the wire: %v", body["thinking"])
	}
	if md, _ := body["metadata"].(map[string]any); md["user_id"] != "row" {
		t.Errorf("extra_body must win over model_map params on collision: %v", body["metadata"])
	}
	if ss, _ := body["stop_sequences"].([]any); len(ss) != 1 || ss[0] != "END" {
		t.Errorf("model_map param stop_sequences did not reach the wire: %v", body["stop_sequences"])
	}
	if body["max_tokens"] != float64(5) {
		t.Errorf("max_tokens = %v, the merge must not disturb the modelled fields", body["max_tokens"])
	}
}

// TestAnthropicJSONModeIsNoOp pins the documented contract: ChatJSON sends
// the same body as Chat — no response_format, no output_config.format (that
// needs a schema ctx does not have), and the request still succeeds.
func TestAnthropicJSONModeIsNoOp(t *testing.T) {
	rec := newAnthropicRecorder(t, 200, anthropicRespOK, nil)
	b := anthropicTestBackend(rec.srv.URL)
	resp, err := ChatJSON(context.Background(), b, "s", "u", Options{NumPredict: 5}, 5*time.Second)
	if err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if resp.Message.Content != "Hello" {
		t.Errorf("content = %q", resp.Message.Content)
	}
	_, body, _ := rec.recorded()
	if _, has := body["response_format"]; has {
		t.Error("response_format sent on the anthropic wire")
	}
	if oc, _ := body["output_config"].(map[string]any); oc["format"] != nil {
		t.Errorf("output_config.format = %v, want none (no schema available)", oc["format"])
	}
}

// TestAnthropicMaxTokensRequired: max_tokens cannot be omitted on this wire,
// so a bare Options{} gets the documented fallback instead of a 400.
func TestAnthropicMaxTokensRequired(t *testing.T) {
	rec := newAnthropicRecorder(t, 200, anthropicRespOK, nil)
	b := anthropicTestBackend(rec.srv.URL)
	if _, err := Chat(context.Background(), b, "s", "u", Options{}, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, body, _ := rec.recorded()
	if body["max_tokens"] != float64(anthropicDefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want the fallback %d", body["max_tokens"], anthropicDefaultMaxTokens)
	}
}

// TestAnthropicEmptySystemOmitted: the API rejects an empty text block, so
// an empty system prompt must yield no system field at all.
func TestAnthropicEmptySystemOmitted(t *testing.T) {
	rec := newAnthropicRecorder(t, 200, anthropicRespOK, nil)
	b := anthropicTestBackend(rec.srv.URL)
	if _, err := Chat(context.Background(), b, "", "u", Options{NumPredict: 5}, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, body, _ := rec.recorded()
	if _, has := body["system"]; has {
		t.Errorf("system = %v, want the field omitted for an empty prompt", body["system"])
	}
}

// TestAnthropicResponseDecode: text blocks concatenate, thinking blocks are
// skipped, PromptTokens is the TOTAL prompt (uncached + cache read + cache
// write), the cache counters travel separately, the message id is the
// provider request id, and the openrouter-only fields stay zero.
func TestAnthropicResponseDecode(t *testing.T) {
	rec := newAnthropicRecorder(t, 200, anthropicRespOK, nil)
	b := anthropicTestBackend(rec.srv.URL)
	resp, err := Chat(context.Background(), b, "s", "u", Options{NumPredict: 5}, 5*time.Second)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Role != "assistant" || resp.Message.Content != "Hello" {
		t.Errorf("message = %+v, want assistant/Hello (text blocks joined, thinking skipped)", resp.Message)
	}
	if resp.EvalCount != 7 {
		t.Errorf("EvalCount = %d, want output_tokens 7", resp.EvalCount)
	}
	if resp.PromptTokens != 510 {
		t.Errorf("PromptTokens = %d, want 10+400+100 (input_tokens is the uncached remainder only)", resp.PromptTokens)
	}
	if resp.CacheReadTokens != 400 || resp.CacheCreationTokens != 100 {
		t.Errorf("cache counters = %d/%d, want 400/100", resp.CacheReadTokens, resp.CacheCreationTokens)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want end_turn mapped to stop", resp.FinishReason)
	}
	if resp.ProviderRequestID != "msg_01abc" {
		t.Errorf("ProviderRequestID = %q, want the message id", resp.ProviderRequestID)
	}
	if resp.ServedModel != "" || resp.CostUSD != nil || resp.TopLogprobs != nil {
		t.Errorf("openrouter-only / logprob fields must stay zero: served=%q cost=%v lp=%v", resp.ServedModel, resp.CostUSD, resp.TopLogprobs)
	}

	// The cache counters reach llmlog as metadata, nothing else on the row
	// changes (the model column stays the pool's pick).
	entry := llmlog.Entry{Model: "pool-model"}
	ApplyProviderTelemetry(&entry, resp)
	if entry.Model != "pool-model" || entry.CostUSD != nil {
		t.Errorf("entry model/cost disturbed: %q %v", entry.Model, entry.CostUSD)
	}
	if entry.Metadata["cache_read_input_tokens"] != 400 || entry.Metadata["cache_creation_input_tokens"] != 100 {
		t.Errorf("metadata = %v, want both cache counters", entry.Metadata)
	}
	if entry.Metadata["provider_request_id"] != "msg_01abc" {
		t.Errorf("metadata.provider_request_id = %v", entry.Metadata["provider_request_id"])
	}

	// A wire without cache counters leaves the metadata untouched (other
	// wires' rows stay byte-identical).
	plain := llmlog.Entry{}
	ApplyProviderTelemetry(&plain, &ChatResponse{})
	if plain.Metadata != nil {
		t.Errorf("metadata allocated without cache counters: %v", plain.Metadata)
	}
}

// TestAnthropicStopReasonMapping: the Messages API vocabulary folds into the
// one FinishReason consumers compare against (dream reads "length", the chat
// harness reads "tool_calls"); unknown values pass through raw, absent stays
// "".
func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "pause_turn": "stop",
		"max_tokens": "length", "model_context_window_exceeded": "length",
		"tool_use": "tool_calls", "refusal": "content_filter",
		"some_future_reason": "some_future_reason", "": "",
	}
	for in, want := range cases {
		if got := mapAnthropicStopReason(in); got != want {
			t.Errorf("map(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAnthropicErrorClassification: a non-2xx answer carries its status into
// the failover classifier exactly like the other wires — 429 keeps the
// Retry-After cooldown, 529 (overloaded) fails over as a gateway fault, 401
// is auth, 400 is request-specific, 500 stops the chain. The provider's
// error type is folded into the rendered message.
func TestAnthropicErrorClassification(t *testing.T) {
	cases := []struct {
		status  int
		errType string
		hdr     map[string]string
		want    backends.ErrClass
		retry   time.Duration
	}{
		{429, "rate_limit_error", map[string]string{"Retry-After": "7"}, backends.ClassRateLimit, 7 * time.Second},
		{529, "overloaded_error", nil, backends.ClassGateway, 0},
		{401, "authentication_error", nil, backends.ClassAuth, 0},
		{400, "invalid_request_error", nil, backends.ClassBadRequest, 0},
		{500, "api_error", nil, backends.ClassServerFault, 0},
	}
	for _, tc := range cases {
		t.Run(tc.errType, func(t *testing.T) {
			body := `{"type":"error","error":{"type":"` + tc.errType + `","message":"nope"}}`
			rec := newAnthropicRecorder(t, tc.status, body, tc.hdr)
			b := anthropicTestBackend(rec.srv.URL)
			_, err := Chat(context.Background(), b, "s", "u", Options{NumPredict: 5}, 5*time.Second)
			if err == nil {
				t.Fatal("want an error")
			}
			var se *httpx.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("error is not a *httpx.StatusError: %v", err)
			}
			if se.Code != tc.status || se.RetryAfter != tc.retry {
				t.Errorf("StatusError = code %d retry %s, want %d/%s", se.Code, se.RetryAfter, tc.status, tc.retry)
			}
			if se.Body != tc.errType+": nope" {
				t.Errorf("body = %q, want the provider error type folded in", se.Body)
			}
			if got := backends.Classify(err, backends.ProviderGeneric); got != tc.want {
				t.Errorf("Classify = %v, want %v", got, tc.want)
			}
		})
	}
}
