// Token-count accuracy audit: the fixed corpus and the drift
// statistics (CHECKLIST 229-230).
//
// The whole budget/ledger/pricing stack trusts the local tiktoken
// estimate (items 145-146), but its drift per provider wire was
// never measured. This file defines WHAT is measured — a fixed
// corpus of prompts in six classes, sent through each wire — and
// HOW the drift is summarised. The harness that actually sends the
// corpus through the real engine against mocks with known
// provider-reported usage lives in internal/router
// (token_audit_test.go); the mock "provider" model is documented
// there and in docs/token-count-audit.md.
//
// Definitions, so the numbers mean one thing everywhere:
//   - Local: the router's local tiktoken estimate of the prompt
//     (tokencount.CountPrompt: message text only — content, text
//     parts, tool-call arguments; roles, tool schemas and
//     per-message framing are NOT counted).
//   - Reported: the prompt-token count the upstream reports for the
//     same request (usage.prompt_tokens / input_tokens /
//     promptTokenCount / inputTokens, per wire).
//   - Error %: 100 * (Local - Reported) / Reported. Negative means
//     the local estimate UNDERCOUNTS what the provider bills — the
//     dangerous direction for a fail-closed budget, because a
//     request the gate thinks fits can still overshoot.
//   - Mean error: the signed mean (systematic bias, with its
//     direction). Mean |error| and max |error|: magnitude, worst
//     case. All three are reported per (wire, corpus class);
//     a single global number would hide a wire that is fine on
//     English and badly wrong on tool calls.
package tokencount

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// CorpusClass is one prompt class in the audit corpus. The classes
// exist because the estimator's failure modes are not uniform:
// framing overhead bites hardest on short prompts, tool schemas on
// tool-heavy ones, and BPE merge behaviour differs across scripts.
type CorpusClass string

const (
	// ClassEnglish is plain English prose, short to long.
	ClassEnglish CorpusClass = "english"
	// ClassCode is prompts carrying source code.
	ClassCode CorpusClass = "code"
	// ClassHinglish is Hindi-English code-switched prose
	// (Roman + Devanagari script).
	ClassHinglish CorpusClass = "hinglish"
	// ClassCJK is Chinese / Japanese prompts.
	ClassCJK CorpusClass = "cjk"
	// ClassLongRepetitive is long, highly repetitive text —
	// the case where BPE merges make one token cover many
	// characters, so a character-based guess would be worst.
	ClassLongRepetitive CorpusClass = "long-repetitive"
	// ClassToolCallHeavy is prompts with tool definitions and
	// tool-call turns — the case the estimator historically
	// ignored entirely (tool schemas are not message text).
	ClassToolCallHeavy CorpusClass = "tool-call-heavy"
)

// AllClasses lists every corpus class, in the order the audit
// reports them.
var AllClasses = []CorpusClass{
	ClassEnglish, ClassCode, ClassHinglish, ClassCJK,
	ClassLongRepetitive, ClassToolCallHeavy,
}

// CorpusCase is one fixed prompt in the audit corpus. Messages and
// Tools are the request exactly as the harness sends it; nothing
// about a case is generated at run time (except the two long
// repetitive cases, whose text is strings.Repeat of a fixed
// sentence — the repetition IS the case).
type CorpusCase struct {
	ID       string
	Class    CorpusClass
	Messages []provider.Message
	Tools    []provider.ToolDefinition
}

// toolDef builds one tool definition from a name, a description and
// a JSON-schema parameters object (given as a JSON literal — a
// malformed literal is a corpus bug, caught by TestCorpusValid).
func toolDef(name, description, paramsJSON string) provider.ToolDefinition {
	return provider.ToolDefinition{
		Type: "function",
		Function: struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(paramsJSON),
		},
	}
}

