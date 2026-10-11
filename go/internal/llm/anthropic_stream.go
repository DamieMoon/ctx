// Streaming chat with tool use over the native Anthropic Messages API
// (POST /v1/messages, stream:true) — the ProtocolAnthropic leg of
// ChatStreamBackend, feeding the SAME StreamEvent/StreamResult contract the
// web-chat harness (chat/engine.go) consumes from the openai wire.
//
// Wire shape (claude-api reference, 2026-10): every data frame carries its
// own "type", so the parser keys on that and ignores the SSE event: lines
// (same information, and keying on the payload survives a proxy that drops
// the event field). Frames handled:
//   - message_start      → usage (input + cache counters; the prompt total)
//   - content_block_start→ tool_use: new pending call, ToolCallName event;
//     text/thinking: nothing
//   - content_block_delta→ text_delta: ContentDelta; input_json_delta:
//     arguments fragment; thinking_delta /
//     signature_delta: accumulated into the replay
//     block, never surfaced to the sink
//   - message_delta      → stop_reason (mapped, FinishReason event) and
//     usage.output_tokens
//   - message_stop       → done
//   - error              → *StreamError (HTTP 200, mid-stream — the same
//     class OpenRouter reports)
//   - ping               → ignored
//
// Message mapping ([]ChatMsg → Messages API): system rows lift into the
// cached top-level system block; an assistant row with tool calls becomes
// [text?, tool_use…] blocks; a tool row becomes a user turn carrying a
// tool_result block, and consecutive same-role turns fold into ONE message
// (the API requires user/assistant alternation — the harness appends one
// tool row per call, so a parallel-call turn yields several tool rows in
// a row). ToolDef (OpenAI function envelope) becomes {name, description,
// input_schema}. tool_choice is never sent (forced choice is a 400 on
// Claude Opus 5.5; auto is also what buildStreamBody sends).
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
)

// --- request mapping ---.

