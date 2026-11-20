// Caveman-lite rule-based prose condensation (M7.5).
//
// English-only, extractive + rule-based ONLY: no model calls, no LLM,
// no new words. Every rule either deletes a filler phrase outright or
// replaces a verbose phrase with words already present in the matched
// phrase (a subset replacement, e.g. "in order to" -> "to",
// "each and every" -> "each"). The condensed text is therefore always
// a subsequence-ish reduction of the original: no word in the output
// that was not in the input, no number/date/identifier altered or
// dropped (enforced by the word/number self-checks below and by the
// M7.2 fidelity gate, which reverts any message the gate would refuse).
//
// What was read in OmniRoute (study only — no code copied; all logic
// below is an independent Go implementation of the observed behaviour):
//
//   - open-sse/services/compression/caveman.ts — cavemanCompress:
//     levels lite/full/ultra (INTENSITY_RANK), user-role default
//     (DEFAULT_CAVEMAN_CONFIG compressRoles ["user"]), minMessageLength
//     50, latest-user-turn protection, preserved-block extraction +
//     restore, validation fallback to the original text, sentence
//     recapitalisation (NOT reproduced here: it can corrupt identifiers
//     such as "iPhone" at a sentence start).
//   - open-sse/services/compression/cavemanRules.ts — 34 built-in
//     rule entries (CAVEMAN_RULES) in 5 categories (filler, context,
//     structural, dedup, ultra); getRulesForContext filters by context
//     (all/user/assistant) and minIntensity.
//   - open-sse/services/compression/rules/en/{filler,structural,
//     context,dedup,ultra}.json — 49 English file-pack rule nodes
//     (filler 14, structural 11, context 8, dedup 4, ultra 12) which
//     take precedence over the built-in list when present; 11 locale
//     dirs exist (de, en, es, fr, hu, id, it, ja, pt-BR, ru, zh).
//   - open-sse/services/compression/types.ts — DEFAULT_CAVEMAN_CONFIG
//     (enabled false, intensity "lite", preservePatterns for fences,
//     inline code, URLs, file paths, error/stack lines).
//   - open-sse/services/compression/preservation.ts —
//     extractPreservedBlocks / restorePreservedBlocks via sentinels.
//
// Deliberate divergences from OmniRoute (honest, not accidental):
//
//   - ENGLISH ONLY, levels lite/full only. No locales, no ultra:
//     ultra is abbreviation substitution ("database" -> "DB"), which
//     introduces new words and breaks the no-new-words guarantee.
//   - No replacement that introduces a word: OmniRoute's
//     "due to the fact that" -> "because", "for every" -> "per",
//     "furthermore" -> "also", "Can you ..." -> "Tell me" style
//     mappings are NOT reproduced where the replacement word is absent
//     from the matched phrase. Affected rules are either dropped
//     (redundant_because) or reduced to their subset-safe variants
//     (purpose_statement keeps only "for the purpose of" -> "for";
//     verbose_connectors deletes instead of substituting "also";
//     context_setup / intent_clarification / repeated_context /
//     repeated_question / reestablished_context delete instead of
//     substituting "Code:" / "Goal:" / "See above." / "[same question]"
//     / "Re:").
//   - Assistant-context rules (assistant_fillers, summary_replacement)
//     are not implemented: this engine condenses user-role messages
//     by default, matching OmniRoute's default compressRoles.
//   - No recapitalisation, no latest-user-turn special case: the
//     coordinator decides which messages are fed in.
//   - Part-carrying messages (Parts set), messages with tool calls,
//     and tool/system/developer messages are never touched: CheckLossy
//     requires part-carrying messages to match byte-identically, so
//     rewriting one would always be refused by the gate.
//
// Wiring (coordinator's job, NOT done here): call CondenseMessages
// (or CondenseText for a single string) and feed its output through
// Apply's lossy path so the M7.2 fidelity gate and M7.3 inflation
// guard still apply. This file never touches compress.go, PARITY.md,
// CHECKLIST.md or docs.
package compress

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// MinMessageLength is the minimum message length (runes) for
// condensation, matching OmniRoute's DEFAULT_CAVEMAN_CONFIG
// minMessageLength of 50. Shorter messages pass through unchanged.
const MinMessageLength = 50

// Level selects how aggressive condensation is. Lite is
// conservative (filler/hedge/pleasantry deletion only); full adds
// the aggressive structural rules (articles, leader phrases,
// passive-voice shortening, quantifier collapse). Any value other
// than LevelFull behaves as lite.
type Level string

const (
	// LevelLite is the conservative rule set (OmniRoute intensity "lite").
	LevelLite Level = "lite"
	// LevelFull is the aggressive rule set (OmniRoute intensity "full").
	LevelFull Level = "full"
)

// Options tunes CondenseMessagesWithOptions. An empty Roles list
// means the default: user-role messages only.
type Options struct {
	// Roles lists the message roles to condense. Nil/empty means ["user"].
	Roles []string
}

