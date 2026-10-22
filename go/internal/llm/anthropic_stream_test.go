// Native Anthropic Messages API wire (anthropic_stream.go), streaming path:
// fixture parsing (text, thinking, tool use, mid-stream error), lenient
// and hard endings, HTTP status errors, the request shape of a multi-turn
// tool round trip, and the ChatStreamBackend dispatch. Fixtures are
// hand-written from the Messages API event reference (claude-api, 2026-10),
// not live captures — what they cannot prove is live-only (cache hits,
// effort effect, 529 behaviour).
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
)

func anthropicStreamBackend(host string) backends.Backend {
	b := anthropicTestBackend(host)
	b.ModelMap = map[string]backends.ModelSpec{"default": {Model: "claude-opus-5-5"}}
	return b
}

func TestAnthropicStreamContentFixture(t *testing.T) {
	srv := serveFixture(t, "anthropic_content.sse")
	defer srv.Close()
	var events []StreamEvent
	res, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "claude-opus-5-5",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 64}, 5*time.Second, collectEvents(&events))
	if err != nil {
		t.Fatalf("ChatStreamBackend: %v", err)
	}
	if res.Content != "Berlin is sunny." {
		t.Errorf("content = %q (thinking deltas must never leak)", res.Content)
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish = %q, want end_turn mapped to stop", res.FinishReason)
	}
	if res.PromptTokens != 312 || res.CompletionTokens != 9 {
		t.Errorf("tokens = %d/%d, want 12+300 prompt (total incl. cache read) and 9 output", res.PromptTokens, res.CompletionTokens)
	}
	if res.CacheReadTokens != 300 || res.CacheCreationTokens != 0 {
		t.Errorf("cache counters = %d/%d, want 300/0", res.CacheReadTokens, res.CacheCreationTokens)
	}
	if res.ProviderRequestID != "msg_01stream" {
		t.Errorf("ProviderRequestID = %q", res.ProviderRequestID)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("tool calls = %v, want none", res.ToolCalls)
	}
	var joined strings.Builder
	finishes := 0
	for i, ev := range events {
		joined.WriteString(ev.ContentDelta)
		if ev.ToolCallName != "" {
			t.Errorf("unexpected tool event %+v", ev)
		}
		if ev.FinishReason != "" {
			finishes++
			if i != len(events)-1 {
				t.Errorf("finish event at %d of %d, want last", i, len(events))
			}
		}
	}
	if joined.String() != res.Content || finishes != 1 {
		t.Errorf("deltas = %q finishes = %d", joined.String(), finishes)
	}
}

