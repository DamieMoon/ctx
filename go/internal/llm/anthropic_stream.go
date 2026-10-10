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
//     signature_delta: ignored (empty under the
//     default display anyway)
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
}

// anthropicMessages converts the multi-turn history: the joined system text
// and the alternating message list. Empty assistant turns without tool
// calls are dropped (the API rejects empty content), and an assistant turn
// that would end the list is kept — the harness never ends on one (it
// appends tool results), and a trailing assistant turn is a prefill the
// Claude 5 models reject with a clear 400 rather than something to mask.
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
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if m.Content != "" {
				sys = append(sys, m.Content)
			}
		case "user":
			appendBlocks("user", anthropicTextBlock{Type: "text", Text: m.Content})
		case "assistant":
			var blocks []any
			if m.Content != "" {
				blocks = append(blocks, anthropicTextBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicToolUseBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Function.Name,
					Input: anthropicToolInput(tc.Function.Arguments),
				})
			}
			if len(blocks) == 0 {
				continue
			}
			appendBlocks("assistant", blocks...)
		case "tool":
			appendBlocks("user", anthropicToolResultBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content})
		}
	}
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
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicStreamState carries the assembly across frames.
type anthropicStreamState struct {
	onEvent    func(StreamEvent) error
	content    strings.Builder
	calls      []*pendingToolCall
	byIndex    map[int]*pendingToolCall
	finish     string
	usage      anthropicUsage
	stopSeen   bool
	done       bool
	messageID  string
	outputSeen bool
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
	st := &anthropicStreamState{onEvent: onEvent, byIndex: map[int]*pendingToolCall{}}
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
		if f.ContentBlock != nil && f.ContentBlock.Type == "tool_use" {
			pc := &pendingToolCall{index: f.Index, id: f.ContentBlock.ID, typ: "function", name: f.ContentBlock.Name}
			st.byIndex[f.Index] = pc
			st.calls = append(st.calls, pc)
			if pc.name != "" {
				pc.nameEmitted = true
				return st.emit(StreamEvent{ToolCallName: pc.name})
			}
		}
	case "content_block_delta":
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
		}
		// thinking_delta / signature_delta: never surfaced.
	case "message_delta":
		if f.Usage != nil {
			st.outputSeen = true
			st.usage.OutputTokens = f.Usage.OutputTokens
			// Newer servers repeat the (cumulative) input counters here;
			// take them when present so the total is the final one.
			if f.Usage.InputTokens > 0 || f.Usage.CacheReadInputTokens > 0 || f.Usage.CacheCreationInputTokens > 0 {
				st.usage.InputTokens = f.Usage.InputTokens
				st.usage.CacheReadInputTokens = f.Usage.CacheReadInputTokens
				st.usage.CacheCreationInputTokens = f.Usage.CacheCreationInputTokens
			}
		}
		if f.Delta != nil && f.Delta.StopReason != "" && st.finish == "" {
			st.stopSeen = true
			st.finish = mapAnthropicStopReason(f.Delta.StopReason)
			return st.emit(StreamEvent{FinishReason: st.finish})
		}
	case "message_stop":
		st.done = true
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
