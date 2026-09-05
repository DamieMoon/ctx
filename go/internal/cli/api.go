// ctx api — a generic REST passthrough (workflow W5, design/03 §4.1). The
// project/types/issues surfaces are real REST mounts, NOT manage actions, so the
// old `ctx manage <action>` raw reach no longer covers them. `ctx api` restores
// script-level access to EVERY route from day one: it signs the request with the
// key from config, sends the (optional) JSON body verbatim, prints the response
// JSON, and maps a success:false envelope to exit code 1.
//
//	ctx api GET  /api/project
//	ctx api POST /api/project '{"identity":"manual:x","scope":"x"}'
//	echo '{"display_name":"X"}' | ctx api PATCH /api/project/<id>
//	ctx api DELETE /api/project/<id>
//
// runRawRequest below is also what `ctx manage` runs (E03-10 B): the short form
// for the manage actions keeps its spelling, but there is one code path, one
// wire shape and one exit contract behind both.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// apiMethods is the closed set of methods the passthrough accepts (a typo like
// "GTE" should fail loudly, not send a malformed request).
var apiMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

func apiCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "api <method> <path> [json]",
		Short: "Raw REST passthrough to any /api route (JSON body via arg or stdin)",
		Long: "Send a raw authenticated request to any API route. The method is one of\n" +
			"GET/POST/PUT/PATCH/DELETE; the path starts with '/' (e.g. /api/project). A\n" +
			"JSON body may be the third argument or piped via stdin; it is sent verbatim\n" +
			"and must be valid JSON. The response JSON is printed; a success:false\n" +
			"envelope exits 1 with the server's reason. This is the script-level reach\n" +
			"for the REST surfaces (project/types/issues) that are not manage actions.",
		Example: `  ctx api GET /api/project
  ctx api POST /api/project '{"identity":"manual:x","scope":"x"}'
  echo '{"display_name":"X"}' | ctx api PATCH /api/project/<id>`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			method := strings.ToUpper(args[0])
			if !apiMethods[method] {
				return fmt.Errorf("unknown method %q (use GET/POST/PUT/PATCH/DELETE)", args[0])
			}
			path := args[1]
			if !strings.HasPrefix(path, "/") {
				return fmt.Errorf("path %q must start with '/' (e.g. /api/project)", path)
			}

			// Body: third arg, else stdin (if piped). Empty = no body.
			raw := ""
			if len(args) == 3 {
				raw = args[2]
			} else if stdin, ok := ReadStdin(); ok {
				raw = stdin
			}
			var body any
			if strings.TrimSpace(raw) != "" {
				if !json.Valid([]byte(raw)) {
					return fmt.Errorf("request body is not valid JSON")
				}
				body = json.RawMessage(raw)
			}

			return runRawRequest(getClient, method, path, body)
		},
	}
}

// runRawRequest is the raw-passthrough half of the CLI: one signed request to a
// caller-chosen route, the response JSON on stdout, the envelope contract on the
// exit code. `ctx api` and `ctx manage` both end here, so the two cannot drift
// apart in wire shape, output or exit code (E03-10 B).
//
// The mode is envelopeOptional because the CALLER picks the route: a body
// without a success field (a bare array, /health) is a legitimate answer here,
// unlike on the typed commands.
//
// The response is printed BEFORE the envelope decides the exit code — the order
// this file has documented since day one ("prints the response JSON; a
// success:false envelope exits 1", docs/cli.md) but did not implement: it used
// to swallow the body of a failed call. Now a script gets both the answer and a
// non-zero exit, and `ctx manage` can inherit the same shape without losing the
// bytes its callers already parse.
func runRawRequest(getClient func() (*Client, error), method, path string, body any) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(method, path, body)
	if err != nil {
		return err
	}
	PrintJSON(resp)
	return checkEnvelope(resp, envelopeOptional)
}