func TestAnthropicStreamToolUseFixture(t *testing.T) {
	srv := serveFixture(t, "anthropic_tool_use.sse")
	defer srv.Close()
	var events []StreamEvent
	res, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "claude-opus-5-5",
		[]ChatMsg{{Role: "user", Content: "weather?"}}, nil, Options{NumPredict: 64}, 5*time.Second, collectEvents(&events))
	if err != nil {
		t.Fatalf("ChatStreamBackend: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish = %q, want tool_use mapped to tool_calls (the harness's dispatch key)", res.FinishReason)
	}
	if res.Content != "Let me check." {
		t.Errorf("content = %q", res.Content)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(res.ToolCalls))
	}
	first := res.ToolCalls[0]
	if first.ID != "toolu_01weather" || first.Type != "function" || first.Function.Name != "get_weather" {
		t.Errorf("first call = %+v", first)
	}
	args := argsAsMap(t, first)
	if args["city"] != "Berlin" || args["unit"] != "c" {
		t.Errorf("arguments = %v, want the partial_json fragments joined into one object", args)
	}
	second := res.ToolCalls[1]
	if second.ID != "toolu_02time" || second.Function.Name != "get_time" || string(second.Function.Arguments) != "{}" {
		t.Errorf("second call = %+v, want empty arguments as {}", second)
	}
	if res.PromptTokens != 552 || res.CompletionTokens != 31 || res.CacheCreationTokens != 512 {
		t.Errorf("tokens = %d/%d cache_create=%d, want 40+512 / 31 / 512", res.PromptTokens, res.CompletionTokens, res.CacheCreationTokens)
	}
	var names []string
	for _, ev := range events {
		if ev.ToolCallName != "" {
			names = append(names, ev.ToolCallName)
		}
	}
	if strings.Join(names, ",") != "get_weather,get_time" {
		t.Errorf("tool name events = %v, want one per call in stream order", names)
	}
	if last := events[len(events)-1]; last.FinishReason != "tool_calls" {
		t.Errorf("last event = %+v, want the finish", last)
	}

	// The assembled calls round-trip into the next request as tool_use
	// blocks with the SAME ids, so tool_result blocks can reference them.
	system, msgs := anthropicMessages([]ChatMsg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "weather?"},
		{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls},
		{Role: "tool", Content: "21C", ToolCallID: "toolu_01weather"},
		{Role: "tool", Content: "12:00", ToolCallID: "toolu_02time"},
	}, true)
	if system != "sys" || len(msgs) != 3 {
		t.Fatalf("system=%q msgs=%d, want 3 turns (user, assistant, merged tool results)", system, len(msgs))
	}
	raw, _ := json.Marshal(msgs)
	for _, want := range []string{
		`"role":"assistant"`, `"type":"tool_use","id":"toolu_01weather","name":"get_weather","input":{"city":"Berlin","unit":"c"}`,
		`"type":"tool_use","id":"toolu_02time","name":"get_time","input":{}`,
		`"type":"tool_result","tool_use_id":"toolu_01weather","content":"21C"`,
		`"type":"tool_result","tool_use_id":"toolu_02time","content":"12:00"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("messages lack %s:\n%s", want, raw)
		}
	}
	if msgs[2].Role != "user" {
		t.Errorf("tool results must travel as a user turn, got %q", msgs[2].Role)
	}
}

func TestAnthropicStreamMidStreamError(t *testing.T) {
	srv := serveFixture(t, "anthropic_midstream_error.sse")
	defer srv.Close()
	res, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, nil)
	var se *StreamError
	if !errors.As(err, &se) {
		t.Fatalf("want *StreamError, got %v (res %v)", err, res)
	}
	if se.Code != "overloaded_error" || se.Message != "Overloaded" {
		t.Errorf("StreamError = %+v", se)
	}
	if res != nil {
		t.Errorf("result must be nil on a mid-stream error, got %+v", res)
	}
}

func TestAnthropicStreamEOFWithoutStopIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"half\"}}\n\n"))
	}))
	defer srv.Close()
	_, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "ended unexpectedly") {
		t.Errorf("err = %v, want the unexpected-end error (a dead connection must not look like a finished turn)", err)
	}
}

func TestAnthropicStreamEOFAfterMessageDeltaAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":3}}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":4}}"))
	}))
	defer srv.Close()
	res, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("EOF after message_delta must be accepted: %v", err)
	}
	if res.Content != "done" || res.FinishReason != "length" || res.PromptTokens != 3 || res.CompletionTokens != 4 {
		t.Errorf("result = %+v", res)
	}
}

func TestAnthropicStreamHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	}))
	defer srv.Close()
	_, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, nil)
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Code != 529 {
		t.Fatalf("want *httpx.StatusError 529, got %v", err)
	}
	if got := backends.Classify(err, backends.ProviderGeneric); got != backends.ClassGateway {
		t.Errorf("Classify = %v, want Gateway (529 overloaded fails over)", got)
	}
}

func TestAnthropicStreamCallbackErrorAborts(t *testing.T) {
	srv := serveFixture(t, "anthropic_content.sse")
	defer srv.Close()
	wantErr := errors.New("sink closed")
	calls := 0
	_, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second,
		func(StreamEvent) error { calls++; return wantErr })
	if !errors.Is(err, wantErr) || calls != 1 {
		t.Errorf("err = %v calls = %d, want the callback error after one event", err, calls)
	}
}

// TestAnthropicStreamRequestWire pins the multi-turn tool round trip on the
// wire: headers, stream flag, cached system block, user/assistant/tool
// mapping with consecutive tool results folded into ONE user turn, tool
// definitions as input_schema, effort, and the absence of every field this
// wire must not carry.
func TestAnthropicStreamRequestWire(t *testing.T) {
	srv, body, hdr := captureRequestBody(t, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n"+
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
		"data: {\"type\":\"message_stop\"}\n\n")
	defer srv.Close()

	tools := []ToolDef{
		{Type: "function", Function: ToolDefFunction{
			Name: "get_weather", Description: "Weather for a city",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
		{Type: "function", Function: ToolDefFunction{Name: "get_time"}},
	}
	msgs := []ChatMsg{
		{Role: "system", Content: "You are a weather demo assistant."},
		{Role: "user", Content: "Weather in Berlin?"},
		{Role: "assistant", Content: "Checking.", ToolCalls: []ToolCall{
			{ID: "toolu_1", Type: "function", Function: ToolCallFunction{Name: "get_weather", Arguments: json.RawMessage(`{"city":"Berlin"}`)}},
			{ID: "toolu_2", Type: "function", Function: ToolCallFunction{Name: "get_time", Arguments: json.RawMessage(`not json`)}},
		}},
		{Role: "tool", Content: `{"temperature_c":21}`, ToolCallID: "toolu_1"},
		{Role: "tool", Content: `12:00`, ToolCallID: "toolu_2"},
	}
	b := anthropicStreamBackend(srv.URL)
	b.ExtraBody = map[string]any{"effort": "high"}
	opts := Options{Temperature: 0.7, RepeatPenalty: 1.1, NumPredict: 256}
	if _, err := ChatStreamBackend(context.Background(), b, "claude-opus-5-5", msgs, tools, opts, 5*time.Second, nil); err != nil {
		t.Fatalf("ChatStreamBackend: %v", err)
	}

	if got := hdr.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := hdr.Get("anthropic-version"); got != anthropicVersion {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := hdr.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q", got)
	}
	if hdr.Get("Authorization") != "" {
		t.Error("Authorization header sent on the anthropic wire")
	}

	var req map[string]any
	if err := json.Unmarshal(*body, &req); err != nil {
		t.Fatalf("request body not JSON: %v\n%s", err, *body)
	}
	if req["stream"] != true || req["model"] != "claude-opus-5-5" || req["max_tokens"] != float64(256+anthropicThinkingReserve["high"]) {
		t.Errorf("stream/model/max_tokens = %v/%v/%v", req["stream"], req["model"], req["max_tokens"])
	}
	if oc, _ := req["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config = %v, want extra_body effort high", req["output_config"])
	}
	for _, k := range anthropicForbiddenKeys {
		if v, has := req[k]; has {
			t.Errorf("body carries %q = %v", k, v)
		}
	}
	sys, _ := req["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v", req["system"])
	}
	if blk, _ := sys[0].(map[string]any); blk["text"] != "You are a weather demo assistant." || blk["cache_control"] == nil {
		t.Errorf("system block = %v, want the lifted system row with cache_control", blk)
	}

	turns, _ := req["messages"].([]any)
	if len(turns) != 3 {
		t.Fatalf("messages = %d turns, want user / assistant / merged tool results:\n%s", len(turns), *body)
	}
	role := func(i int) string { m, _ := turns[i].(map[string]any); r, _ := m["role"].(string); return r }
	blocks := func(i int) []any { m, _ := turns[i].(map[string]any); c, _ := m["content"].([]any); return c }
	if role(0) != "user" || role(1) != "assistant" || role(2) != "user" {
		t.Errorf("roles = %s/%s/%s", role(0), role(1), role(2))
	}
	if ub := blocks(0); len(ub) != 1 || ub[0].(map[string]any)["text"] != "Weather in Berlin?" {
		t.Errorf("user turn = %v", ub)
	}
	ab := blocks(1)
	if len(ab) != 3 {
		t.Fatalf("assistant turn = %v, want text + 2 tool_use", ab)
	}
	if tu, _ := ab[1].(map[string]any); tu["type"] != "tool_use" || tu["id"] != "toolu_1" || tu["name"] != "get_weather" {
		t.Errorf("tool_use[0] = %v", tu)
	} else if in, _ := tu["input"].(map[string]any); in["city"] != "Berlin" {
		t.Errorf("tool_use[0].input = %v", tu["input"])
	}
	if tu, _ := ab[2].(map[string]any); tu["id"] != "toolu_2" {
		t.Errorf("tool_use[1] = %v", tu)
	} else if in, _ := tu["input"].(map[string]any); in == nil || len(in) != 0 {
		t.Errorf("tool_use[1].input = %v, want {} for unparseable arguments", tu["input"])
	}
	tr := blocks(2)
	if len(tr) != 2 {
		t.Fatalf("tool-result turn = %v, want both results folded into one user turn", tr)
	}
	if r0, _ := tr[0].(map[string]any); r0["type"] != "tool_result" || r0["tool_use_id"] != "toolu_1" || r0["content"] != `{"temperature_c":21}` {
		t.Errorf("tool_result[0] = %v", r0)
	}
	if r1, _ := tr[1].(map[string]any); r1["tool_use_id"] != "toolu_2" || r1["content"] != "12:00" {
		t.Errorf("tool_result[1] = %v", r1)
	}
	for i := range turns {
		if _, has := turns[i].(map[string]any)["tool_calls"]; has {
			t.Errorf("turn %d carries the OpenAI tool_calls field", i)
		}
	}

	wt, _ := req["tools"].([]any)
	if len(wt) != 2 {
		t.Fatalf("tools = %v", req["tools"])
	}
	if t0, _ := wt[0].(map[string]any); t0["name"] != "get_weather" || t0["description"] != "Weather for a city" || t0["input_schema"] == nil || t0["function"] != nil {
		t.Errorf("tools[0] = %v, want the Messages API shape (name/description/input_schema)", t0)
	}
	if t1, _ := wt[1].(map[string]any); t1["input_schema"] == nil {
		t.Errorf("tools[1] = %v, want an empty object schema for a parameterless tool", t1)
	}
}

// TestChatStreamBackendDispatch: the protocol field, not the host, picks
// the wire — an openai row keeps /v1/chat/completions with Bearer auth
// and its extra_body, an anthropic row takes /v1/messages.
func TestChatStreamBackendDispatch(t *testing.T) {
	type hit struct{ path, auth, key string }
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, hit{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key")})
		if r.URL.Path == "/v1/messages" {
			_, _ = w.Write([]byte("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		_, _ = w.Write([]byte(minimalStopStream))
	}))
	defer srv.Close()
	msgs := []ChatMsg{{Role: "user", Content: "hi"}}

	oa := backends.Backend{Host: srv.URL, Protocol: backends.ProtocolOpenAI, APIKey: "sk-oa"}
	if _, err := ChatStreamBackend(context.Background(), oa, "m", msgs, nil, Options{NumPredict: 4}, 5*time.Second, nil); err != nil {
		t.Fatalf("openai: %v", err)
	}
	ol := backends.Backend{Host: srv.URL, Protocol: backends.ProtocolOllama}
	if _, err := ChatStreamBackend(context.Background(), ol, "m", msgs, nil, Options{NumPredict: 4}, 5*time.Second, nil); err != nil {
		t.Fatalf("ollama: %v", err)
	}
	if _, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m", msgs, nil, Options{NumPredict: 4}, 5*time.Second, nil); err != nil {
		t.Fatalf("anthropic: %v", err)
	}
	want := []hit{
		{"/v1/chat/completions", "Bearer sk-oa", ""},
		{"/v1/chat/completions", "", ""},
		{"/v1/messages", "", "sk-ant-test"},
	}
	if len(hits) != len(want) {
		t.Fatalf("hits = %v", hits)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Errorf("hit %d = %+v, want %+v", i, hits[i], want[i])
		}
	}
}

// TestAnthropicMessagesEdgeCases: consecutive user turns fold, empty
// assistant turns drop, several system rows join.
func TestAnthropicMessagesEdgeCases(t *testing.T) {
	system, msgs := anthropicMessages([]ChatMsg{
		{Role: "system", Content: "a"},
		{Role: "system", Content: "b"},
		{Role: "user", Content: "one"},
		{Role: "user", Content: "two"},
		{Role: "assistant", Content: ""},
		{Role: "user", Content: "three"},
	}, true)
	if system != "a\n\nb" {
		t.Errorf("system = %q", system)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("msgs = %+v, want one folded user turn (empty assistant dropped)", msgs)
	}
	if blocks, _ := msgs[0].Content.([]any); len(blocks) != 3 {
		t.Errorf("folded user turn = %v, want three text blocks", msgs[0].Content)
	}
}

// --- review follow-ups (adversarial pass, 2026-10-11) ---.

// TestAnthropicStreamMessageDeltaUsageKeepsCacheCounters: a message_delta
// that repeats input_tokens but omits the cache fields (older servers,
// proxies) must not zero the counters message_start reported.
func TestAnthropicStreamMessageDeltaUsageKeepsCacheCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":400,\"cache_creation_input_tokens\":50}}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":4}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()
	res, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.CacheReadTokens != 400 || res.CacheCreationTokens != 50 || res.PromptTokens != 12+400+50 || res.CompletionTokens != 4 {
		t.Errorf("usage = prompt %d cache %d/%d out %d, want 462 / 400/50 / 4", res.PromptTokens, res.CacheReadTokens, res.CacheCreationTokens, res.CompletionTokens)
	}
}

// TestAnthropicStreamNamelessToolUseDropped: a tool_use block start without
// a name (proxy artefact) yields no call and no event; its input deltas
// fall on no pending call.
func TestAnthropicStreamNamelessToolUseDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"\",\"name\":\"\",\"input\":{}}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_ok\",\"name\":\"get_time\",\"input\":{}}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()
	var events []StreamEvent
	res, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, collectEvents(&events))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "toolu_ok" {
		t.Errorf("tool calls = %+v, want only the named one", res.ToolCalls)
	}
	names := 0
	for _, ev := range events {
		if ev.ToolCallName != "" {
			names++
		}
	}
	if names != 1 {
		t.Errorf("tool name events = %d, want 1", names)
	}
}

// TestAnthropicMessagesRepairsEmptyToolIDs: a history served earlier by an
// openai-wire row (llama.cpp streams calls without ids) replays with
// synthetic, matching ids instead of a 400 on tool_use.id.
func TestAnthropicMessagesRepairsEmptyToolIDs(t *testing.T) {
	_, msgs := anthropicMessages([]ChatMsg{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{Function: ToolCallFunction{Name: "a", Arguments: json.RawMessage(`{}`)}},
			{ID: "real", Function: ToolCallFunction{Name: "b", Arguments: json.RawMessage(`{}`)}},
			{Function: ToolCallFunction{Name: "c", Arguments: json.RawMessage(`{}`)}},
		}},
		{Role: "tool", Content: "ra"},
		{Role: "tool", Content: "rb", ToolCallID: "real"},
		{Role: "tool", Content: "rc"},
	}, true)
	raw, _ := json.Marshal(msgs)
	s := string(raw)
	for _, want := range []string{
		`"type":"tool_use","id":"ctxcall_1","name":"a"`, `"type":"tool_use","id":"real","name":"b"`, `"type":"tool_use","id":"ctxcall_2","name":"c"`,
		`"tool_use_id":"ctxcall_1","content":"ra"`, `"tool_use_id":"real","content":"rb"`, `"tool_use_id":"ctxcall_2","content":"rc"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("messages lack %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"id":""`) || strings.Contains(s, `"tool_use_id":""`) {
		t.Errorf("an empty id reached the wire:\n%s", s)
	}
}

