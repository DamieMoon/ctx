// Live smoke of the anthropic wire against api.anthropic.com. Skipped under
// -short and without CTX_ANTHROPIC_SMOKE_KEY, so CI never spends tokens —
// note the integration job runs WITHOUT -short, so there the gate is the
// absent variable alone (nothing in this file does work before the skip).
// Run it by hand from a shell that holds the key:
//
//	CTX_ANTHROPIC_SMOKE_KEY=sk-ant-… go test ./internal/llm/ -run Live -count=1 -v
//
// It proves what the fakes cannot: the request shape is accepted by the real
// endpoint, the prompt cache hits on the second call, the thinking block is
// replayed correctly through a tool round trip, effort is accepted, the
// 1-token probe's behaviour, and the 401 classification. Each probe logs the
// usage it saw. Cost: a few thousand tokens per run.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/promptguard"
)

func liveAnthropicBackend(t *testing.T) backends.Backend {
	t.Helper()
	if testing.Short() {
		t.Skip("live smoke skipped under -short")
	}
	key := os.Getenv("CTX_ANTHROPIC_SMOKE_KEY")
	if key == "" {
		t.Skip("CTX_ANTHROPIC_SMOKE_KEY not set")
	}
	model := os.Getenv("CTX_ANTHROPIC_SMOKE_MODEL")
	if model == "" {
		model = "claude-opus-5-5"
	}
	host := os.Getenv("CTX_ANTHROPIC_SMOKE_HOST")
	if host == "" {
		host = "https://api.anthropic.com"
	}
	return backends.Backend{
		Host: host, Protocol: backends.ProtocolAnthropic, ProviderClass: backends.ProviderGeneric,
		Model: model, Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal,
		APIKey: key, APIKeyRef: "smoke", NumCtx: 100000,
		ModelMap: map[string]backends.ModelSpec{"default": {Model: model}},
	}
}

// liveSystemPrompt is a stable prefix comfortably above the 512-token cache
// minimum, followed by the per-call promptguard rule exactly as the
// pipelines send it.
func liveSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are ctx, a knowledge store assistant. Answer briefly and only from the sources given. ")
	for i := 0; i < 60; i++ {
		sb.WriteString("Rule ")
		sb.WriteString(strings.Repeat("alpha beta gamma delta ", 3))
		sb.WriteString("applies to every answer; cite the source id in brackets. ")
	}
	sb.WriteString(promptguard.Rule(promptguard.NewNonce()))
	return sb.String()
}

func TestAnthropicLiveNonStreamAndCache(t *testing.T) {
	b := liveAnthropicBackend(t)
	ctx := context.Background()
	opts := SynthesisOptions(100000) // the production preset: temperature, repeat penalty (both dropped), NumPredict 500
	first, err := Chat(ctx, b, liveSystemPrompt(), "Reply with exactly: ok", opts, 60*time.Second)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	t.Logf("first: finish=%q out=%d prompt=%d cache_read=%d cache_create=%d id=%s content=%q",
		first.FinishReason, first.EvalCount, first.PromptTokens, first.CacheReadTokens, first.CacheCreationTokens, first.ProviderRequestID, first.Message.Content)
	if first.Message.Content == "" || first.EvalCount == 0 {
		t.Errorf("empty answer: %+v", first)
	}
	second, err := Chat(ctx, b, liveSystemPrompt(), "Reply with exactly: ok again", opts, 60*time.Second)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	t.Logf("second: finish=%q out=%d prompt=%d cache_read=%d cache_create=%d",
		second.FinishReason, second.EvalCount, second.PromptTokens, second.CacheReadTokens, second.CacheCreationTokens)
	if second.CacheReadTokens == 0 {
		t.Errorf("no cache hit on the second call with a byte-identical stable prefix (fresh nonce only): cache_read=0, cache_create=%d", second.CacheCreationTokens)
	}

	// JSON mode is a no-op on this wire: the same call must still succeed.
	j, err := ChatJSON(ctx, b, "Answer with a JSON object {\"ok\": true}.", "Go.", Options{NumPredict: 200}, 60*time.Second)
	if err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	t.Logf("json-mode no-op: finish=%q content=%q", j.FinishReason, j.Message.Content)
}

func TestAnthropicLiveEffortAndProbe(t *testing.T) {
	b := liveAnthropicBackend(t)
	ctx := context.Background()
	for _, eff := range []string{"low", "high"} {
		r, err := Chat(ctx, b, "Answer in one word.", "What colour is the sky on a clear day?",
			Options{NumPredict: 300, Extra: map[string]any{"effort": eff}}, 60*time.Second)
		if err != nil {
			t.Fatalf("effort %s: %v", eff, err)
		}
		t.Logf("effort=%s finish=%q out=%d content=%q", eff, r.FinishReason, r.EvalCount, r.Message.Content)
	}
	// The backend-test chat probe shape: max_tokens 1 on a model that thinks.
	p, err := Chat(ctx, b, "You are a connectivity probe.", "Reply with: ok", Options{Temperature: 0, NumPredict: 1}, 30*time.Second)
	if err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) {
			t.Logf("probe max_tokens=1: HTTP %d %s (the probe would report %s)", se.Code, se.Body, backends.Classify(err, backends.ProviderGeneric))
		} else {
			t.Fatalf("probe: %v", err)
		}
	} else {
		t.Logf("probe max_tokens=1: finish=%q out=%d content=%q", p.FinishReason, p.EvalCount, p.Message.Content)
	}
}

