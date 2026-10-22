// The anthropic-wire half of the closing-call contract (H13a pins the
// openai/ollama half in final_call_no_tools_test.go).
//
// On the Messages API a replayed thinking block is accepted only while the
// system prompt, the tools and the earlier messages it was produced under are
// unchanged (platform.claude.com/docs/en/api/errors#thinking-block-no-longer-matches-the-conversation;
// enforced with a 400 for accounts created on or after 2026-08-31). The tool
// loop carries the last iteration's thinking block in memory into the next
// call, so two request shapes must not drop the tools array silently:
//
//  1. the closing call (finalCall) on a full-trust anthropic row keeps the
//     loop's tools array byte-identical and adds tool_choice {"type":"none"}
//     — accepted on Claude Opus 5.5 / Sonnet 5.5 (errors#forced-tool-use-not-supported:
//     "auto (the default) and none are accepted"), and tool_choice is outside
//     the checked prefix (build-with-claude/preserved-thinking);
//  2. a row below full trust never receives ctx's tool definitions, so its
//     request renders the tool history as text and carries no thinking block
//     at all (removing every thinking block keeps the request valid —
//     preserved-thinking, "or all of them").
//
// Same DB-free shape as H13a: the fake upstream answers 502, the outcome stays
// unserved and ends in finishUnserved (error event, no database).
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

// toolTurnHistory is the in-memory state finalCall sees after one tool
// iteration on the anthropic wire: the assistant tool call with its thinking
// block riding along (ProviderBlocks), then the tool's result.
func toolTurnHistory() []llm.ChatMsg {
	return []llm.ChatMsg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "what is x?"},
		{
			Role: "assistant",
			ToolCalls: []llm.ToolCall{{ID: "toolu_1", Type: "function", Function: llm.ToolCallFunction{
				Name: "ctx_query", Arguments: json.RawMessage(`{"query":"x"}`),
			}}},
			ProviderBlocks: []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"","signature":"sigX"}`)},
		},
		{Role: "tool", Content: `{"ok":true,"hits":1}`, ToolCallID: "toolu_1"},
	}
}

func anthropicEngine(b backends.Backend) *Engine {
	return &Engine{
		provider: staticProvider{chain: []backends.Backend{b}},
		exec:     NewExecutor(nil, nil, 0),
		cfg:      Config{}.withDefaults(),
		now:      time.Now,
	}
}

func runFinalCall(t *testing.T, e *Engine, msgs []llm.ChatMsg) {
	t.Helper()
	err := e.finalCall(context.Background(), &store.ChatSession{ID: "sess-fa", Scope: "private"},
		msgs, true, backends.SensPublic, backends.SensPublic, backends.SensPublic,
		time.Now(), "key-fa", "tool_limit",
		interactiveStreamAdmission(t, newStreamRecAdmitter(t)), newEventSink())
	if err != nil {
		t.Fatalf("finalCall: %v", err)
	}
}

// TestFinalCallAnthropicKeepsToolsWithChoiceNone: the closing call on a
// full-trust anthropic row carries the SAME tools array as a loop iteration
// (the binding prefix) plus tool_choice none, and still replays the thinking
// block of the last iteration.
func TestFinalCallAnthropicKeepsToolsWithChoiceNone(t *testing.T) {
	loopCap, closeCap := &bodyCapture{}, &bodyCapture{}
	loopSrv, closeSrv := loopCap.server(t), closeCap.server(t)

	// Reference: what a regular loop iteration sends on the same row.
	eLoop := anthropicEngine(invariantBackend(loopSrv.URL, backends.ProtocolAnthropic))
	so := eLoop.runStream(context.Background(), []backends.Backend{invariantBackend(loopSrv.URL, backends.ProtocolAnthropic)},
		toolTurnHistory(), true, false, 0, 1, "sess-fa", "key-fa", backends.SensPublic,
		interactiveStreamAdmission(t, newStreamRecAdmitter(t)), newEventSink())
	if so.served {
		t.Fatal("fake backend answered 502 — the outcome must stay unserved")
	}
	loopTools, err := json.Marshal(loopCap.only(t)["tools"])
	if err != nil {
		t.Fatal(err)
	}

	runFinalCall(t, anthropicEngine(invariantBackend(closeSrv.URL, backends.ProtocolAnthropic)), toolTurnHistory())
	body := closeCap.only(t)

	arr, ok := body["tools"].([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("closing call carried tools = %v, want the loop's tools array (the thinking block's binding prefix)", body["tools"])
	}
	closeTools, _ := json.Marshal(body["tools"])
	if !bytes.Equal(loopTools, closeTools) {
		t.Errorf("closing tools differ from the loop's:\nloop:  %s\nclose: %s", loopTools, closeTools)
	}
	if tc, _ := body["tool_choice"].(map[string]any); tc["type"] != "none" || len(tc) != 1 {
		t.Errorf("tool_choice = %v, want {\"type\":\"none\"}", body["tool_choice"])
	}
	raw, _ := json.Marshal(body["messages"])
	for _, want := range []string{`"signature":"sigX"`, `"type":"tool_use"`, `"type":"tool_result"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("closing messages lack %s:\n%s", want, raw)
		}
	}
}

// TestAnthropicBelowFullTrustRendersToolHistoryAsText: a row below full
// trust gets neither ctx's tool definitions nor tool blocks nor thinking
// blocks — in a loop iteration and in the closing call alike. The tool call
// and its result survive as text, so the model still sees the material.
func TestAnthropicBelowFullTrustRendersToolHistoryAsText(t *testing.T) {
	for _, closing := range []bool{false, true} {
		capt := &bodyCapture{}
		srv := capt.server(t)
		b := invariantBackend(srv.URL, backends.ProtocolAnthropic)
		b.Trust = backends.TrustNoCredentials
		e := anthropicEngine(b)
		if closing {
			runFinalCall(t, e, toolTurnHistory())
		} else {
			so := e.runStream(context.Background(), []backends.Backend{b}, toolTurnHistory(), true, false, 0, 1,
				"sess-fa", "key-fa", backends.SensPublic, interactiveStreamAdmission(t, newStreamRecAdmitter(t)), newEventSink())
			if so.served {
				t.Fatal("fake backend answered 502 — the outcome must stay unserved")
			}
		}
		body := capt.only(t)
		for _, k := range []string{"tools", "tool_choice"} {
			if v, has := body[k]; has {
				t.Errorf("closing=%v: body carries %q = %v on a row below full trust", closing, k, v)
			}
		}
		raw, _ := json.Marshal(body["messages"])
		for _, banned := range []string{`"tool_use"`, `"tool_result"`, `"thinking"`, `sigX`} {
			if strings.Contains(string(raw), banned) {
				t.Errorf("closing=%v: messages carry %s:\n%s", closing, banned, raw)
			}
		}
		var turns []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &turns); err != nil {
			t.Fatalf("closing=%v: messages: %v\n%s", closing, err, raw)
		}
		var text strings.Builder
		for i, turn := range turns {
			if i > 0 && turns[i-1].Role == turn.Role {
				t.Errorf("closing=%v: turns %d/%d share role %q (alternation)", closing, i-1, i, turn.Role)
			}
			for _, blk := range turn.Content {
				text.WriteString(blk.Text)
			}
		}
		for _, want := range []string{"ctx_query", `{"query":"x"}`, `{"ok":true,"hits":1}`} {
			if !strings.Contains(text.String(), want) {
				t.Errorf("closing=%v: rendered history lacks %s:\n%s", closing, want, text.String())
			}
		}
	}
}
