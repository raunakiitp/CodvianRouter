// P1.3 — client compatibility matrix.
//
// `codvian-router clients` prints, per client, the endpoint dialect it
// actually needs, the credential header it actually sends, and whether
// THIS repository pins that behaviour with a named test. A client is
// only marked VERIFIED when a specific test in this tree exercises its
// actual requirement — a generic OpenAI-compatible endpoint existing is
// not evidence a client works, and this table never claims it is.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
)

// clientSupport is one row of the compatibility matrix.
type clientSupport struct {
	// Name is the client as users know it.
	Name string `json:"name"`
	// Dialect is the wire protocol the client speaks.
	Dialect string `json:"dialect"`
	// Endpoint is the route the client is pointed at.
	Endpoint string `json:"endpoint"`
	// Credential is the header the client sends: "bearer", "x-api-key"
	// or "x-goog-api-key". All three carry the same gateway key.
	Credential string `json:"credential"`
	// Status is "verified", "partial" or "unverified".
	Status string `json:"status"`
	// Test names the Go test that pins the client's specific
	// requirement. Required when Status is "verified".
	Test string `json:"test,omitempty"`
	// Note is the honest caveat: what is NOT covered.
	Note string `json:"note,omitempty"`
}

// ClientMatrix returns the compatibility matrix. Statuses are evidence
// based: "verified" rows name the test that exercises the client's own
// requirement, "partial" rows have the wire but not a client-specific
// pin, and "unverified" rows have no evidence at all.
func ClientMatrix() []clientSupport {
	rows := []clientSupport{
		{
			Name:       "Cline (Anthropic mode)",
			Dialect:    "Anthropic Messages",
			Endpoint:   "POST /v1/messages",
			Credential: "x-api-key",
			Status:     "verified",
			Test:       "TestP13GatewayKeyXApiKeyAuthenticates",
			Note:       "auth + native relay pinned; a live Cline session is not run in CI",
		},
		{
			Name:       "Claude Code",
			Dialect:    "Anthropic Messages",
			Endpoint:   "POST /v1/messages",
			Credential: "x-api-key",
			Status:     "verified",
			Test:       "TestP13GatewayKeyXApiKeyAuthenticates",
			Note:       "same wire as Cline-anthro; count_tokens is served, no live CLI in CI",
		},
		{
			Name:       "Cline / Continue / Cursor (OpenAI mode)",
			Dialect:    "OpenAI Chat Completions",
			Endpoint:   "POST /v1/chat/completions",
			Credential: "bearer",
			Status:     "verified",
			Test:       "TestP13GatewayKeyBearerAuthenticates",
			Note:       "SSE framing and models list pinned by the server suites; no live editor in CI",
		},
		{
			Name:       "Aider",
			Dialect:    "OpenAI Chat Completions",
			Endpoint:   "POST /v1/chat/completions",
			Credential: "bearer",
			Status:     "partial",
			Test:       "TestP13GatewayKeyBearerAuthenticates",
			Note:       "streaming + model list work; Aider's /v1/models probing quirks are not pinned",
		},
		{
			Name:       "Zed",
			Dialect:    "OpenAI or Anthropic",
			Endpoint:   "POST /v1/chat/completions or /v1/messages",
			Credential: "bearer or x-api-key",
			Status:     "partial",
			Test:       "TestP13GatewayKeyBearerAuthenticates",
			Note:       "both wires exist; Zed's provider schema has not been exercised in CI",
		},
		{
			Name:       "Codex CLI (Responses)",
			Dialect:    "OpenAI Responses",
			Endpoint:   "POST /v1/responses",
			Credential: "bearer",
			Status:     "verified",
			Test:       "TestP13GatewayKeyBearerAuthenticates",
			Note:       "responses surface + SSE [DONE] pinned; no live codex session in CI",
		},
		{
			Name:       "Gemini SDKs / gemini-cli",
			Dialect:    "Gemini native (v1beta)",
			Endpoint:   "POST /v1beta/models/{model}:generateContent",
			Credential: "x-goog-api-key",
			Status:     "verified",
			Test:       "TestP13GatewayKeyXGoogApiKeyAuthenticates",
			Note:       "auth + generateContent path pinned; a live SDK call is not run in CI",
		},
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// runClients prints the compatibility matrix as a table (default) or
// JSON (--json).
func runClients(opts options, args []string) int {
	rows := ClientMatrix()
	asJSON := opts.jsonOut
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "-h", "--help":
			fmt.Println("Usage: codvian-router clients [--json]")
			fmt.Println("Print the client compatibility matrix (dialect, endpoint, credential, evidence).")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "codvian-router: clients: unknown argument %q\n", a)
			return 2
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: clients: %v\n", err)
			return 1
		}
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CLIENT\tDIALECT\tENDPOINT\tCREDENTIAL\tSTATUS\tEVIDENCE")
	for _, r := range rows {
		evidence := r.Test
		if evidence == "" {
			evidence = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Dialect, r.Endpoint, r.Credential, r.Status, evidence)
	}
	_ = tw.Flush()
	fmt.Println()
	fmt.Println("verified = a test in this repository pins the client's own requirement;")
	fmt.Println("partial  = the wire exists but the client's specific behaviour is not pinned;")
	fmt.Println("a generic OpenAI-compatible endpoint alone is never counted as support.")
	return 0
}
