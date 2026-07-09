// Package extract distils conversation prose into durable, classified facts
// without an LLM — a port of mempalace's heuristic extractor. It pulls the
// decisions, preferences, and problems worth keeping out of a short-term intake
// capture, as durable memories that earn trust on recall. Marker-driven and
// deliberately conservative: a miss just means no extra memory, never a wrong
// one. Both the backfill importer and write-time extraction share this logic.
package extract

import (
	"regexp"
	"strings"

	"github.com/eleboucher/memini/internal/memory"
)

// Kind labels an extracted memory; it doubles as the memory's first tag.
type Kind string

const (
	KindDecision   Kind = "decision"
	KindPreference Kind = "preference"
	KindProblem    Kind = "problem"
	KindFact       Kind = "fact"  // architecture/config/factual statement
	KindHowTo      Kind = "howto" // setup/instruction steps
)

// Tier maps an extracted kind to its memory tier: a preference is a how-to rule,
// so it's procedural; decisions and problems are durable facts, so semantic.
func (k Kind) Tier() memory.Tier {
	if k == KindPreference || k == KindHowTo {
		return memory.TierProcedural
	}
	return memory.TierSemantic
}

// MinConfidence gates an extraction: confidence is min(1, score/5) where score
// is the count of distinct markers that match plus a length bonus, so a lone
// marker on a short segment (0.2) is dropped while two distinct markers, or one
// in a longer segment, pass.
const MinConfidence = 0.3

// MaxPerExchange caps extractions from a single exchange so one rambling turn
// can't flood the store.
const MaxPerExchange = 5

type markerSet struct {
	kind Kind
	pats []*regexp.Regexp
}

// markerSets are matched against lowercased prose. The patterns are RE2-safe
// (no lookahead) ports of mempalace's decision/preference/problem markers.
var markerSets = func() []markerSet {
	compile := func(raw []string) []*regexp.Regexp {
		out := make([]*regexp.Regexp, len(raw))
		for i, r := range raw {
			out[i] = regexp.MustCompile(r)
		}
		return out
	}
	return []markerSet{
		{KindDecision, compile([]string{
			`\blet'?s (use|go with|try|pick|choose|switch to)\b`,
			`\bwe (should|decided|chose|went with|picked|settled on)\b`,
			`\bi'?m going (to|with)\b`,
			`\bbetter (to|than|approach|option|choice)\b`,
			`\binstead of\b`, `\brather than\b`,
			`\bthe reason (is|was|being)\b`,
			`\btrade-?off\b`, `\bpros and cons\b`,
		})},
		{KindPreference, compile([]string{
			`\bi prefer\b`, `\balways use\b`, `\bnever use\b`,
			`\bdon'?t (ever |like to )?(use|do|mock|stub|import)\b`,
			`\bi like (to|when|how)\b`, `\bi hate (when|how|it when)\b`,
			`\bplease (always|never|don'?t)\b`,
			`\bmy (rule|preference|style|convention) is\b`,
			`\bwe (always|never)\b`, `\buse\b.*\binstead of\b`,
		})},
		{KindProblem, compile([]string{
			`\b(bug|error|crash|fail|broke|broken|issue|problem)\b`,
			`\bdoesn'?t work\b`, `\bnot working\b`,
			`\bkeeps? (failing|crashing|breaking|erroring)\b`,
			`\broot cause\b`, `\bthe (problem|issue|bug) (is|was)\b`,
			`\bthe fix (is|was)\b`, `\bworkaround\b`,
			`\bfixed (it|the|by)\b`, `\bsolution (is|was)\b`,
			`\bresolved\b`, `\bpatched\b`,
		})},
		{KindFact, compile([]string{
			`\bthe (system|app|server|service|backend|store|database|frontend|api|project|repo|codebase|module|package) ` +
				`(uses|runs on|is|requires|relies on|depends on|is built with|is written in)\b`,
			`\bdeploy(ed|s|ment) (to|on|via|using)\b`,
			`\b(tests?|ci|pipeline) run (on|in|via|using)\b`,
			`\bby default[,;]?\s+(the|it|they|we|this)\b`,
			`\bthe (config|configuration) (is|file is|lives in|is at)\b`,
			`\b(environment variable|env var)\b`,
		})},
		{KindHowTo, compile([]string{
			`\bto (set up|configure|install|deploy|run|test|debug|enable|disable)\b`,
			`\byou (need to|have to|must) (set|configure|run|install|add|create|enable|disable)\b`,
			`\bsteps? (are|to|for)\b`,
			`\brun (the (command|following)|this (command|snippet))\b`,
		})},
	}
}()