func TestAnthropicLiveStreamToolRoundTrip(t *testing.T) {
	b := liveAnthropicBackend(t)
	ctx := context.Background()
	tool := ToolDef{Type: "function", Function: ToolDefFunction{
		Name: "get_weather", Description: "Current weather for a city",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}
	// A short tool turn may come back without any thinking block (adaptive
	// thinking skips trivial turns — observed live 2026-10-11), which would
	// leave the replay path unexercised. Ask for visible reasoning and a
	// question that needs a step of deliberation before the call.
	b.ExtraBody = map[string]any{"thinking": map[string]any{"type": "adaptive", "display": "summarized"}}
	msgs := []ChatMsg{
		{Role: "system", Content: "You are a weather assistant. Use the get_weather tool to answer weather questions; never guess. Think about which city the user means before calling the tool."},
		{Role: "user", Content: "I am in the capital of Germany. Is it warm enough for a t-shirt outside right now? Use the tool."},
	}
	var events []StreamEvent
	res, err := ChatStreamBackend(ctx, b, b.Model, msgs, []ToolDef{tool}, Options{NumPredict: 2048, Extra: map[string]any{"effort": "high"}}, 90*time.Second, collectEvents(&events))
	if err != nil {
		t.Fatalf("first stream: %v", err)
	}
	t.Logf("first stream: finish=%q calls=%d blocks=%d out=%d prompt=%d cache_read=%d cache_create=%d content=%q",
		res.FinishReason, len(res.ToolCalls), len(res.ProviderBlocks), res.CompletionTokens, res.PromptTokens, res.CacheReadTokens, res.CacheCreationTokens, res.Content)
	if res.FinishReason != "tool_calls" || len(res.ToolCalls) == 0 {
		t.Fatalf("expected a tool call, got finish=%q calls=%+v", res.FinishReason, res.ToolCalls)
	}
	for _, blk := range res.ProviderBlocks {
		t.Logf("provider block: %.120s", blk)
	}
	call := res.ToolCalls[0]
	args := argsAsMap(t, call)
	t.Logf("tool call: id=%s name=%s args=%v", call.ID, call.Function.Name, args)

	// Round trip: the assistant turn WITH its thinking block, then the result.
	msgs = append(msgs,
		ChatMsg{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls, ProviderBlocks: res.ProviderBlocks},
		ChatMsg{Role: "tool", Content: `{"temperature_c": 21, "sky": "clear"}`, ToolCallID: call.ID},
	)
	events = events[:0]
	final, err := ChatStreamBackend(ctx, b, b.Model, msgs, []ToolDef{tool}, Options{NumPredict: 1024}, 90*time.Second, collectEvents(&events))
	if err != nil {
		t.Fatalf("second stream (thinking replay): %v", err)
	}
	t.Logf("second stream: finish=%q out=%d prompt=%d cache_read=%d cache_create=%d content=%q",
		final.FinishReason, final.CompletionTokens, final.PromptTokens, final.CacheReadTokens, final.CacheCreationTokens, final.Content)
	if final.FinishReason != "stop" || !strings.Contains(final.Content, "21") {
		t.Errorf("expected a final answer quoting the tool result, got finish=%q content=%q", final.FinishReason, final.Content)
	}
	if final.CacheReadTokens == 0 {
		t.Logf("note: no cache read on the second iteration (prefix below the 512-token minimum is expected for this tiny prompt)")
	}
	if len(res.ProviderBlocks) == 0 {
		t.Logf("REPLAY PATH NOT EXERCISED: the first turn carried no thinking block, so the round trip above and the one below are identical requests")
		return
	}

	// The same round trip WITHOUT the thinking block is what the wire did
	// before the review fix — record what the API says about a STRIPPED
	// block today (only meaningful when one was produced, see above).
	noThinking := append([]ChatMsg{}, msgs[:2]...)
	noThinking = append(noThinking,
		ChatMsg{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls},
		ChatMsg{Role: "tool", Content: `{"temperature_c": 21, "sky": "clear"}`, ToolCallID: call.ID},
	)
	if _, err := ChatStreamBackend(ctx, b, b.Model, noThinking, []ToolDef{tool}, Options{NumPredict: 256}, 90*time.Second, nil); err != nil {
		t.Logf("round trip without the thinking block: %v (expected on the Claude 5 models — this is why ProviderBlocks exist)", err)
	} else {
		t.Logf("round trip without the thinking block: accepted by the API today")
	}
}

func TestAnthropicLiveBadKeyClassifiesAuth(t *testing.T) {
	b := liveAnthropicBackend(t)
	b.APIKey = "sk-ant-invalid-smoke"
	_, err := Chat(context.Background(), b, "s", "Reply with: ok", Options{NumPredict: 5}, 30*time.Second)
	if err == nil {
		t.Fatal("a bad key must fail")
	}
	cls := backends.Classify(err, backends.ProviderGeneric)
	t.Logf("bad key: %v → %s", err, cls)
	if cls != backends.ClassAuth {
		t.Errorf("Classify = %s, want auth", cls)
	}
}