// Corpus returns the fixed audit corpus: 3 cases per class, 18 in
// all. The corpus is committed, not generated: the audit numbers
// in docs/token-count-audit.md are reproducible from exactly
// these prompts.
func Corpus() []CorpusCase {
	weatherTool := toolDef("get_weather",
		"Get the current weather for a city, with temperature, humidity and a short forecast.",
		`{"type":"object","properties":{"city":{"type":"string","description":"City name, e.g. Pune"},"units":{"type":"string","enum":["celsius","fahrenheit"],"description":"Temperature units"}},"required":["city"]}`)
	flightsTool := toolDef("search_flights",
		"Search for flights between two airports on a date, returning price, duration and stops.",
		`{"type":"object","properties":{"origin":{"type":"string","description":"IATA code of the departure airport"},"destination":{"type":"string","description":"IATA code of the arrival airport"},"date":{"type":"string","description":"Departure date in YYYY-MM-DD format"},"passengers":{"type":"integer","description":"Number of adult passengers","minimum":1},"cabin":{"type":"string","enum":["economy","premium_economy","business","first"]}},"required":["origin","destination","date"]}`)
	hotelsTool := toolDef("search_hotels",
		"Search hotels in a city for a date range, with price per night, rating and amenities.",
		`{"type":"object","properties":{"city":{"type":"string","description":"City to search in"},"check_in":{"type":"string","description":"Check-in date in YYYY-MM-DD format"},"check_out":{"type":"string","description":"Check-out date in YYYY-MM-DD format"},"guests":{"type":"integer","description":"Number of guests","minimum":1},"max_price_per_night":{"type":"number","description":"Maximum price per night in USD"},"amenities":{"type":"array","items":{"type":"string"},"description":"Required amenities, e.g. wifi, pool, gym"}},"required":["city","check_in","check_out"]}`)

	return []CorpusCase{
		// ---- English ----
		{ID: "eng-short", Class: ClassEnglish, Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "What is the capital of France?"},
		}},
		{ID: "eng-medium", Class: ClassEnglish, Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant. Answer concisely."},
			{Role: "user", Content: "Explain photosynthesis in about a paragraph. I know it involves sunlight, water and carbon dioxide, and that plants produce oxygen, but I never understood where exactly the energy goes or why the process needs chlorophyll at all. What would happen to a plant kept in total darkness for a week?"},
		}},
		{ID: "eng-long", Class: ClassEnglish, Messages: []provider.Message{
			{Role: "system", Content: "You are a careful editor. Rewrite the draft below for clarity, keeping its meaning and its informal tone."},
			{Role: "user", Content: "Here is the draft I need rewritten. I have been thinking about how remote work changed the way teams build trust. When everyone was in the same office, trust came from being visible: you saw people at their desks, you overheard conversations, you knew who was stuck because you could see them staring at the screen. Remote work removed all of that, and for the first year or two most teams tried to replace it with surveillance — status indicators, activity trackers, always-on video. That failed, obviously, because being watched is not the same as being trusted. The teams that actually made it work did something different: they made their work visible instead of their people. Decisions written down, progress posted in the open, blockers named early, so anyone could see the state of the work without watching anyone. Trust moved from presence to evidence. I think that is the real lesson, and it applies well beyond remote work: any time you cannot see people, show the work. The draft goes on like this for a while, but you get the idea — please rewrite it so the argument lands in half the words."},
		}},
		// ---- Code ----
		{ID: "code-python", Class: ClassCode, Messages: []provider.Message{
			{Role: "user", Content: "Explain what this Python function does, line by line, and point out any bug:\n\ndef fib(n):\n    if n <= 1:\n        return n\n    a, b = 0, 1\n    for _ in range(n):\n        a, b = b, a + b\n    return a\n\nprint(fib(10))"},
		}},
		{ID: "code-go", Class: ClassCode, Messages: []provider.Message{
			{Role: "user", Content: "Review this Go code for concurrency bugs:\n\nfunc process jobs(jobs <-chan Job, results chan<- Result, wg *sync.WaitGroup) {\n    defer wg.Done()\n    for j := range jobs {\n        r := handle(j)\n        results <- r\n    }\n}\n\nfunc main() {\n    jobs := make(chan Job, 100)\n    results := make(chan Result, 100)\n    var wg sync.WaitGroup\n    for w := 0; w < 3; w++ {\n        wg.Add(1)\n        go process(jobs, results, &wg)\n    }\n    wg.Wait()\n    close(results)\n}"},
		}},
		{ID: "code-debug", Class: ClassCode, Messages: []provider.Message{
			{Role: "user", Content: "This stack trace appears when I run my service. What is the root cause?\n\nTraceback (most recent call last):\n  File \"app.py\", line 42, in handle_request\n    user = db.get_user(user_id)\n  File \"db.py\", line 17, in get_user\n    raise ConnectionError(\"pool exhausted: 10/10 connections in use\")\nConnectionError: pool exhausted: 10/10 connections in use\n\nThe handler opens a connection per request and never closes it explicitly."},
		}},
		// ---- Hinglish ----
		{ID: "hinglish-roman", Class: ClassHinglish, Messages: []provider.Message{
			{Role: "user", Content: "Mujhe apne boss ko ek email likhna hai, kal ki chhutti ke liye. Meri tabiyat thodi kharab hai, fever hai, toh main work from home karna chahta hoon. Email polite aur short hona chahiye, aur main promise karna chahta hoon ki saare pending tasks shaam tak complete kar dunga."},
		}},
		{ID: "hinglish-devanagari", Class: ClassHinglish, Messages: []provider.Message{
			{Role: "user", Content: "मुझे एक छुट्टी का आवेदन लिखना है। मैं अगले सोमवार से बुधवार तक अपने गाँव जा रहा हूँ, क्योंकि मेरे दादा जी की तबीयत ठीक नहीं है। कृपया एक औपचारिक पत्र लिखिए जिसमें छुट्टी का कारण और तारीखें स्पष्ट हों।"},
		}},
		{ID: "hinglish-mixed", Class: ClassHinglish, Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant who understands Hinglish."},
			{Role: "user", Content: "Yaar, mera laptop bahut slow ho gaya hai, startup mein hi 5 minute lagte hain. Maine already temporary files delete kar diye, but koi farak nahi pada. Kya SSD upgrade karna better rahega ya RAM badhana? Budget around 8000 rupees hai, aur laptop 4 saal purana hai."},
		}},
		// ---- CJK ----
		{ID: "cjk-zh", Class: ClassCJK, Messages: []provider.Message{
			{Role: "user", Content: "请用中文解释什么是光合作用，并说明为什么植物需要叶绿素。如果把一株植物放在完全黑暗的环境中一周，会发生什么变化？"},
		}},
		{ID: "cjk-ja", Class: ClassCJK, Messages: []provider.Message{
			{Role: "user", Content: "リモートワークでチームの信頼を築く方法について、日本語で説明してください。オフィスにいた頃と比べて、何が変わったのでしょうか。"},
		}},
		{ID: "cjk-mixed", Class: ClassCJK, Messages: []provider.Message{
			{Role: "user", Content: "请帮我写一封英文邮件给我的 manager，主题是申请下周三休假。I need to attend a family event in Shanghai, and I will finish all my pending tasks before I leave. 语气要正式但友好。"},
		}},
		// ---- Long repetitive ----
		{ID: "long-repeat", Class: ClassLongRepetitive, Messages: []provider.Message{
			{Role: "user", Content: "Repeat this sentence back to me exactly, then stop:\n" + strings.Repeat("The quick brown fox jumps over the lazy dog. ", 100)},
		}},
		{ID: "long-lorem", Class: ClassLongRepetitive, Messages: []provider.Message{
			{Role: "user", Content: "Summarise the following text in one sentence:\n" + strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris. ", 25)},
		}},
		{ID: "long-list", Class: ClassLongRepetitive, Messages: []provider.Message{
			{Role: "user", Content: "Here is my packing list. Tell me if anything important is missing:\n" + strings.Repeat("- passport, tickets, phone charger, toothbrush, sunscreen, water bottle, notebook, pen, headphones, jacket\n", 30)},
		}},
		// ---- Tool-call heavy ----
		{ID: "tool-single", Class: ClassToolCallHeavy, Messages: []provider.Message{
			{Role: "user", Content: "What is the weather like in Pune right now?"},
		}, Tools: []provider.ToolDefinition{weatherTool}},
		{ID: "tool-multi", Class: ClassToolCallHeavy, Messages: []provider.Message{
			{Role: "system", Content: "You are a travel assistant. Use the available tools to answer."},
			{Role: "user", Content: "Plan a trip for me: fly from Mumbai to Delhi on 2026-12-20 for 2 people in economy, and find a hotel in Delhi for 3 nights under $80 a night with wifi and a gym."},
		}, Tools: []provider.ToolDefinition{flightsTool, hotelsTool}},
		{ID: "tool-conv", Class: ClassToolCallHeavy, Messages: []provider.Message{
			{Role: "user", Content: "What is the weather like in Pune right now?"},
			{Role: "assistant", Content: "", ToolCalls: []provider.ToolCall{
				{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune","units":"celsius"}`}},
			}},
			{Role: "tool", Content: `{"temperature": 28, "humidity": 62, "forecast": "partly cloudy"}`, ToolCallID: "call_1"},
			{Role: "user", Content: "Thanks — and what about Mumbai?"},
		}, Tools: []provider.ToolDefinition{weatherTool}},
	}
}