// CondenseResult is the outcome of CondenseMessages: the (possibly
// condensed) messages, the union of rule names that fired (in
// rule-application order), and how many messages actually changed.
type CondenseResult struct {
	Messages     []provider.Message
	AppliedRules []string
	Changed      int
}

// Rule is one condensation rule: a name, the minimum level it runs
// at, and a pure function on rune slices that returns the rewritten
// text plus whether it changed anything. Rules only ever delete or
// shorten; none introduces a word.
type Rule struct {
	Name  string
	Level Level
	apply func([]rune) ([]rune, bool)
}

// RulesForLevel returns the rules that run at level, in application
// order (lite rules first, then full rules, then sentence dedupe,
// then whitespace normalisation).
func RulesForLevel(level Level) []Rule { return rulesForLevel(level) }

func rulesForLevel(level Level) []Rule {
	var out []Rule
	for _, r := range cavemanRules {
		if r.Level == LevelLite || level == LevelFull {
			out = append(out, r)
		}
	}
	return out
}

// CondenseText condenses one prose string at level. Strings shorter
// than MinMessageLength (runes) are returned unchanged. The second
// return value names the rules that fired.
func CondenseText(text string, level Level) (string, []string) {
	if utf8.RuneCountInString(text) < MinMessageLength {
		return text, nil
	}
	return condenseTextWithRules(text, rulesForLevel(level))
}

// CondenseMessages condenses the user-role messages of msgs at
// level. System/developer/assistant/tool messages, part-carrying
// messages, and messages with tool calls are returned unchanged, as
// are messages shorter than MinMessageLength. The input slice is
// never mutated. A message whose condensed form would fail the
// M7.2 fidelity gate (or the word/number self-checks, or the
// empty/too-short guard) is returned unchanged.
func CondenseMessages(msgs []provider.Message, level Level) CondenseResult {
	return CondenseMessagesWithOptions(msgs, level, Options{})
}

// CondenseMessagesWithOptions is CondenseMessages with an explicit
// role list (Options.Roles; nil/empty means ["user"]).
func CondenseMessagesWithOptions(msgs []provider.Message, level Level, opts Options) CondenseResult {
	roles := map[string]bool{"user": true}
	if len(opts.Roles) > 0 {
		roles = map[string]bool{}
		for _, r := range opts.Roles {
			roles[r] = true
		}
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	var applied []string
	seen := map[string]bool{}
	changed := 0
	for i, m := range msgs {
		nm, rules := condenseMessage(m, level, roles)
		out[i] = nm
		if nm.Content != m.Content {
			changed++
			for _, n := range rules {
				if !seen[n] {
					seen[n] = true
					applied = append(applied, n)
				}
			}
		}
	}
	return CondenseResult{Messages: out, AppliedRules: applied, Changed: changed}
}

// condenseMessage condenses one message (or returns it unchanged
// when it is out of scope or any safety check fails).
func condenseMessage(m provider.Message, level Level, roles map[string]bool) (provider.Message, []string) {
	if !roles[m.Role] {
		return m, nil
	}
	// Part-carrying messages must match byte-identically under
	// CheckLossy, and tool calls must never be rewritten.
	if len(m.Parts) > 0 || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
		return m, nil
	}
	out, applied := CondenseText(m.Content, level)
	if out == m.Content {
		return m, nil
	}
	// Never ship an emptied or near-emptied message: a message
	// that was nothing but filler reverts to the original.
	if strings.TrimSpace(out) == "" || utf8.RuneCountInString(out) < 10 {
		return m, nil
	}
	nm := m
	nm.Content = out
	// M7.2 fidelity gate, applied per message: any case the gate
	// would refuse is left uncondensed by the engine itself.
	if ok, _ := CheckLossy([]provider.Message{m}, []provider.Message{nm}); !ok {
		return m, nil
	}
	return nm, applied
}

// condenseTextWithRules runs the full pipeline (protected-segment
// extraction, rules, sentence dedupe, cleanup, restore) to a fixed
// point: every rule only deletes or shortens, so the text shrinks
// monotonically and the loop terminates; the fixed point is what
// makes condense idempotent. The word/number self-checks run on the
// final text; on failure the original is returned unchanged.
func condenseTextWithRules(text string, rules []Rule) (string, []string) {
	cur := text
	var applied []string
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		next, iterApplied := condenseOnce(cur, rules)
		for _, n := range iterApplied {
			if !seen[n] {
				seen[n] = true
				applied = append(applied, n)
			}
		}
		if next == cur {
			break
		}
		cur = next
	}
	if !wordsSubset(text, cur) || !numbersEqual(text, cur) {
		return text, nil
	}
	return cur, applied
}