// anthropicToolUseBlock / anthropicToolResultBlock / anthropicTextBlock are
// the content blocks of the chat harness's turns.
type anthropicToolUseBlock struct {
	Type  string          `json:"type"` // "tool_use"
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anthropicToolResultBlock struct {
	Type      string `json:"type"` // "tool_result"
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// anthropicThinkingBlock / anthropicRedactedThinkingBlock are the reasoning
// blocks the stream parser re-assembles so the harness can replay them
// unchanged. The claude-api reference asks that an assistant turn whose
// tool_use still awaits its tool_result keep its thinking block, and the
// preserved-thinking check rejects a block that was ALTERED; the text is
// empty under the default display and the signature carries the
// reasoning. Live (claude-opus-5-5, 2026-10-11) a short tool-call turn
// came back with no thinking block at all and the block-less round trip
// was accepted — so this is the reference's rule honoured, not a 400
// observed (what the API does with a STRIPPED block was not exercised).
type anthropicThinkingBlock struct {
	Type      string `json:"type"` // "thinking"
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type anthropicRedactedThinkingBlock struct {
	Type string `json:"type"` // "redacted_thinking"
	Data string `json:"data"`
}

// orphanToolResult is the synthetic answer for a tool_use the history never
// answered (the turn was interrupted between the model's call and the
// tool's result — client gone, context cancelled). The API rejects an
// assistant tool_use that is not followed by its tool_result, and the
// orphan sits in the persisted history for the rest of the session, so
// without this repair every later turn of that session would 400.
const orphanToolResult = "no result recorded: the turn was interrupted before this tool ran"

// anthropicMessages converts the multi-turn history: the joined system text
// and the alternating message list. Empty assistant turns without tool
// calls are dropped (the API rejects empty content), and an assistant turn
// that would end the list is kept — the harness never ends on one (it
// appends tool results), and a trailing assistant turn is a prefill the
// Claude 5 models reject with a clear 400 rather than something to mask.
//
// Every system row is lifted into the top-level system block — the harness
// puts exactly one, first. A mid-history system row would be moved ahead of
// the conversation (and invalidate the cached prefix); if the harness ever
// injects such rows they belong in messages as role:system turns instead.
func anthropicMessages(msgs []ChatMsg) (system string, out []anthropicMessage) {
	var sys []string
	appendBlocks := func(role string, blocks ...any) {
		if n := len(out); n > 0 && out[n-1].Role == role {
			existing, _ := out[n-1].Content.([]any)
			out[n-1].Content = append(existing, blocks...)
			return
		}
		out = append(out, anthropicMessage{Role: role, Content: blocks})
	}
	// Tool-call ids are mandatory on this wire (tool_use.id and the
	// matching tool_result.tool_use_id). A history served earlier by an
	// openai-wire row can carry EMPTY ids — llama.cpp streams calls without
	// one and the openai parser tolerates it — so empty ids are repaired
	// here: each nameless call gets a synthetic id, and the tool rows that
	// follow with an empty ToolCallID consume those ids in order (the
	// harness appends one tool row per call, in call order). open holds the
	// ids of the last assistant turn's calls that no tool row has answered
	// yet; whatever is still open when the next turn starts (or the history
	// ends) gets the synthetic error result.
	var pendingIDs, open []string
	synthetic := 0
	flushOrphans := func() {
		for _, id := range open {
			appendBlocks("user", anthropicToolResultBlock{Type: "tool_result", ToolUseID: id, Content: orphanToolResult, IsError: true})
		}
		open = open[:0]
	}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if m.Content != "" {
				sys = append(sys, m.Content)
			}
		case "user":
			flushOrphans()
			appendBlocks("user", anthropicTextBlock{Type: "text", Text: m.Content})
		case "assistant":
			flushOrphans()
			var blocks []any
			// Reasoning blocks first, verbatim — the order the API produced
			// them in (thinking precedes the tool_use it introduced).
			for _, raw := range m.ProviderBlocks {
				blocks = append(blocks, raw)
			}
			if m.Content != "" {
				blocks = append(blocks, anthropicTextBlock{Type: "text", Text: m.Content})
			}
			pendingIDs = pendingIDs[:0]
			for _, tc := range m.ToolCalls {
				id := tc.ID
				if id == "" {
					synthetic++
					id = fmt.Sprintf("ctxcall_%d", synthetic)
					pendingIDs = append(pendingIDs, id)
				}
				open = append(open, id)
				blocks = append(blocks, anthropicToolUseBlock{
					Type: "tool_use", ID: id, Name: tc.Function.Name,
					Input: anthropicToolInput(tc.Function.Arguments),
				})
			}
			if len(blocks) == 0 {
				continue
			}
			appendBlocks("assistant", blocks...)
		case "tool":
			id := m.ToolCallID
			if id == "" && len(pendingIDs) > 0 {
				id, pendingIDs = pendingIDs[0], pendingIDs[1:]
			}
			for i, o := range open {
				if o == id {
					open = append(open[:i], open[i+1:]...)
					break
				}
			}
			appendBlocks("user", anthropicToolResultBlock{Type: "tool_result", ToolUseID: id, Content: m.Content})
		}
	}
	flushOrphans()
	return strings.Join(sys, "\n\n"), out
}

// anthropicToolInput renders assembled tool-call arguments as the tool_use
// input object. ToolCall carries the object form already (normalizeArgs);
// anything that is not a valid JSON object becomes {} — the executor has
// already answered such a call with an ok:false result, and the history
// must still replay.
func anthropicToolInput(args json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(args)
	if len(t) > 0 && t[0] == '{' && json.Valid(t) {
		return t
	}
	return json.RawMessage("{}")
}

// anthropicTools converts the OpenAI function envelopes. input_schema is
// required; a tool without parameters gets the empty object schema.
func anthropicTools(tools []ToolDef) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		schema := bytes.TrimSpace(t.Function.Parameters)
		if len(schema) == 0 || bytes.Equal(schema, []byte("null")) {
			schema = []byte(`{"type":"object","properties":{}}`)
		}
		out = append(out, anthropicTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	return out
}

// chatStreamAnthropic is the streaming twin of chatAnthropic. Same retry
// doctrine as ChatStream: httpx.DoRetryOnce covers connection setup only —
// nothing is replayed once response bytes arrived.
func chatStreamAnthropic(ctx context.Context, b backends.Backend, model string,
	msgs []ChatMsg, tools []ToolDef, opts Options, timeout time.Duration,
	onEvent func(StreamEvent) error) (*StreamResult, error) {
	system, messages := anthropicMessages(msgs)
	req := anthropicRequest{
		Model:        model,
		MaxTokens:    anthropicMaxTokens(opts),
		System:       anthropicSystem(system),
		Messages:     messages,
		Stream:       true,
		OutputConfig: anthropicOutputConfig{Effort: resolveAnthropicEffort(opts, &b)},
		Tools:        anthropicTools(tools),
		// Top-level automatic caching: the API places a breakpoint on the
		// last cacheable block, so each iteration of the tool loop (and
		// each turn of the session) reads the growing history from cache
		// instead of paying full input price for it; the explicit system
		// breakpoint stays the stable-prefix anchor.
		CacheControl: &anthropicCacheControl{Type: "ephemeral"},
	}
	body, err := buildAnthropicBody(req, opts, &b)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Host+"/v1/messages", bytes.NewReader(body)) //nolint:gosec // G704: host from the F3 backend pool (context_backends) — admin-gated + locality-validated, no free user input.
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	setAnthropicHeaders(httpReq, &b)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := httpx.DoRetryOnce(httpClient, httpReq, body)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Typed StatusError for the failover classifier, as on every wire.
		return nil, anthropicStatusError(resp)
	}

	return parseAnthropicSSE(resp.Body, onEvent)
}