// Sample is one measured (local, reported) pair for one corpus
// case sent through one wire.
type Sample struct {
	Wire     string
	Class    CorpusClass
	CaseID   string
	Local    int // local tiktoken estimate of the prompt
	Reported int // provider-reported prompt tokens for the same request
}

// ErrPct is the sample's signed error in percent of the reported
// count: 100 * (Local - Reported) / Reported. Negative means the
// local estimate undercounts what the provider bills.
func (s Sample) ErrPct() float64 {
	if s.Reported <= 0 {
		return 0
	}
	return 100 * float64(s.Local-s.Reported) / float64(s.Reported)
}

// DriftStats summarises the drift for one (wire, class) cell: how
// many samples, the signed mean error (systematic bias, with its
// direction), the mean absolute error and the worst absolute
// error (magnitude).
type DriftStats struct {
	Wire       string
	Class      CorpusClass
	N          int
	MeanErrPct float64
	MeanAbsPct float64
	MaxAbsPct  float64
}

// ComputeDrift summarises samples for one (wire, class) cell.
// Samples for other cells are ignored; an empty cell reports N=0
// and zero errors (never NaN), so a missing cell is visible as
// "no data" instead of a misleading zero.
func ComputeDrift(wire string, class CorpusClass, samples []Sample) DriftStats {
	st := DriftStats{Wire: wire, Class: class}
	var sum, sumAbs float64
	for _, s := range samples {
		if s.Wire != wire || s.Class != class || s.Reported <= 0 {
			continue
		}
		e := s.ErrPct()
		st.N++
		sum += e
		if e < 0 {
			sumAbs -= e
		} else {
			sumAbs += e
		}
		if ae := absf(e); ae > st.MaxAbsPct {
			st.MaxAbsPct = ae
		}
	}
	if st.N > 0 {
		st.MeanErrPct = sum / float64(st.N)
		st.MeanAbsPct = sumAbs / float64(st.N)
	}
	return st
}