// condenseOnce is one pipeline pass over text with rules. Protected
// segments (code fences, JSON, inline code, URLs) are extracted to
// placeholders first so no rule can touch them, and restored after;
// if any protected segment does not survive the pass byte-identical,
// the original text is returned unchanged.
func condenseOnce(text string, rules []Rule) (string, []string) {
	extracted, blocks := extractProtected(text)
	cur := []rune(extracted)
	var applied []string
	for _, ru := range rules {
		next, changed := ru.apply(cur)
		if changed {
			applied = append(applied, ru.Name)
			cur = next
		}
	}
	out := restoreProtected(string(cur), blocks)
	for _, b := range blocks {
		if !strings.Contains(out, b.content) {
			return text, nil
		}
	}
	return out, applied
}

// ---------------------------------------------------------------------------
// Phrase matching (rune-based, so offsets stay valid for non-ASCII text)
// ---------------------------------------------------------------------------

// phraseAlt is one alternative of a phrase rule: the words to match
// (lowercase) and the replacement ("" = delete). The replacement
// must consist only of words already present in the matched phrase.
type phraseAlt struct {
	words          []string
	replacement    string
	noBoundaryLeft bool // phrase starts with punctuation (", and also")
}

func phraseAltOf(phrase, replacement string) phraseAlt {
	return phraseAlt{words: strings.Fields(phrase), replacement: replacement}
}

// sortAlts orders alternatives longest-first so a longer phrase
// wins over a shorter one starting at the same place
// ("thank you so much" over "thank you").
func sortAlts(alts []phraseAlt) []phraseAlt {
	sorted := append([]phraseAlt(nil), alts...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if len(sorted[i].words) != len(sorted[j].words) {
			return len(sorted[i].words) > len(sorted[j].words)
		}
		return len(strings.Join(sorted[i].words, " ")) > len(strings.Join(sorted[j].words, " "))
	})
	return sorted
}

func lowerRunes(r []rune) []rune {
	out := make([]rune, len(r))
	for i, c := range r {
		out[i] = unicode.ToLower(c)
	}
	return out
}

func isWordRune(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$'
}

func isSpaceRune(c rune) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// boundaryBefore reports whether a phrase may start at i: the
// preceding rune must not be a word rune, and a preceding '.' must
// not itself follow a word rune (so "foo.actually" never matches
// "actually", while ". Actually" does).
func boundaryBefore(r []rune, i int) bool {
	if i <= 0 {
		return true
	}
	if isWordRune(r[i-1]) {
		return false
	}
	if r[i-1] == '.' && i >= 2 && isWordRune(r[i-2]) {
		return false
	}
	return true
}

// boundaryAfter is the mirror of boundaryBefore at the end of a match.
func boundaryAfter(r []rune, i int) bool {
	if i >= len(r) {
		return true
	}
	if isWordRune(r[i]) {
		return false
	}
	if r[i] == '.' && i+1 < len(r) && isWordRune(r[i+1]) {
		return false
	}
	return true
}

// lineStart reports whether i is at the start of a line up to
// leading spaces/tabs (for anchored, line-start-only rules).
func lineStart(r []rune, i int) bool {
	j := i
	for j > 0 && (r[j-1] == ' ' || r[j-1] == '\t') {
		j--
	}
	return j == 0 || r[j-1] == '\n'
}

// nextWordLower reports whether the next non-space rune after end
// is a lowercase letter (used by the articles/emphasis/leader rules
// so they only fire before a lowercase word).
func nextWordLower(r []rune, end int) bool {
	k := end
	for k < len(r) && isSpaceRune(r[k]) {
		k++
	}
	return k < len(r) && r[k] >= 'a' && r[k] <= 'z'
}

// sentenceInitial reports whether start begins a sentence (start of
// text, or after a sentence terminator or newline, ignoring spaces).
func sentenceInitial(r []rune, start int) bool {
	j := start
	for j > 0 && (r[j-1] == ' ' || r[j-1] == '\t') {
		j--
	}
	if j == 0 {
		return true
	}
	c := r[j-1]
	return c == '.' || c == '!' || c == '?' || c == '\n'
}

// findPhrase finds the first occurrence of alt at or after from in
// the lowercased rune slice, honouring word boundaries and, for
// anchored rules, the line-start requirement. It returns rune
// offsets into the original text (lower is aligned rune-for-rune).
func findPhrase(lower []rune, alt phraseAlt, from int, anchored bool) (int, int, bool) {
	first := []rune(alt.words[0])
	for {
		idx := indexRunes(lower[from:], first)
		if idx < 0 {
			return 0, 0, false
		}
		start := from + idx
		if !alt.noBoundaryLeft && !boundaryBefore(lower, start) {
			from = start + 1
			continue
		}
		if anchored && !lineStart(lower, start) {
			from = start + 1
			continue
		}
		end := start + len(first)
		ok := true
		for _, w := range alt.words[1:] {
			j := end
			for j < len(lower) && isSpaceRune(lower[j]) {
				j++
			}
			if j == end || !hasPrefixRunes(lower[j:], []rune(w)) {
				ok = false
				break
			}
			end = j + len([]rune(w))
		}
		if !ok {
			from = start + 1
			continue
		}
		if !boundaryAfter(lower, end) {
			from = start + 1
			continue
		}
		return start, end, true
	}
}

