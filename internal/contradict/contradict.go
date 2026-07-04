// Package contradict classifies a pair of durable memory texts without an LLM:
// is the new write a restatement of the old fact (corroborate it), an update
// that contradicts it (the old fact is now stale), or a distinct claim (do
// nothing)? Embedding similarity cannot make this call — a value swap ("TTL is
// 10 minutes" → "TTL is 15 minutes") embeds in the same band as a restatement
// (measured in bench/dedup_test.go) — so this is a lexical differ in the
// tradition of de Marneffe et al. (ACL 2008): only the surface-detectable
// contradiction classes (changed value slot, flipped polarity) are in scope,
// and anything ambiguous is deliberately not an Update. Precision-first: the
// costly error is flagging a restatement as an update, which both downranks a
// live fact and loses its corroboration.
package contradict

import (
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"

	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/memory"
)

// Class is the detector's verdict on a (new, old) memory pair.
type Class int

const (
	// Distinct means different claims: the pair shares too little to compare.
	Distinct Class = iota
	// Restatement means the same claim reworded: the old fact is re-observed.
	Restatement
	// Update means the new text contradicts the old: same subject and
	// attribute, but a changed value or flipped polarity.
	Update
)

func (c Class) String() string {
	switch c {
	case Restatement:
		return "restatement"
	case Update:
		return "update"
	default:
		return "distinct"
	}
}

// Result is a classification with the evidence that produced it, for merge
// hints, logs, and bench audits.
type Result struct {
	Class  Class
	Reason string
}

// Config holds the detector's decision thresholds. The zero value is unusable;
// start from Default. Fields are exported so the bench can sweep them.
type Config struct {
	// OverlapFloor is the minimum shared-content overlap coefficient
	// (|A∩B| / min(|A|,|B|)) for the pair to be about the same claim at all.
	OverlapFloor float64
	// ResidueMax caps the non-value differing content tokens per side for a
	// value swap (and on the new side for a polarity flip): more residue means
	// the texts differ in substance, not just in one slot.
	ResidueMax int
	// NegOverlapFloor is the minimum containment of the new text in the old
	// (|A∩B| / |A|) for the polarity trigger: a genuine retraction talks about
	// the things the old fact mentions, and little else.
	NegOverlapFloor float64
	// AliasPrefixMin treats two differing value tokens as aliases of one name
	// when one is a prefix of the other and the shorter is at least this long
	// ("postgres"/"postgresql" yes, "go"/"golang" no).
	AliasPrefixMin int
}

// Default is the shipped configuration, priced in bench/contradiction_test.go.
var Default = Config{OverlapFloor: 0.5, ResidueMax: 2, NegOverlapFloor: 0.6, AliasPrefixMin: 4}

// Classify labels newText against oldText. It never returns Update without a
// shared topical anchor plus a decisive differing value or polarity cue.
func Classify(newText, oldText string, cfg Config) Result {
	if memory.Fingerprint(newText) == memory.Fingerprint(oldText) {
		return Result{Restatement, "identical content"}
	}
	nf, of := analyze(newText), analyze(oldText)
	if len(nf.tokens) == 0 || len(of.tokens) == 0 {
		return Result{Distinct, "no content tokens"}
	}
	shared := intersect(nf.tokens, of.tokens)
	onlyNew := subtract(nf.tokens, of.tokens)
	onlyOld := subtract(of.tokens, nf.tokens)
	overlap := float64(len(shared)) / float64(min(len(nf.tokens), len(of.tokens)))
	sharedEntity := len(intersect(nf.entities, of.entities)) > 0
	if overlap < cfg.OverlapFloor || (!sharedEntity && len(shared) < 2) {
		return Result{Distinct, fmt.Sprintf("overlap %.2f below floor", overlap)}
	}

	// Value swap: BOTH sides changed a narrow-class value slot (number, time,
	// quoted span, named entity) with almost nothing else differing. A value
	// added on one side only is the added-detail restatement shape and never
	// fires.
	vNew := intersect(onlyNew, nf.values)
	vOld := intersect(onlyOld, of.values)
	vNew, vOld = dropAliases(vNew, vOld, cfg.AliasPrefixMin)
	residNew := subtract(subtract(onlyNew, nf.values), cueWords)
	residOld := subtract(subtract(onlyOld, of.values), cueWords)
	if len(vNew) > 0 && len(vOld) > 0 &&
		len(residNew) <= cfg.ResidueMax && len(residOld) <= cfg.ResidueMax {
		return Result{Update, fmt.Sprintf("value swap: %q -> %q", first(vOld), first(vNew))}
	}

	// Polarity flip: the new text negates or retires the old claim. A genuine
	// retraction names the old fact's subject and little else, so it must be
	// contained in the old text — old-side residue (detail the retraction does
	// not repeat) is free. Negation must be new-side only: facts often carry
	// negation as part of the claim ("opaque cursors, not page numbers"), so a
	// positive restatement of a negated fact must not read as a flip. A bare
	// negation without an explicit retro cue is the weaker signal and needs
	// near-total containment with at most one residual token.
	if sharedEntity || len(shared) >= 3 {
		cue := retroCue(newText)
		negFlip := nf.negated && !of.negated
		// Cue tokens are the change signal, not divergence: keep them out of
		// both sides of the containment ratio.
		newContent := subtract(nf.tokens, cueWords)
		containment := float64(len(subtract(shared, cueWords))) / float64(max(1, len(newContent)))
		switch {
		case cue != "" && containment >= cfg.NegOverlapFloor && len(residNew) <= cfg.ResidueMax:
			return Result{Update, "polarity: " + cue}
		case negFlip && containment >= max(cfg.NegOverlapFloor, 0.75) && len(residNew) <= 1:
			return Result{Update, "polarity: negation flip"}
		}
	}

	return Result{Restatement, fmt.Sprintf("overlap %.2f, no decisive change", overlap)}
}