// TestAnthropicStreamThinkingBlocksReplayFirst: the fixture's thinking block
// (empty text, signature — the default display) is re-assembled into
// StreamResult.ProviderBlocks and placed FIRST in the replayed assistant
// turn, ahead of text and tool_use — the order the API produced them in,
// which the reference asks to be kept while a tool_use awaits its result.
func TestAnthropicStreamThinkingBlocksReplayFirst(t *testing.T) {
	srv := serveFixture(t, "anthropic_tool_use.sse")
	defer srv.Close()
	res, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "weather?"}}, nil, Options{NumPredict: 64}, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ProviderBlocks) != 1 {
		t.Fatalf("ProviderBlocks = %d, want the one thinking block", len(res.ProviderBlocks))
	}
	if got := string(res.ProviderBlocks[0]); got != `{"type":"thinking","thinking":"","signature":"sigABC"}` {
		t.Errorf("thinking block = %s", got)
	}
	if res.Content != "Let me check." || len(res.ToolCalls) != 2 {
		t.Errorf("content/calls disturbed: %q / %d", res.Content, len(res.ToolCalls))
	}

	_, msgs := anthropicMessages([]ChatMsg{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls, ProviderBlocks: res.ProviderBlocks},
		{Role: "tool", Content: "21C", ToolCallID: "toolu_01weather"},
		{Role: "tool", Content: "12:00", ToolCallID: "toolu_02time"},
	}, true)
	blocks, _ := msgs[1].Content.([]any)
	if len(blocks) != 4 {
		t.Fatalf("assistant turn = %d blocks, want thinking + text + 2 tool_use", len(blocks))
	}
	first, _ := json.Marshal(blocks[0])
	if !strings.HasPrefix(string(first), `{"type":"thinking"`) {
		t.Errorf("first block = %s, want the thinking block", first)
	}

	// The openai wire never sees the field.
	raw, _ := json.Marshal(ChatMsg{Role: "assistant", Content: "x", ProviderBlocks: res.ProviderBlocks})
	if strings.Contains(string(raw), "thinking") {
		t.Errorf("ProviderBlocks leaked into the OpenAI message object: %s", raw)
	}
}

