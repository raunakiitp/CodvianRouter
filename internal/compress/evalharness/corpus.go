package evalharness

import (
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Case is one fixed corpus case: a literal prompt plus the lossy
// token budget the harness trims toward. Nothing here is fetched,
// generated, or random — the corpus is code, so a changed literal
// changes the measured numbers and the golden test catches it.
type Case struct {
	// Name identifies the case in tables and tests. Unique.
	Name string
	// Category groups the case (code, json, prose, tool-output,
	// system-heavy, mixed, history).
	Category string
	// Messages is the fixed prompt.
	Messages []provider.Message
	// LossyBudget is the token budget for the lossy engine run.
	// It is a literal per case, chosen when the corpus was
	// written: below the case's input size for the long cases (so
	// lossy actually trims), above it for the short/no-op cases
	// (so lossy honestly reports 0 saved there).
	LossyBudget int
}

func msg(role, content string) provider.Message {
	return provider.Message{Role: role, Content: content}
}

func join(lines ...string) string {
	return strings.Join(lines, "\n")
}

// Corpus returns the fixed measured-savings corpus: 13 cases
// covering code (Go + Python, with fences), JSON (nested +
// tabular), prose (short + long), tool output (JSON tool results +
// ANSI/progress text), system-prompt-heavy, and mixed/history
// prompts. Order is fixed and is the order tables print in.
func Corpus() []Case {
	return []Case{
		{
			Name:        "go-code-fenced",
			Category:    "code",
			LossyBudget: 50,
			Messages: []provider.Message{
				msg("system", "You are a Go expert. Answer with code only."),
				msg("user", join(
					"Review this handler for bugs.   ",
					"",
					"",
					"```go",
					"func handle(w http.ResponseWriter, r *http.Request) {",
					"\tname := r.URL.Query().Get(\"name\")",
					"\tif name == \"\" {",
					"\t\tname = \"world\"",
					"\t}",
					"\tfmt.Fprintf(w, \"hello %s\", name)",
					"}",
					"```",
					"",
					"",
					"What happens when name contains HTML?   ",
				)),
			},
		},
		{
			Name:        "python-code-fenced",
			Category:    "code",
			LossyBudget: 45,
			Messages: []provider.Message{
				msg("system", "You are a Python expert."),
				msg("user", join(
					"Why is this slow?  ",
					"",
					"```python",
					"def total(rows):",
					"    out = 0",
					"    for r in rows:",
					"        for c in r:",
					"            out += c",
					"    return out",
					"",
					"",
					"print(total([[1, 2], [3, 4]]))",
					"```",
					"",
					"Suggest a faster version.   ",
				)),
			},
		},
		{
			Name:        "json-nested",
			Category:    "json",
			LossyBudget: 80,
			Messages: []provider.Message{
				msg("user", join(
					"Parse this config and list every enabled feature:   ",
					"",
					"{",
					`  "service": {`,
					`    "name": "router",`,
					`    "features": {`,
					`      "cache": {"enabled": true, "ttl_seconds": 300},`,
					`      "retry": {"enabled": false, "max_attempts": 3},`,
					`      "metrics": {"enabled": true, "port": 9090}`,
					`    },`,
					`    "upstreams": [`,
					`      {"name": "a", "weight": 2},`,
					`      {"name": "b", "weight": 1}`,
					`    ]`,
					`  }`,
					`}`,
					"",
					"",
					"Answer as a bullet list.   ",
				)),
			},
		},
		{
			Name:        "json-tabular",
			Category:    "json",
			LossyBudget: 100,
			Messages: []provider.Message{
				msg("user", join(
					"Which region grew fastest? Data:  ",
					"",
					`[{"region":"north","q1":120,"q2":150,"q3":180,"q4":210},`,
					` {"region":"south","q1":90,"q2":95,"q3":110,"q4":130},`,
					` {"region":"east","q1":200,"q2":190,"q3":220,"q4":260},`,
					` {"region":"west","q1":60,"q2":80,"q3":75,"q4":95},`,
					` {"region":"central","q1":140,"q2":160,"q3":155,"q4":175}]`,
					"",
					"Give the region and the percentage growth from q1 to q4.   ",
				)),
			},
		},
		{
			Name:        "prose-short",
			Category:    "prose",
			LossyBudget: 200,
			Messages: []provider.Message{
				msg("user", "What is the capital of France? Answer in one sentence."),
			},
		},
		{
			Name:        "prose-long",
			Category:    "prose",
			LossyBudget: 120,
			Messages: []provider.Message{
				msg("user", join(
					"Write a summary of how a local-first LLM router works.   ",
					"",
					"",
					"A local-first router sits between the agent and the providers. It takes one OpenAI-compatible request, picks the best healthy target from the providers the operator connected, and forwards the request. If the target is slow, rate-limited, or down, it moves to the next target without the agent noticing.   ",
					"",
					"",
					"",
					"The router also keeps costs down. It caches repeated prompts, reuses prefix state where the provider supports it, and compresses the prompt before sending it. Quotas from every connected account behave like one pool, so a request is never wasted on an exhausted key while another key still has quota.   ",
					"",
					"Finally, the router records what happened: which provider answered, how many tokens were used, what it cost, and how long it took. That record is what makes the system debuggable instead of a black box.   ",
					"",
					"",
				)),
			},
		},
		{
			Name:        "tool-output-json",
			Category:    "tool-output",
			LossyBudget: 40,
			Messages: []provider.Message{
				msg("system", "You use tools to look up data."),
				{Role: "assistant", Content: "I will look up the order.", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "get_order", Arguments: `{"id": 42}`}}}},
				{Role: "tool", Content: `{"id": 42, "status": "pending", "total": 19.99}`, ToolCallID: "call_1"},
				{Role: "tool", Content: `{"id": 42, "status": "shipped", "total": 19.99, "tracking": "1Z999"}`, ToolCallID: "call_1"},
				msg("user", "Has order 42 shipped yet?   "),
			},
		},
		{
			Name:        "tool-output-ansi",
			Category:    "tool-output",
			LossyBudget: 100,
			Messages: []provider.Message{
				msg("system", "You summarise command output."),
				{Role: "assistant", Content: "Running the test suite now."},
				{Role: "tool", Content: join(
					"\x1b[32mPASS\x1b[0m TestRouter (0.42s)   ",
					"\x1b[32mPASS\x1b[0m TestCache (0.11s)",
					"⠋ running TestFailover...",
					"⠙ running TestFailover...",
					"⠹ running TestFailover...",
					"\x1b[31mFAIL\x1b[0m TestFailover (1.20s)",
					"    expected 0 errors, got 3",
					"Progress: [██████████] 100%   ",
					"",
					"",
					"Done: 41 passed, 1 failed.   ",
				), ToolCallID: "call_run_tests"},
				msg("user", "Summarise the test run and tell me what failed.   "),
			},
		},
		{
			Name:        "system-prompt-heavy",
			Category:    "system-heavy",
			LossyBudget: 35,
			Messages: []provider.Message{
				msg("system", "You are Codvian Router's assistant. Be concise, accurate, and never invent numbers. If you do not know, say so."),
				msg("user", "What is 2+2?"),
				msg("system", "You are Codvian Router's assistant. Be concise, accurate, and never invent numbers. If you do not know, say so."),
				msg("user", "What is 3+3?"),
				msg("system", "You are Codvian Router's assistant. Be concise, accurate, and never invent numbers. If you do not know, say so."),
				msg("user", "What is 4+4? Answer briefly."),
			},
		},
		{
			Name:        "mixed-code-tool-prose",
			Category:    "mixed",
			LossyBudget: 50,
			Messages: []provider.Message{
				msg("system", "You are a debugging assistant."),
				msg("system", "You are a debugging assistant."),
				msg("user", join(
					"The server panics on this input:   ",
					"",
					"```go",
					"m := map[string]int{}",
					"fmt.Println(m[\"missing\"])",
					"```",
					"",
					"Here is the log:   ",
				)),
				{Role: "tool", Content: join(
					"panic: runtime error: index out of range [3] with length 3   ",
					"goroutine 12 [running]:",
					"main.handle(0xc00001a000)",
					"",
					"",
				), ToolCallID: "call_logs"},
				msg("user", "Explain the panic and give a fix.   "),
			},
		},
		{
			Name:        "noop-clean-prose",
			Category:    "prose",
			LossyBudget: 200,
			Messages: []provider.Message{
				msg("user", "Explain photosynthesis in two sentences."),
			},
		},
		{
			Name:        "whitespace-heavy-prose",
			Category:    "prose",
			LossyBudget: 30,
			Messages: []provider.Message{
				msg("user", join(
					"Draft a short email to the team.   ",
					"",
					"",
					"",
					"Subject: Friday deploy   ",
					"",
					"",
					"Hi all,   ",
					"",
					"We are deploying on Friday at 10am.   ",
					"",
					"",
					"Please hold off merging after Thursday.   ",
					"",
					"",
					"",
					"Thanks,   ",
					"Raunak   ",
					"",
					"",
				)),
			},
		},
		{
			Name:        "multi-turn-history",
			Category:    "history",
			LossyBudget: 80,
			Messages: []provider.Message{
				msg("system", "You are a helpful coding assistant."),
				msg("user", "How do I read a file in Go?"),
				msg("assistant", "Use os.ReadFile, which returns the contents as a byte slice and an error."),
				msg("user", "And how do I write one?"),
				msg("assistant", "Use os.WriteFile with the filename, the bytes, and a permission like 0644."),
				msg("user", "What if the file does not exist when I read it?"),
				msg("assistant", "os.ReadFile returns an error wrapping fs.ErrNotExist; check it with errors.Is."),
				msg("user", "Show me a complete example that reads a config file, and explain each step in detail, including error handling and what happens on each failure path.   "),
			},
		},
	}
}