func indexRunes(hay, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
outer:
	for i := 0; i+len(needle) <= len(hay); i++ {
		for j := range needle {
			if hay[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

func hasPrefixRunes(hay, prefix []rune) bool {
	if len(prefix) > len(hay) {
		return false
	}
	for i := range prefix {
		if hay[i] != prefix[i] {
			return false
		}
	}
	return true
}

// findFirstPhrase returns the earliest match of any alternative
// (ties go to the earliest-listed, i.e. longest, alternative).
// validate, when non-nil, must also accept the match or the search
// resumes just past it.
func findFirstPhrase(r, lower []rune, alts []phraseAlt, anchored bool, validate func([]rune, int, int) bool) (int, int, phraseAlt, bool) {
	bestStart, bestEnd := -1, -1
	var best phraseAlt
	for _, alt := range alts {
		from := 0
		for {
			s, e, ok := findPhrase(lower, alt, from, anchored)
			if !ok {
				break
			}
			if validate != nil && !validate(r, s, e) {
				from = s + 1
				continue
			}
			if bestStart < 0 || s < bestStart {
				bestStart, bestEnd, best = s, e, alt
			}
			break
		}
	}
	if bestStart < 0 {
		return 0, 0, phraseAlt{}, false
	}
	return bestStart, bestEnd, best, true
}

// deletionSpan extends a deletion [start,end) over one adjacent
// whitespace/punctuation run so no double space or dangling
// punctuation is left behind: prefer the trailing side (one
// trailing punctuation char from ",.!?:" plus following spaces),
// else the leading spaces.
func deletionSpan(r []rune, start, end int) (int, int) {
	e := end
	if e < len(r) && (r[e] == ',' || r[e] == '.' || r[e] == '!' || r[e] == '?' || r[e] == ':') {
		e++
	}
	if e < len(r) && (r[e] == ' ' || r[e] == '\t') {
		for e < len(r) && (r[e] == ' ' || r[e] == '\t') {
			e++
		}
		return start, e
	}
	s := start
	for s > 0 && (r[s-1] == ' ' || r[s-1] == '\t') {
		s--
	}
	return s, end
}

func spliceRunes(r []rune, s, e int, rep []rune) []rune {
	out := make([]rune, 0, len(r)-(e-s)+len(rep))
	out = append(out, r[:s]...)
	out = append(out, rep...)
	out = append(out, r[e:]...)
	return out
}

func capitaliseFirst(r []rune) []rune {
	if len(r) == 0 {
		return r
	}
	out := append([]rune(nil), r...)
	out[0] = unicode.ToUpper(out[0])
	return out
}

// applyPhraseRule repeatedly applies the alternatives of one rule
// until none matches. Replacement text keeps the capitalisation of
// the matched phrase's first letter.
func applyPhraseRule(r []rune, alts []phraseAlt, anchored bool, validate func([]rune, int, int) bool) ([]rune, bool) {
	changed := false
	for {
		lower := lowerRunes(r)
		s, e, alt, ok := findFirstPhrase(r, lower, alts, anchored, validate)
		if !ok {
			break
		}
		changed = true
		if alt.replacement == "" {
			ds, de := deletionSpan(r, s, e)
			r = spliceRunes(r, ds, de, nil)
		} else {
			rep := []rune(alt.replacement)
			if s < len(r) && unicode.IsUpper(r[s]) {
				rep = capitaliseFirst(rep)
			}
			r = spliceRunes(r, s, e, rep)
		}
	}
	return r, changed
}

// deleteRule builds a deletion rule from a list of phrases.
func deleteRule(name string, level Level, phrases []string, anchored bool) Rule {
	alts := make([]phraseAlt, 0, len(phrases))
	for _, p := range phrases {
		alts = append(alts, phraseAltOf(p, ""))
	}
	alts = sortAlts(alts)
	return Rule{Name: name, Level: level, apply: func(r []rune) ([]rune, bool) {
		return applyPhraseRule(r, alts, anchored, nil)
	}}
}

// replaceRule builds a subset-replacement rule from phrase ->
// replacement pairs (replacement words must already occur in the
// phrase).
func replaceRule(name string, level Level, pairs [][2]string, anchored bool) Rule {
	alts := make([]phraseAlt, 0, len(pairs))
	for _, p := range pairs {
		alts = append(alts, phraseAltOf(p[0], p[1]))
	}
	alts = sortAlts(alts)
	return Rule{Name: name, Level: level, apply: func(r []rune) ([]rune, bool) {
		return applyPhraseRule(r, alts, anchored, nil)
	}}
}

// deleteWordBeforeLowerRule builds a rule that deletes a single
// word only when the next word starts with a lowercase letter
// (articles, emphasis words, leader phrases). anchored restricts
// matches to line starts; skipSentenceInitial keeps sentence-initial
// words (articles); exceptions are next words that must never
// trigger the rule ("a priori").
func deleteWordBeforeLowerRule(name string, level Level, words []string, anchored, skipSentenceInitial bool, exceptions map[string]bool) Rule {
	alts := make([]phraseAlt, 0, len(words))
	for _, w := range words {
		alts = append(alts, phraseAltOf(w, ""))
	}
	alts = sortAlts(alts)
	validate := func(r []rune, s, e int) bool {
		if !nextWordLower(r, e) {
			return false
		}
		if skipSentenceInitial && sentenceInitial(r, s) {
			return false
		}
		if len(exceptions) > 0 {
			k := e
			for k < len(r) && isSpaceRune(r[k]) {
				k++
			}
			j := k
			for j < len(r) && r[j] >= 'a' && r[j] <= 'z' {
				j++
			}
			if exceptions[string(r[k:j])] {
				return false
			}
		}
		return true
	}
	return Rule{Name: name, Level: level, apply: func(r []rune) ([]rune, bool) {
		return applyPhraseRule(r, alts, anchored, validate)
	}}
}

// ---------------------------------------------------------------------------
// The rule inventory (38 rules; see the file header for the OmniRoute
// mapping and the deliberate omissions). Lite rules run at both
// levels; full rules run only at LevelFull.
// ---------------------------------------------------------------------------

var cavemanRules = []Rule{
	// -- Lite: filler removal --
	deleteRule("excessive_gratitude", LevelLite, []string{
		"thank you so much", "thanks in advance", "i really appreciate it", "i really appreciate",
	}, false),
	deleteRule("pleasantries", LevelLite, []string{
		"i'd be happy to", "i would be happy to", "i'd be glad to", "i would be glad to",
		"glad to help", "happy to help", "thank you", "thanks", "no problem",
		"you're welcome", "absolutely", "certainly", "of course",
	}, false),
	deleteRule("polite_framing", LevelLite, []string{
		"could you please", "would you please", "can you please",
		"i would like you to", "i want you to", "i need you to",
		"please", "kindly",
	}, false),
	deleteRule("hedging", LevelLite, []string{
		"it seems like", "it appears that", "i think that", "i believe that",
		"probably", "possibly",
	}, false),
	deleteRule("filler_adverbs", LevelLite, []string{
		"basically", "essentially", "actually", "literally", "simply", "currently",
	}, false),
	deleteRule("filler_phrases", LevelLite, []string{
		"i want to", "i need to", "i'd like to", "i'm looking for",
	}, true),
	deleteRule("redundant_openers", LevelLite, []string{
		"hi there", "hello", "good morning", "hey",
	}, true),
	deleteRule("verbose_requests", LevelLite, []string{
		"i was just wondering if you could", "i was wondering if you could",
		"just wondering if you could", "would it be possible to",
	}, false),
	deleteRule("self_reference", LevelLite, []string{
		"i am trying to", "i am working on", "i have been",
	}, true),
	deleteRule("softeners", LevelLite, []string{
		"if possible", "when you get a chance", "at your convenience", "just wondering",
	}, false),
	deleteRule("uncertainty_fillers", LevelLite, []string{
		"i guess", "i suppose", "more or less", "in a way",
	}, false),
	deleteRule("qualifier_removal", LevelLite, []string{
		"a bit of", "a little of", "a bit", "a little", "somewhat", "kind of", "sort of",
	}, false),
	replaceRule("verbose_instructions", LevelLite, [][2]string{
		{"provide a detailed explanation of", "provide"},
		{"give me a comprehensive explanation of", "give"},
		{"write an in-depth explanation of", "write"},
		{"create a thorough explanation of", "create"},
		{"provide a detailed", "provide"},
		{"give me a comprehensive", "give"},
		{"write an in-depth", "write"},
		{"create a thorough", "create"},
		{"explain in detail", "explain"},
	}, false),
	replaceRule("question_to_directive", LevelLite, [][2]string{
		{"can you explain why", "explain why"},
		{"could you show me how", "show how"},
		{"would you tell me", "tell me"},
		{"can you tell me", "tell me"},
	}, false),
	// -- Lite: context condensation --
	deleteRule("context_setup", LevelLite, []string{
		"i have the following code", "here is my code", "below is the code",
	}, false),
	deleteRule("intent_clarification", LevelLite, []string{
		"what i'm trying to do is", "my objective is to", "what i need is", "i'm aiming to",
	}, false),
	deleteRule("background_removal", LevelLite, []string{
		"as you may know", "as we discussed earlier",
	}, false),
	deleteRule("repeated_context", LevelLite, []string{
		"as mentioned before", "as previously stated", "as i said before",
	}, false),
	deleteRule("repeated_question", LevelLite, []string{
		"same question as before", "i asked this earlier", "this is the same question",
	}, false),
	deleteRule("reestablished_context", LevelLite, []string{
		"going back to the code above", "referring back to", "returning to",
	}, false),
	deleteRule("meta_commentary", LevelLite, []string{
		"note that", "keep in mind that", "remember that",
	}, true),
	replaceRule("purpose_statement", LevelLite, [][2]string{
		{"for the purpose of", "for"},
	}, false),
	replaceRule("purpose_phrases", LevelLite, [][2]string{
		{"in order to", "to"},
		{"so as to", "to"},
	}, false),
	deleteRule("verbose_connectors", LevelLite, []string{
		"furthermore", "additionally", "moreover", "in addition",
	}, false),
	deleteRule("transition_removal", LevelLite, []string{
		"on the other hand", "in contrast", "however",
	}, true),
	deleteWordBeforeLowerRule("emphasis_removal", LevelLite,
		[]string{"very", "really", "extremely", "highly", "quite"}, false, false, nil),
	replaceRule("explanatory_prefix", LevelLite, [][2]string{
		{"the function appears to be handling", "function:"},
		{"the code seems to", "code:"},
		{"the class is", "class:"},
		{"this module is", "module:"},
	}, false),
	// -- Full: structural compression --
	deleteRule("redundant_phrasing", LevelFull, []string{
		"make sure to", "be sure to",
	}, false),
	deleteRule("redundant_directive", LevelFull, []string{
		"it is important to", "you should", "remember to",
	}, false),
	// ", and also" / ", as well as" -> "," (subset: punctuation only).
	Rule{Name: "list_conjunction", Level: LevelFull, apply: func(r []rune) ([]rune, bool) {
		alts := []phraseAlt{
			{words: []string{",", "and", "also"}, replacement: ",", noBoundaryLeft: true},
			{words: []string{",", "as", "well", "as"}, replacement: ",", noBoundaryLeft: true},
		}
		return applyPhraseRule(r, alts, false, nil)
	}},
	replaceRule("redundant_quantifiers", LevelFull, [][2]string{
		{"each and every single", "each"},
		{"each and every", "each"},
	}, false),
	replaceRule("all_quantifier", LevelFull, [][2]string{
		{"any and all", "all"},
	}, false),
	replaceRule("passive_voice", LevelFull, [][2]string{
		{"is being used", "used"},
		{"is being called", "called"},
		{"is being generated", "generated"},
		{"was created", "created"},
		{"was generated", "generated"},
		{"was implemented", "implemented"},
	}, false),
	replaceRule("compound_collapse", LevelFull, [][2]string{
		{"and any potential", "and"},
	}, false),
	deleteWordBeforeLowerRule("articles", LevelFull,
		[]string{"a", "an", "the"}, false, true,
		map[string]bool{"priori": true, "posteriori": true}),
	deleteWordBeforeLowerRule("leader_phrases", LevelFull,
		[]string{"i'll", "i will", "i can", "i'd", "let me", "you can", "we will", "we can", "let's"},
		true, false, nil),
	// -- Structural (both levels): dedupe, then whitespace --
	{Name: "repeated_sentence_dedupe", Level: LevelLite, apply: func(r []rune) ([]rune, bool) {
		out, changed := dedupeSentences(string(r))
		return []rune(out), changed
	}},
	{Name: "whitespace_normalisation", Level: LevelLite, apply: func(r []rune) ([]rune, bool) {
		out := cleanupText(string(r))
		return []rune(out), out != string(r)
	}},
}

// ruleByName finds a rule by name (used by tests for toggle evidence).
func ruleByName(name string) (Rule, bool) {
	for _, r := range cavemanRules {
		if r.Name == name {
			return r, true
		}
	}
	return Rule{}, false
}

// rulesForLevelExcept returns the rules for level with one rule
// removed (used by tests: each rule test must fail with the rule
// disabled and pass with it).
func rulesForLevelExcept(level Level, except string) []Rule {
	var out []Rule
	for _, r := range rulesForLevel(level) {
		if r.Name != except {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Repeated-sentence dedupe
// ---------------------------------------------------------------------------

// dedupeSentences drops exact-duplicate sentences (case-insensitive,
// whitespace-normalised, ≥3 words) after their first occurrence.
// Sentences keep their trailing whitespace, so removal never joins
// two sentences without a separator.
// Sentences containing a digit run are never dropped: dropping one
// copy of a repeated numbered sentence would change the digit-run
// multiset and trip the numbersEqual self-check, reverting the whole
// message (safe, but zero savings for the entire message). Skipping
// them keeps the other rules' savings.
func dedupeSentences(s string) (string, bool) {
	r := []rune(s)
	var sentences []string
	start := 0
	for i := 0; i < len(r); i++ {
		if r[i] != '.' && r[i] != '!' && r[i] != '?' {
			continue
		}
		if i+1 < len(r) && !isSpaceRune(r[i+1]) {
			continue
		}
		j := i + 1
		for j < len(r) && isSpaceRune(r[j]) {
			j++
		}
		sentences = append(sentences, string(r[start:j]))
		start = j
		i = j - 1
	}
	if start < len(r) {
		sentences = append(sentences, string(r[start:]))
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(sentences))
	changed := false
	for _, sn := range sentences {
		// Never drop a sentence containing a digit run (see the
		// doc comment above): it stays, even as a duplicate.
		if strings.ContainsAny(sn, "0123456789") {
			out = append(out, sn)
			continue
		}
		key := normaliseSentence(sn)
		if len(strings.Fields(key)) >= 3 && seen[key] {
			changed = true
			continue
		}
		seen[key] = true
		out = append(out, sn)
	}
	return strings.Join(out, ""), changed
}

func normaliseSentence(s string) string {
	s = strings.ToLower(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, ".!?")
}

// ---------------------------------------------------------------------------
// Whitespace / punctuation normalisation
// ---------------------------------------------------------------------------

var (
	collapseSpacesRe   = regexp.MustCompile(`[ \t]{2,}`)
	lineLeadSpacesRe   = regexp.MustCompile(`(?m)^[ \t]+`)
	spaceBeforePunctRe = regexp.MustCompile(`[ \t]+([,.!?;:])`)
	repeatPunctRe      = regexp.MustCompile(`([.!?])[.!?]+`)
	repeatCommaRe      = regexp.MustCompile(`,{2,}`)
	commaBeforeTermRe  = regexp.MustCompile(`,([.!?])`)
	trailingCommaRe    = regexp.MustCompile(`(?m),[ \t]*$`)
	leadingCommaRe     = regexp.MustCompile(`(?m)^,[ \t]*`)
	trailingSpacesRe   = regexp.MustCompile(`(?m)[ \t]+$`)
	blankLinesRe       = regexp.MustCompile(`\n{3,}`)
)

// cleanupText normalises whitespace and punctuation left behind by
// deletions: collapses space runs, strips line-leading spaces and
// commas, removes spaces before punctuation, collapses repeated
// punctuation, collapses blank-line runs, trims the ends. It never
// adds a word.
func cleanupText(s string) string {
	s = collapseSpacesRe.ReplaceAllString(s, " ")
	s = lineLeadSpacesRe.ReplaceAllString(s, "")
	s = spaceBeforePunctRe.ReplaceAllString(s, "$1")
	s = repeatPunctRe.ReplaceAllString(s, "$1")
	s = repeatCommaRe.ReplaceAllString(s, ",")
	s = commaBeforeTermRe.ReplaceAllString(s, "$1")
	s = trailingCommaRe.ReplaceAllString(s, "")
	s = leadingCommaRe.ReplaceAllString(s, "")
	s = trailingSpacesRe.ReplaceAllString(s, "")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// Protected-segment extraction (code fences, JSON, inline code, URLs)
// ---------------------------------------------------------------------------

type protectedBlock struct {
	placeholder string
	content     string
}

// extractProtected replaces every protected segment with a unique
// placeholder (NUL bytes, which never occur in prompts) and returns
// the extracted text plus the blocks for later restoration.
func extractProtected(s string) (string, []protectedBlock) {
	var blocks []protectedBlock
	add := func(content string) string {
		ph := fmt.Sprintf("\x00CAVEMAN%d\x00", len(blocks))
		blocks = append(blocks, protectedBlock{placeholder: ph, content: content})
		return ph
	}
	s = replaceSpans(s, fenceSpans(s), add)
	s = replaceSpans(s, jsonSpans(s), add)
	s = replaceSpans(s, inlineCodeSpans(s), add)
	s = replaceSpans(s, urlSpans(s), add)
	return s, blocks
}

func restoreProtected(s string, blocks []protectedBlock) string {
	for _, b := range blocks {
		s = strings.ReplaceAll(s, b.placeholder, b.content)
	}
	return s
}

// replaceSpans replaces each [start,end) span with add(content),
// skipping spans that overlap an earlier one.
func replaceSpans(s string, spans [][2]int, add func(string) string) string {
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var sb strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp[0] < pos || sp[1] <= sp[0] {
			continue
		}
		sb.WriteString(s[pos:sp[0]])
		sb.WriteString(add(s[sp[0]:sp[1]]))
		pos = sp[1]
	}
	sb.WriteString(s[pos:])
	return sb.String()
}

// fenceSpans returns the byte spans of closed ``` / ~~~ blocks and
// of an unclosed fence tail (opener to end of text). It reuses the
// fenceLine parser from fidelity.go.
func fenceSpans(s string) [][2]int {
	var spans [][2]int
	lines := strings.SplitAfter(s, "\n")
	off := 0
	inFence := false
	var fenceChar byte
	fenceRun := 0
	start := 0
	for _, ln := range lines {
		lineOff := off
		off += len(ln)
		c, run, rest := fenceLine(ln)
		if !inFence {
			if run >= 3 && !strings.ContainsRune(rest, rune(c)) {
				inFence, fenceChar, fenceRun, start = true, c, run, lineOff
			}
			continue
		}
		if c == fenceChar && run >= fenceRun && strings.TrimSpace(rest) == "" {
			end := off
			if strings.HasSuffix(ln, "\n") {
				end--
			}
			spans = append(spans, [2]int{start, end})
			inFence = false
		}
	}
	if inFence {
		spans = append(spans, [2]int{start, len(s)})
	}
	return spans
}

// jsonSpans returns the byte spans of complete JSON values (strict
// parse, mirroring fidelity.go) plus lenient brace/bracket spans
// that look JSON-shaped (balanced, containing ':' or ',', capped at
// 4 KiB) so single-quoted or trailing-comma JSON is also protected.
// Over-protection is safe: extraction/restoration is byte-exact.
func jsonSpans(s string) [][2]int {
	var spans [][2]int
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '{' && c != '[' {
			continue
		}
		var raw json.RawMessage
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		if err := dec.Decode(&raw); err != nil {
			continue
		}
		if len(raw) < 2 {
			continue
		}
		spans = append(spans, [2]int{i, i + len(raw)})
		i += len(raw) - 1
	}
	// Lenient pass on a copy with the strict spans blanked out.
	blanked := []byte(s)
	for _, sp := range spans {
		for j := sp[0]; j < sp[1] && j < len(blanked); j++ {
			blanked[j] = ' '
		}
	}
	b := string(blanked)
	for i := 0; i < len(b); i++ {
		var open, close byte
		need := byte(':')
		switch b[i] {
		case '{':
			open, close, need = '{', '}', ':'
		case '[':
			open, close, need = '[', ']', ','
		default:
			continue
		}
		depth := 0
		end := -1
		for j := i; j < len(b) && j-i <= 4096; j++ {
			if b[j] == open {
				depth++
			} else if b[j] == close {
				depth--
				if depth == 0 {
					end = j + 1
					break
				}
			}
		}
		if end < 0 || !strings.ContainsRune(b[i:end], rune(need)) {
			continue
		}
		spans = append(spans, [2]int{i, end})
		i = end - 1
	}
	return spans
}

// NOTE: a lone backtick pairs with the next opener, so the text
// between them is over-protected (safe direction — the content
// survives, only savings are lost). Pinned by
// TestCavemanLoneBacktickMisPair.
var inlineCodeRe = regexp.MustCompile("`[^`\n]+`")

// inlineCodeSpans returns the byte spans of `inline code` runs.
func inlineCodeSpans(s string) [][2]int {
	var spans [][2]int
	for _, loc := range inlineCodeRe.FindAllStringIndex(s, -1) {
		spans = append(spans, [2]int{loc[0], loc[1]})
	}
	return spans
}

// urlSpans returns the byte spans of URLs, trimmed the same way
// fidelity.go trims them (unbalanced closers, trailing sentence
// punctuation), so the gate and the engine agree on what a URL is.
func urlSpans(s string) [][2]int {
	var spans [][2]int
	for _, loc := range urlPattern.FindAllStringIndex(s, -1) {
		u := trimUnbalancedClosers(s[loc[0]:loc[1]])
		u = strings.TrimRight(u, ".,;:!?")
		if u == "" {
			continue
		}
		spans = append(spans, [2]int{loc[0], loc[0] + len(u)})
	}
	return spans
}

// ---------------------------------------------------------------------------
// Self-checks: the condensed text must be a reduction of the original
// ---------------------------------------------------------------------------

var (
	wordRe   = regexp.MustCompile(`[A-Za-z]+(?:'[A-Za-z]+)?`)
	numberRe = regexp.MustCompile(`\d+`)
)

func countRegexTokens(re *regexp.Regexp, s string) map[string]int {
	m := map[string]int{}
	for _, t := range re.FindAllString(s, -1) {
		m[strings.ToLower(t)]++
	}
	return m
}

// wordsSubset reports whether every word in condensed (with
// multiplicity) also occurs in original: no new word introduced.
func wordsSubset(original, condensed string) bool {
	orig := countRegexTokens(wordRe, original)
	for w, n := range countRegexTokens(wordRe, condensed) {
		if orig[w] < n {
			return false
		}
	}
	return true
}

// numbersEqual reports whether the digit runs of condensed are
// exactly the digit runs of original (same values, same counts):
// numbers, dates and identifiers are never altered or dropped.
func numbersEqual(original, condensed string) bool {
	orig := countRegexTokens(numberRe, original)
	cond := countRegexTokens(numberRe, condensed)
	if len(orig) != len(cond) {
		return false
	}
	for k, n := range orig {
		if cond[k] != n {
			return false
		}
	}
	return true
}