// --- SSE parsing ---.

// anthropicStreamFrame is the union of every data frame; only the fields
// the parser reads are modelled.
type anthropicStreamFrame struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string         `json:"id"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Data string `json:"data"` // redacted_thinking payload
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// pendingThinking accumulates one thinking block across its deltas.
type pendingThinking struct {
	text      strings.Builder
	signature string
}

// anthropicStreamState carries the assembly across frames.
type anthropicStreamState struct {
	onEvent   func(StreamEvent) error
	content   strings.Builder
	calls     []*pendingToolCall
	byIndex   map[int]*pendingToolCall
	thinking  map[int]*pendingThinking
	blocks    []json.RawMessage // reasoning blocks, stream order, replayed verbatim
	finish    string
	usage     anthropicUsage
	stopSeen  bool
	done      bool
	messageID string
}

func (st *anthropicStreamState) emit(ev StreamEvent) error {
	if st.onEvent == nil {
		return nil
	}
	if err := st.onEvent(ev); err != nil {
		return fmt.Errorf("llm: stream event callback: %w", err)
	}
	return nil
}

// parseAnthropicSSE reads frames until message_stop or EOF. Lenient ending
// mirrors parseSSEStream: EOF after message_delta (stop reason known) is a
// complete turn; EOF before it is a hard error — a mid-stream connection
// death must not look like a finished answer.
func parseAnthropicSSE(r io.Reader, onEvent func(StreamEvent) error) (*StreamResult, error) {
	st := &anthropicStreamState{onEvent: onEvent, byIndex: map[int]*pendingToolCall{}, thinking: map[int]*pendingThinking{}}
	br := bufio.NewReader(r)
	var data []string
	for {
		line, readErr := br.ReadString('\n')
		if err := st.feedLine(strings.TrimRight(line, "\r\n"), &data); err != nil {
			return nil, err
		}
		if st.done {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if err := st.dispatchData(&data); err != nil {
					return nil, err
				}
				break
			}
			return nil, fmt.Errorf("llm: read stream: %w", readErr)
		}
	}
	if !st.done && !st.stopSeen {
		return nil, fmt.Errorf("llm: stream ended unexpectedly (no message_stop, no message_delta stop_reason)")
	}
	return st.result(), nil
}

// feedLine: blank → dispatch the buffered frame; ":" → comment; "data:" →
// buffer; "event:"/"id:"/"retry:" → ignored (the type travels in the data).
func (st *anthropicStreamState) feedLine(line string, data *[]string) error {
	switch {
	case line == "":
		return st.dispatchData(data)
	case strings.HasPrefix(line, ":"):
		return nil
	case strings.HasPrefix(line, "data:"):
		v := strings.TrimPrefix(line, "data:")
		v = strings.TrimPrefix(v, " ")
		*data = append(*data, v)
		return nil
	default:
		return nil
	}
}

func (st *anthropicStreamState) dispatchData(data *[]string) error {
	if len(*data) == 0 {
		return nil
	}
	payload := strings.Join(*data, "\n")
	*data = (*data)[:0]
	return st.handleFrame(payload)
}

func (st *anthropicStreamState) handleFrame(payload string) error {
	var f anthropicStreamFrame
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return fmt.Errorf("llm: malformed stream chunk: %w", err)
	}
	switch f.Type {
	case "error":
		if f.Error == nil {
			return &StreamError{Message: "stream error event without an error object"}
		}
		return &StreamError{Code: f.Error.Type, Message: f.Error.Message}
	case "message_start":
		if f.Message != nil {
			st.messageID = f.Message.ID
			st.usage = f.Message.Usage
		}
	case "content_block_start":
		return st.onBlockStart(&f)
	case "content_block_delta":
		return st.onBlockDelta(&f)
	case "content_block_stop":
		st.onBlockStop(f.Index)
	case "message_delta":
		return st.onMessageDelta(&f)
	case "message_stop":
		st.done = true
	}
	// ping and unknown frame types: ignored.
	return nil
}

// onBlockStart opens a content block. The API puts a tool_use block's id
// and name on the start frame (never on a later delta, unlike the OpenAI
// index-keyed assembly). A nameless tool_use block — a proxy artefact, not
// an API shape — is dropped rather than persisted as an empty call the
// harness would dispatch and replay as tool_use{id:""} (a 400 on the next
// turn); its input_json_delta frames then fall on no pending call.
func (st *anthropicStreamState) onBlockStart(f *anthropicStreamFrame) error {
	if f.ContentBlock == nil {
		return nil
	}
	switch f.ContentBlock.Type {
	case "tool_use":
		if f.ContentBlock.Name == "" {
			return nil
		}
		pc := &pendingToolCall{index: f.Index, id: f.ContentBlock.ID, typ: "function", name: f.ContentBlock.Name, nameEmitted: true}
		st.byIndex[f.Index] = pc
		st.calls = append(st.calls, pc)
		return st.emit(StreamEvent{ToolCallName: pc.name})
	case "thinking":
		st.thinking[f.Index] = &pendingThinking{}
	case "redacted_thinking":
		// Arrives complete on the block start; replayed as-is.
		raw, _ := json.Marshal(anthropicRedactedThinkingBlock{Type: "redacted_thinking", Data: f.ContentBlock.Data})
		st.blocks = append(st.blocks, raw)
	}
	return nil
}

// onBlockDelta routes a delta by its own type: text to the sink, tool
// input to the pending call, reasoning to the pending thinking block
// (accumulated for the replay only — never surfaced; empty under the
// default display anyway).
func (st *anthropicStreamState) onBlockDelta(f *anthropicStreamFrame) error {
	if f.Delta == nil {
		return nil
	}
	switch f.Delta.Type {
	case "text_delta":
		if f.Delta.Text != "" {
			st.content.WriteString(f.Delta.Text)
			return st.emit(StreamEvent{ContentDelta: f.Delta.Text})
		}
	case "input_json_delta":
		if pc := st.byIndex[f.Index]; pc != nil {
			pc.args.WriteString(f.Delta.PartialJSON)
		}
	case "thinking_delta":
		if pt := st.thinking[f.Index]; pt != nil {
			pt.text.WriteString(f.Delta.Thinking)
		}
	case "signature_delta":
		if pt := st.thinking[f.Index]; pt != nil {
			pt.signature = f.Delta.Signature
		}
	}
	return nil
}

// onBlockStop seals a thinking block into its replay form.
func (st *anthropicStreamState) onBlockStop(index int) {
	pt := st.thinking[index]
	if pt == nil {
		return
	}
	raw, _ := json.Marshal(anthropicThinkingBlock{Type: "thinking", Thinking: pt.text.String(), Signature: pt.signature})
	st.blocks = append(st.blocks, raw)
	delete(st.thinking, index)
}

// onMessageDelta takes the stop reason and the output count. Newer servers
// repeat the (cumulative) input counters here; each one is taken only when
// it is itself present (> 0), so a delta that carries input_tokens alone
// cannot zero the cache counters message_start reported.
func (st *anthropicStreamState) onMessageDelta(f *anthropicStreamFrame) error {
	if f.Usage != nil {
		st.usage.OutputTokens = f.Usage.OutputTokens
		if f.Usage.InputTokens > 0 {
			st.usage.InputTokens = f.Usage.InputTokens
		}
		if f.Usage.CacheReadInputTokens > 0 {
			st.usage.CacheReadInputTokens = f.Usage.CacheReadInputTokens
		}
		if f.Usage.CacheCreationInputTokens > 0 {
			st.usage.CacheCreationInputTokens = f.Usage.CacheCreationInputTokens
		}
	}
	if f.Delta != nil && f.Delta.StopReason != "" && st.finish == "" {
		st.stopSeen = true
		st.finish = mapAnthropicStopReason(f.Delta.StopReason)
		return st.emit(StreamEvent{FinishReason: st.finish})
	}
	return nil
}

func (st *anthropicStreamState) result() *StreamResult {
	prompt, output, cacheRead, cacheCreate := anthropicUsageTotals(st.usage)
	res := &StreamResult{
		Content:             st.content.String(),
		FinishReason:        st.finish,
		PromptTokens:        prompt,
		CompletionTokens:    output,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreate,
		ProviderRequestID:   st.messageID,
		ProviderBlocks:      st.blocks,
	}
	for _, pc := range st.calls {
		args := bytes.TrimSpace(pc.args.Bytes())
		if len(args) == 0 {
			args = []byte("{}")
		}
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:       pc.id,
			Type:     "function",
			Function: ToolCallFunction{Name: pc.name, Arguments: args},
		})
	}
	return res
}