// codeLinePatterns identify lines that are code/command/output rather than prose,
// so a fenced snippet doesn't get scored as a "decision".
var codeLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^[$#]\s`),
	regexp.MustCompile(`^(cd|source|echo|export|pip|npm|git|python|bash|curl|wget|mkdir|rm|cp|mv|ls|cat|grep|find|chmod|sudo|brew|docker)\s`),
	regexp.MustCompile("^```"),
	regexp.MustCompile(`^(import|from|def|class|function|const|let|var|return)\s`),
	regexp.MustCompile(`^[A-Z_]{2,}=`),
	regexp.MustCompile(`^(if|for|while|try|except|elif|else:)\b`),
	regexp.MustCompile(`^\w+\.\w+\(`),
}

// Result is one classified segment ready to become a durable memory.
type Result struct {
	Kind    Kind
	Content string
}

// Typed scans a block of conversation text and returns the
// decision/preference/problem segments that clear the confidence gate, capped at
// MaxPerExchange. Empty when nothing qualifies.
func Typed(text string) []Result {
	var out []Result
	for seg := range strings.SplitSeq(text, "\n\n") {
		seg = strings.TrimSpace(seg)
		if len(seg) < 20 {
			continue
		}
		prose := strings.ToLower(proseOnly(seg))
		bestKind, bestScore := Kind(""), 0
		for _, ms := range markerSets {
			if s := scoreMarkers(prose, ms.pats); s > bestScore {
				bestKind, bestScore = ms.kind, s
			}
		}
		if bestScore == 0 {
			continue
		}
		score := bestScore + lengthBonus(seg)
		if float64(min(score, 5))/5.0 < MinConfidence {
			continue
		}
		out = append(out, Result{Kind: bestKind, Content: seg})
		if len(out) >= MaxPerExchange {
			break
		}
	}
	return out
}

// scoreMarkers counts how many distinct patterns match text. Distinct-match
// (not total occurrences) so a single word repeated can't inflate the score past
// the confidence gate; two different markers is a genuinely stronger signal.
func scoreMarkers(text string, pats []*regexp.Regexp) int {
	score := 0
	for _, p := range pats {
		if p.MatchString(text) {
			score++
		}
	}
	return score
}

// lengthBonus rewards longer segments, which carry more context.
func lengthBonus(seg string) int {
	switch {
	case len(seg) > 500:
		return 2
	case len(seg) > 200:
		return 1
	default:
		return 0
	}
}

// proseOnly drops code/command lines so classification scores on prose. Falls
// back to the original text when nothing prose-like remains.
func proseOnly(seg string) string {
	var prose []string
	inFence := false
	for line := range strings.SplitSeq(seg, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence || isCodeLine(t) {
			continue
		}
		prose = append(prose, line)
	}
	if joined := strings.TrimSpace(strings.Join(prose, "\n")); joined != "" {
		return joined
	}
	return seg
}

func isCodeLine(t string) bool {
	if t == "" {
		return false
	}
	for _, p := range codeLinePatterns {
		if p.MatchString(t) {
			return true
		}
	}
	return false
}

// hedgePatterns veto whole-write classification: tentative phrasing must stay
// episodic history rather than become a durable fact asserted confidently.
var hedgePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bmaybe\b`), regexp.MustCompile(`\bi think\b`),
	regexp.MustCompile(`\bnot sure\b`), regexp.MustCompile(`\bpossibly\b`),
	regexp.MustCompile(`\bmight\b`), regexp.MustCompile(`\bprobably\b`),
	regexp.MustCompile(`\breportedly\b`), regexp.MustCompile(`\bi guess\b`),
	regexp.MustCompile(`\bcould be\b`),
}

// roleScaffold marks transcript-shaped content: a captured exchange is session
// history, not a single fact, however many markers it contains.
var roleScaffold = regexp.MustCompile(`(?mi)^\s*(user|assistant)\s*:`)

// ClassifyMaxChars bounds a classifiable write: one durable fact is terse;
// longer content is session history even when it contains decision markers.
const ClassifyMaxChars = 400

// Classify labels an entire write whose caller picked no tier. It is stricter
// than Typed because the verdict covers the full text rather than an extracted
// segment: the content must be short, prose-shaped, unhedged, not a transcript,
// and clear the same marker-confidence gate. ok=false means no confident call —
// callers fall back to the working-intake default.
func Classify(text string) (Kind, bool) {
	seg := strings.TrimSpace(text)
	if len(seg) < 20 || len(seg) > ClassifyMaxChars {
		return "", false
	}
	if roleScaffold.MatchString(seg) {
		return "", false
	}
	prose := strings.ToLower(proseOnly(seg))
	for _, h := range hedgePatterns {
		if h.MatchString(prose) {
			return "", false
		}
	}
	bestKind, bestScore := Kind(""), 0
	for _, ms := range markerSets {
		if s := scoreMarkers(prose, ms.pats); s > bestScore {
			bestKind, bestScore = ms.kind, s
		}
	}
	if bestScore == 0 {
		return "", false
	}
	if float64(min(bestScore+lengthBonus(seg), 5))/5.0 < MinConfidence {
		return "", false
	}
	return bestKind, true
}