// features is one text reduced to comparable sets.
type features struct {
	tokens   map[string]bool // normalized content tokens (values included)
	values   map[string]bool // digit-bearing, quoted, or named-entity tokens
	entities map[string]bool // full entity keys from extract.Entities
	negated  bool
}

var (
	tokenRe  = regexp.MustCompile(`[a-z]+|[0-9]+(?:[.:][0-9]+)*`)
	ampmRe   = regexp.MustCompile(`\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b`)
	clockRe  = regexp.MustCompile(`\b0(\d):(\d{2})\b`)
	quotedRe = regexp.MustCompile("\"([^\"]{1,80})\"|`([^`]{1,80})`|'([^']{1,80})'")
)

func analyze(text string) features {
	f := features{
		tokens:   map[string]bool{},
		values:   map[string]bool{},
		entities: map[string]bool{},
	}
	entityWords := map[string]bool{}
	for _, e := range extract.Entities(text) {
		f.entities[e] = true
		for w := range strings.FieldsSeq(e) {
			entityWords[w] = true
		}
	}

	lower := normalizeTimes(strings.ToLower(text))
	quoted := map[string]bool{}
	for _, m := range quotedRe.FindAllStringSubmatch(lower, -1) {
		for _, span := range m[1:] {
			for _, w := range tokenRe.FindAllString(span, -1) {
				quoted[w] = true
			}
		}
	}

	for _, tok := range tokenRe.FindAllString(lower, -1) {
		isValue := quoted[tok] || entityWords[tok] || tok[0] >= '0' && tok[0] <= '9'
		if tok[0] >= 'a' {
			if negators[tok] {
				f.negated = true
			}
			if n, ok := numberWords[tok]; ok {
				tok, isValue = n, true
			} else {
				tok = normalizeWord(tok)
				isValue = isValue || entityWords[tok]
			}
		}
		if len(tok) < 2 || extract.Stopword(tok) || extraStopwords[tok] {
			continue
		}
		f.tokens[tok] = true
		if isValue {
			f.values[tok] = true
		}
	}
	return f
}