// Summarize computes DriftStats for every (wire, class) cell that
// has at least one sample, in wire-then-class order. Cells with no
// samples are omitted (a missing cell is a harness gap, and the
// test asserts full coverage separately).
func Summarize(samples []Sample) []DriftStats {
	seen := map[string]bool{}
	var keys []string
	for _, s := range samples {
		k := s.Wire + "\x00" + string(s.Class)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]DriftStats, 0, len(keys))
	for _, k := range keys {
		parts := strings.SplitN(k, "\x00", 2)
		out = append(out, ComputeDrift(parts[0], CorpusClass(parts[1]), samples))
	}
	return out
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// MarkdownTable renders drift stats as the Markdown table used in
// docs/token-count-audit.md — one row per (wire, class) cell.
// The harness test logs this table verbatim, so the doc's numbers
// are copied from a real run, never retyped from memory.
func MarkdownTable(stats []DriftStats) string {
	var sb strings.Builder
	sb.WriteString("| Wire | Corpus class | n | Mean error % | Mean |error| % | Max |error| % |\n")
	sb.WriteString("|---|---|---|---|---|---|\n")
	for _, st := range stats {
		fmt.Fprintf(&sb, "| %s | %s | %d | %+.2f | %.2f | %.2f |\n",
			st.Wire, st.Class, st.N, st.MeanErrPct, st.MeanAbsPct, st.MaxAbsPct)
	}
	return sb.String()
}