// TestAnthropicStreamRedactedThinkingCarried: a redacted_thinking block
// arrives complete on its start frame and is replayed as-is.
func TestAnthropicStreamRedactedThinkingCarried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"EmwKAhgB\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()
	res, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ProviderBlocks) != 1 || string(res.ProviderBlocks[0]) != `{"type":"redacted_thinking","data":"EmwKAhgB"}` {
		t.Errorf("ProviderBlocks = %v", res.ProviderBlocks)
	}
}

// TestAnthropicMessagesRepairsOrphanToolUse: a tool_use the history never
// answered (interrupted turn) gets a synthetic is_error tool_result before
// the next user text, and at the end of the history — otherwise every later
// turn of the session would 400 on this wire.
func TestAnthropicMessagesRepairsOrphanToolUse(t *testing.T) {
	_, msgs := anthropicMessages([]ChatMsg{
		{Role: "user", Content: "q1"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "A", Function: ToolCallFunction{Name: "a", Arguments: json.RawMessage(`{}`)}},
			{ID: "B", Function: ToolCallFunction{Name: "b", Arguments: json.RawMessage(`{}`)}},
		}},
		{Role: "tool", Content: "ra", ToolCallID: "A"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "C", Function: ToolCallFunction{Name: "c", Arguments: json.RawMessage(`{}`)}}}},
	}, true)
	if len(msgs) != 5 {
		t.Fatalf("turns = %d, want user / assistant / user[ra, B-orphan, q2] / assistant / user[C-orphan]", len(msgs))
	}
	raw, _ := json.Marshal(msgs[2].Content)
	s := string(raw)
	if !strings.Contains(s, `"tool_use_id":"A","content":"ra"`) ||
		!strings.Contains(s, `"tool_use_id":"B","content":"`+orphanToolResult+`","is_error":true`) ||
		!strings.HasSuffix(s, `{"type":"text","text":"q2"}]`) {
		t.Errorf("third turn = %s", s)
	}
	last, _ := json.Marshal(msgs[4].Content)
	if msgs[4].Role != "user" || !strings.Contains(string(last), `"tool_use_id":"C"`) || !strings.Contains(string(last), `"is_error":true`) {
		t.Errorf("trailing orphan not repaired: %s %s", msgs[4].Role, last)
	}
	// An answered call gets no synthetic result.
	if strings.Count(s, `"tool_use_id":"A"`) != 1 {
		t.Errorf("answered call duplicated: %s", s)
	}
}