// normalizeTimes rewrites am/pm clock times to 24-hour hh:mm and strips the
// leading zero from zero-padded hours, so "2am", "2:00" and "02:00" compare
// equal.
func normalizeTimes(lower string) string {
	lower = ampmRe.ReplaceAllStringFunc(lower, func(m string) string {
		g := ampmRe.FindStringSubmatch(m)
		h := atoiSafe(g[1]) % 12
		if g[3] == "pm" {
			h += 12
		}
		mm := g[2]
		if mm == "" {
			mm = "00"
		}
		return fmt.Sprintf("%d:%s", h, mm)
	})
	return clockRe.ReplaceAllString(lower, "$1:$2")
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// normalizeWord maps a letter token to a comparable stem: irregular verb
// forms, a light suffix strip (with doubled-consonant undoubling and trailing-e
// trim so "capped"/"cap" and "expires"/"expire" meet), then unit aliases. Both
// texts pass through the same normalizer, so consistency matters more than
// linguistic correctness.
func normalizeWord(tok string) string {
	if base, ok := irregularVerbs[tok]; ok {
		tok = base
	}
	for _, suf := range [...]string{"ies", "ing", "ed", "es", "s"} {
		if strings.HasSuffix(tok, suf) && len(tok)-len(suf) >= 3 {
			tok = tok[:len(tok)-len(suf)]
			if suf == "ies" {
				tok += "y"
			}
			break
		}
	}
	if n := len(tok); n >= 4 && tok[n-1] == tok[n-2] && !strings.ContainsRune("aeiou", rune(tok[n-1])) {
		tok = tok[:n-1]
	}
	if n := len(tok); n >= 4 && tok[n-1] == 'e' {
		tok = tok[:n-1]
	}
	if alias, ok := unitAliases[tok]; ok {
		return alias
	}
	return tok
}

// retroCue returns the retrospective change phrase found in the new text, if
// any. "instead of"/"rather than" are deliberately absent: they are decision
// markers (extract.go) that appear in the original statement of a decision, so
// a restatement may legitimately carry them.
func retroCue(text string) string {
	lower := strings.ToLower(text)
	for _, cue := range retroCues {
		if strings.Contains(lower, cue) {
			return cue
		}
	}
	return ""
}

var retroCues = []string{
	"no longer", "anymore", "any more",
	"switched from", "switched to", "switched off", "switched away",
	"migrated from", "migrated to", "migrated off", "migrated away",
	"moved off", "moved away from",
	"stopped using", "stopped doing", "we stopped",
	"deprecated", "used to", "retired",
}

// cueWords are change-signal tokens exempted from residue counts: their
// presence is what an update looks like, not substantive divergence.
var cueWords = map[string]bool{
	"longer": true, "anymore": true, "switched": true, "switch": true,
	"migrated": true, "migrate": true, "moved": true, "mov": true,
	"deprecated": true, "deprecat": true, "stopped": true, "stopp": true, "stop": true,
	"used": true, "using": true, "us": true, "doing": true,
	"retired": true, "retir": true,
	"dropped": true, "dropp": true, "drop": true, "instead": true, "currently": true,
}

// negators are the token stems that signal negation; the tokenizer splits
// "don't" into "don"+"t", so contraction stems stand in for the full forms.
var negators = map[string]bool{
	"not": true, "no": true, "never": true, "cannot": true,
	"don": true, "doesn": true, "didn": true, "won": true,
	"isn": true, "aren": true, "wasn": true, "weren": true,
}

// extraStopwords supplement extract's list with function words that matter for
// content diffing but not for entity spans.
var extraStopwords = map[string]bool{
	"per": true, "via": true, "through": true, "throughout": true,
}

var numberWords = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4",
	"five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9",
	"ten": "10", "eleven": "11", "twelve": "12", "thirteen": "13",
	"fourteen": "14", "fifteen": "15", "sixteen": "16", "seventeen": "17",
	"eighteen": "18", "nineteen": "19", "twenty": "20", "thirty": "30",
	"forty": "40", "fifty": "50", "sixty": "60", "seventy": "70",
	"eighty": "80", "ninety": "90", "hundred": "100", "thousand": "1000",
}

var irregularVerbs = map[string]string{
	"sent": "send", "ran": "run", "kept": "keep", "built": "build",
	"wrote": "write", "took": "take", "gave": "give",
}

var unitAliases = map[string]string{
	"megabyte": "mb", "gigabyte": "gb", "kilobyte": "kb", "terabyte": "tb",
	"second": "sec", "minute": "min", "hour": "hr", "millisecond": "ms",
	"request": "req", "percent": "percent",
}

// dropAliases removes value-token pairs that are two spellings of one name:
// numerically equal ("10"/"10.0") or a shared prefix at least minPrefix long
// ("postgres"/"postgresql").
func dropAliases(vNew, vOld map[string]bool, minPrefix int) (map[string]bool, map[string]bool) {
	outNew, outOld := clone(vNew), clone(vOld)
	for a := range vNew {
		for b := range vOld {
			if !outNew[a] || !outOld[b] {
				continue
			}
			if numEqual(a, b) || prefixAlias(a, b, minPrefix) {
				delete(outNew, a)
				delete(outOld, b)
			}
		}
	}
	return outNew, outOld
}

func numEqual(a, b string) bool {
	if a == "" || b == "" || a[0] < '0' || a[0] > '9' || b[0] < '0' || b[0] > '9' {
		return false
	}
	return strings.TrimRight(strings.TrimSuffix(a, ".0"), ".") == strings.TrimRight(strings.TrimSuffix(b, ".0"), ".")
}

func prefixAlias(a, b string, minPrefix int) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= minPrefix && strings.HasPrefix(b, a)
}

func intersect(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if b[k] {
			out[k] = true
		}
	}
	return out
}

func subtract(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func clone(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	maps.Copy(out, m)
	return out
}

// first returns the lexically smallest key so Reason strings are
// deterministic.
func first(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}
