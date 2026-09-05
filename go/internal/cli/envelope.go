// The CLI response contract lives here, in ONE function (T03-13).
//
// Every ctx route answers with the same frame: {"success":bool,"error":string}
// plus the payload. Before this file the CLI had three answers to the question
// "what does success:false mean for the exit code": checkSettingsEnvelope
// (stderr + exit 1), checkAPIEnvelope (same, but a body without the frame is
// not an error), and — at two dozen command paths — nothing at all, which
// printed the failure to stdout and exited 0. That last one is the trap
// api.go named: a failed `ctx stats` inside a shell pipeline or a cron job
// looked exactly like a successful one.
//
// Now there is one checker and one flag. The flag is not about strictness for
// its own sake, it is about who chose the route:
//
//   - Typed commands (`ctx stats`, `ctx settings get`, `ctx block-grant list`, …)
//     address a fixed ctx endpoint that always answers with the frame. A body
//     without it is a broken response, not a payload — envelopeRequired.
//   - The raw passthroughs (`ctx api`, `ctx manage`) let the CALLER pick the
//     route, including routes that legitimately answer without the frame (a
//     bare array, /health). There a frameless body prints and exits 0 —
//     envelopeOptional.
//
// `ctx health` is outside the contract entirely: GET /health is not an /api
// route and carries no envelope, so it keeps exit 0 and is not checked here.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// envelopeMode decides how a response body that is NOT a ctx envelope is
// treated. It does NOT change what success:false means — that is exit 1 in
// both modes, which is the whole point of the contract.
type envelopeMode int

const (
	// envelopeRequired: the body must be a ctx envelope with success:true.
	// An unparseable body or a missing success field is an error — but a
	// frameless body that still carries {"error":…} is reported by that reason,
	// which is what the auth middleware sends on a 401.
	envelopeRequired envelopeMode = iota
	// envelopeOptional: only an explicit success:false is an error. Anything
	// the frame does not cover prints as-is and exits 0.
	envelopeOptional
)

// checkEnvelope turns a server answer into the command error that cobra maps
// to exit 1 — the single place where the CLI decides that. The raw body is the
// error detail, because the server messages are already caller-ready
// ("validation: …", "… is restart-only").
//
// ORDER MATTERS at every call site: the caller prints FIRST and calls this
// second. Checking before printing yields the same exit code and silently drops
// the body of a failed call — which would break every `ctx stats | jq` that
// this contract is supposed to leave untouched. The two exceptions are
// mcpListRun and keysListRun, whose old failure output ("No MCP clients
// registered.") was a false statement rather than a machine contract; they
// check first and print nothing.
func checkEnvelope(resp []byte, mode envelopeMode) error {
	var env struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(resp, &env) != nil {
		if mode == envelopeOptional {
			return nil
		}
		return fmt.Errorf("unparseable response: %s", truncateForError(resp))
	}
	// A missing success field is the frameless case: only the raw passthroughs
	// accept it as an answer.
	if env.Success == nil && mode == envelopeOptional {
		return nil
	}
	if env.Success != nil && *env.Success {
		return nil
	}
	// Failure. The server's own reason is the message whenever it sent one —
	// including the frameless shapes that carry only {"error":…}, which the
	// auth middleware returns on a 401 and which the typed commands have always
	// reported by their reason rather than by their raw body.
	if env.Error != "" {
		return fmt.Errorf("%s", env.Error)
	}
	return fmt.Errorf("request failed: %s", truncateForError(resp))
}

// truncateForError bounds the raw body that goes into an error message: enough
// to recognize the answer, never a 10 MB payload on stderr.
func truncateForError(resp []byte) string {
	s := strings.TrimSpace(string(resp))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