// TestAnthropicStreamTopLevelCacheControl: the multi-turn stream path asks
// for the automatic breakpoint on the conversation tail.
func TestAnthropicStreamTopLevelCacheControl(t *testing.T) {
	srv, body, _ := captureRequestBody(t, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	defer srv.Close()
	if _, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(*body, &req)
	if cc, _ := req["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("top-level cache_control = %v, want {type: ephemeral}", req["cache_control"])
	}
}

// TestStreamErrorClassifiesLikeItsStatus: the anthropic error types inside an
// HTTP-200 stream map onto the classes the equivalent status codes get.
func TestStreamErrorClassifiesLikeItsStatus(t *testing.T) {
	cases := map[string]backends.ErrClass{
		"overloaded_error": backends.ClassGateway, "rate_limit_error": backends.ClassRateLimit,
		"authentication_error": backends.ClassAuth, "invalid_request_error": backends.ClassBadRequest,
		"api_error": backends.ClassServerFault, "502": backends.ClassServerFault, "": backends.ClassServerFault,
	}
	for code, want := range cases {
		err := fmt.Errorf("wrapped: %w", &StreamError{Code: code, Message: "m"})
		if got := backends.Classify(err, backends.ProviderGeneric); got != want {
			t.Errorf("Classify(StreamError %q) = %v, want %v", code, got, want)
		}
	}
}

// --- closing call and tool-less requests (review finding 1) ---.

// TestAnthropicStreamToolChoiceNone: Options.ToolChoiceNone keeps the tools
// array and adds tool_choice {"type":"none"} (accepted on Claude Opus 5.5 —
// platform.claude.com/docs/en/api/errors#forced-tool-use-not-supported); it
// is a no-op on a request without tools (there is nothing to choose from).
func TestAnthropicStreamToolChoiceNone(t *testing.T) {
	tools := []ToolDef{{Type: "function", Function: ToolDefFunction{Name: "get_time"}}}
	msgs := []ChatMsg{{Role: "user", Content: "hi"}}
	for _, tc := range []struct {
		tools      []ToolDef
		wantChoice bool
	}{{tools, true}, {nil, false}} {
		srv, body, _ := captureRequestBody(t, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
		if _, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
			msgs, tc.tools, Options{NumPredict: 8, ToolChoiceNone: true}, 5*time.Second, nil); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		var req map[string]any
		_ = json.Unmarshal(*body, &req)
		choice, has := req["tool_choice"].(map[string]any)
		if tc.wantChoice && (!has || choice["type"] != "none") {
			t.Errorf("tool_choice = %v, want {type: none} next to the tools array", req["tool_choice"])
		}
		if !tc.wantChoice && req["tool_choice"] != nil {
			t.Errorf("tool_choice = %v on a request without tools", req["tool_choice"])
		}
		if tc.wantChoice && req["tools"] == nil {
			t.Error("tools array dropped although tool_choice none was asked for")
		}
	}
}

// TestAnthropicMessagesWithoutToolsRendersText: a request that carries no
// tool definitions carries no tool_use / tool_result blocks and no thinking
// blocks either — the calls and results are rendered as text in their turns,
// empty-id calls included, and the turns still alternate.
func TestAnthropicMessagesWithoutToolsRendersText(t *testing.T) {
	system, msgs := anthropicMessages([]ChatMsg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "Checking.", ToolCalls: []ToolCall{
			{ID: "A", Function: ToolCallFunction{Name: "get_weather", Arguments: json.RawMessage(`{"city":"Berlin"}`)}},
			{Function: ToolCallFunction{Name: "get_time", Arguments: json.RawMessage(`{}`)}},
		}, ProviderBlocks: []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"","signature":"sigX"}`)}},
		{Role: "tool", Content: "21C", ToolCallID: "A"},
		{Role: "tool", Content: "12:00"},
		{Role: "assistant", Content: "Berlin: 21C at noon."},
		{Role: "user", Content: "thanks"},
	}, false)
	if system != "sys" {
		t.Errorf("system = %q", system)
	}
	raw, _ := json.Marshal(msgs)
	s := string(raw)
	for _, banned := range []string{`"tool_use"`, `"tool_result"`, `"thinking"`, `sigX`} {
		if strings.Contains(s, banned) {
			t.Errorf("tool-less request carries %s:\n%s", banned, s)
		}
	}
	if len(msgs) != 5 {
		t.Fatalf("turns = %d, want user / assistant / user[both results] / assistant / user:\n%s", len(msgs), s)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Errorf("turns %d/%d share role %q", i-1, i, msgs[i].Role)
		}
	}
	for _, want := range []string{"Checking.", "get_weather", `{\"city\":\"Berlin\"}`, "get_time", "21C", "12:00", "thanks"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered history lacks %s:\n%s", want, s)
		}
	}
}

// TestAnthropicStreamMaxTokensWithoutTextIsAnError: the stream twin — a
// stream that ends at max_tokens with no text and no tool call is the
// thinking-ate-the-budget case, an error rather than an empty chat answer.
func TestAnthropicStreamMaxTokensWithoutTextIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":3}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":40}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()
	_, err := ChatStreamBackend(context.Background(), anthropicStreamBackend(srv.URL), "m",
		[]ChatMsg{{Role: "user", Content: "hi"}}, nil, Options{NumPredict: 8}, 5*time.Second, nil)
	if !errors.Is(err, ErrOutputLimitNoText) {
		t.Fatalf("err = %v, want ErrOutputLimitNoText", err)
	}
}
