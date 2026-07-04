package extract

import (
	"strings"
	"unicode"
)

// MaxEntities caps the entities extracted from one memory, keeping the first
// (reading-order) mentions so one long transcript can't flood the entity index.
const MaxEntities = 12

// maxEntitySpanTokens drops capitalized runs longer than this: a span that long
// is a shouted heading or title-case sentence, not a name.
const maxEntitySpanTokens = 4

// Entities extracts salient named-entity keys from a memory's content without
// an LLM: maximal runs of capitalized tokens ("Charlotte's Web", "Black
// Friday", "Postgres"), normalized to lowercase with possessives stripped.
// Deliberately precision-first — noisy entities poison every consumer — so it
// only trusts capitalization evidence:
//   - a sentence-initial span counts only when it has ≥2 tokens, its lead word
//     is also seen capitalized mid-sentence in the same text, or the word
//     carries its own casing signal (internal uppercase or digits, "iPhone",
//     "Qwen3");
//   - stopwords (function words, greetings, months/weekdays) never join a
//     span; they split it instead;
//   - purely numeric or single-letter tokens never start an entity.
//
// Lowercase concept nouns ("pottery", "the retry policy") are out of scope by
// design: without a POS tagger they cannot be told from ordinary prose.
func Entities(text string) []string {
	spans := entitySpans(text)

	// Words seen capitalized mid-sentence anywhere in the text: evidence that
	// the same word at a sentence start is a name, not just sentence case.
	midWords := make(map[string]bool)
	for _, sp := range spans {
		for i, w := range sp.words {
			if !sp.initial || i > 0 {
				midWords[w] = true
			}
		}
	}

	seen := make(map[string]bool)
	var out []string
	for _, sp := range spans {
		if sp.initial && len(sp.words) < 2 && !midWords[sp.words[0]] && !selfEvidentEntity(sp.raw[0]) {
			continue
		}
		key := strings.Join(sp.words, " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
		if len(out) >= MaxEntities {
			break
		}
	}
	return out
}

// entitySpan is one maximal run of capitalized tokens: raw keeps the original
// tokens (for casing-signal checks), words the normalized forms.
type entitySpan struct {
	words   []string
	raw     []string
	initial bool // span starts at a sentence start
}

// entitySpans scans text token by token, accumulating capitalized runs and
// tracking sentence boundaries (line starts and .!?; enders).
func entitySpans(text string) []entitySpan {
	var spans []entitySpan
	var cur entitySpan
	flush := func() {
		if len(cur.words) > 0 && len(cur.words) <= maxEntitySpanTokens && !allCalendar(cur.words) {
			spans = append(spans, cur)
		}
		cur = entitySpan{}
	}
	for line := range strings.SplitSeq(text, "\n") {
		atSentenceStart := true
		inBracket := false
		for tok := range strings.FieldsSeq(line) {
			// Bracketed runs ("[8:00 pm on 8 May, 2023]", "[WARN]") are
			// scaffolding, not prose: skip them without consuming the sentence
			// start, so the token after the bracket is still sentence-initial.
			if inBracket || strings.HasPrefix(tok, "[") {
				inBracket = !strings.Contains(tok, "]")
				flush()
				continue
			}
			trimmed, endsSentence := trimToken(tok)
			if trimmed == "" {
				flush()
				// Bare punctuation keeps the sentence state it implies.
				if endsSentence {
					atSentenceStart = true
				}
				continue
			}
			norm := normalizeEntityWord(trimmed)
			switch {
			case !startsUpper(trimmed), entityStopword(norm), norm == "", !hasLetter(norm), len([]rune(norm)) < 2:
				flush()
			default:
				if len(cur.words) == 0 {
					cur.initial = atSentenceStart
				}
				cur.words = append(cur.words, norm)
				cur.raw = append(cur.raw, trimmed)
			}
			if endsSentence {
				flush()
			}
			atSentenceStart = endsSentence
		}
		flush()
	}
	flush()
	return spans
}

// trimToken strips surrounding punctuation from one whitespace-delimited token
// and reports whether it ended a sentence (.!?;: or closing quote after one).
func trimToken(tok string) (string, bool) {
	trimmed := strings.TrimFunc(tok, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’' && r != '-'
	})
	ends := strings.ContainsAny(trailingPunct(tok, trimmed), ".!?;:")
	return trimmed, ends
}

// trailingPunct returns the punctuation trimToken removed from tok's tail.
func trailingPunct(tok, trimmed string) string {
	if i := strings.LastIndex(tok, trimmed); trimmed != "" && i >= 0 {
		return tok[i+len(trimmed):]
	}
	return tok
}

// startsUpper reports whether the token's first rune is an uppercase letter.
func startsUpper(tok string) bool {
	for _, r := range tok {
		return unicode.IsUpper(r)
	}
	return false
}

// selfEvidentEntity reports casing evidence beyond sentence position: internal
// uppercase (CamelCase, "LGBTQ") or digits ("Qwen3").
func selfEvidentEntity(raw string) bool {
	for i, r := range raw {
		if i > 0 && (unicode.IsUpper(r) || unicode.IsDigit(r)) {
			return true
		}
	}
	return false
}

// hasLetter reports whether the word contains at least one letter, so purely
// numeric tokens ("2023", "8gb" passes) never form an entity on their own.
func hasLetter(w string) bool {
	return strings.ContainsFunc(w, unicode.IsLetter)
}

// normalizeEntityWord lowercases a token and strips a trailing possessive.
func normalizeEntityWord(tok string) string {
	w := strings.ToLower(tok)
	w = strings.ReplaceAll(w, "’", "'")
	w = strings.TrimSuffix(w, "'s")
	return strings.TrimSuffix(w, "'")
}

// entityStopword reports whether a normalized word can never be (part of) an
// entity: function words, auxiliaries, greetings/discourse markers, and
// calendar words. A contraction is judged by its pre-apostrophe stem too, so
// "don't"/"let's"/"we're" need no separate entries.
func entityStopword(w string) bool {
	if entityStopwords[w] {
		return true
	}
	if i := strings.IndexByte(w, '\''); i > 0 && entityStopwords[w[:i]] {
		return true
	}
	return false
}

// calendarWords may join a span ("Black Friday") but never form an entity on
// their own: a bare month or weekday is a date, not a name. Checked span-wide
// by allCalendar rather than in entityStopword, which would split the span.
var calendarWords = map[string]bool{
	"monday": true, "tuesday": true, "wednesday": true, "thursday": true,
	"friday": true, "saturday": true, "sunday": true,
	"january": true, "february": true, "march": true, "april": true,
	"may": true, "june": true, "july": true, "august": true,
	"september": true, "october": true, "november": true, "december": true,
}

// allCalendar reports whether every word of a span is a calendar word.
func allCalendar(words []string) bool {
	for _, w := range words {
		if !calendarWords[w] {
			return false
		}
	}
	return true
}

var entityStopwords = func() map[string]bool {
	words := []string{
		// pronouns & determiners
		"i", "me", "my", "mine", "we", "us", "our", "ours", "you", "your", "yours",
		"he", "him", "his", "she", "her", "hers", "it", "its", "they", "them",
		"their", "theirs", "this", "that", "these", "those", "the", "a", "an",
		"some", "any", "each", "every", "all", "both", "few", "many", "much",
		"one", "two", "other", "another", "such", "same", "own", "no", "none",
		// question words
		"what", "when", "where", "who", "whom", "whose", "why", "how", "which",
		// auxiliaries & common verbs (incl. contraction stems)
		"is", "are", "was", "were", "be", "been", "being", "am", "do", "does",
		"did", "don", "didn", "doesn", "isn", "aren", "wasn", "weren", "have",
		"has", "had", "haven", "hasn", "hadn", "will", "would", "wouldn",
		"should", "shouldn", "could", "couldn", "can", "cannot", "may", "might",
		"must", "shall", "won", "let", "get", "got", "go", "going", "gonna",
		"come", "make", "see", "know", "think", "want", "need", "try", "keep",
		// conjunctions & prepositions
		"and", "or", "but", "nor", "so", "yet", "if", "then", "than", "as",
		"at", "by", "for", "from", "in", "into", "of", "off", "on", "onto",
		"out", "over", "to", "under", "up", "down", "with", "without", "about",
		"after", "before", "during", "while", "since", "until", "because",
		"though", "although", "however", "also", "too", "very", "not", "there",
		"here", "again", "once", "just", "still", "even", "only", "now",
		// discourse, greetings, reactions
		"hey", "hi", "hello", "bye", "goodbye", "wow", "oh", "ah", "ok", "okay",
		"yes", "yeah", "yep", "no", "nope", "thanks", "thank", "please",
		"sorry", "sure", "great", "good", "cool", "nice", "well", "right",
		"awesome", "amazing", "perfect", "exactly", "definitely", "absolutely",
		"maybe", "perhaps", "anyway", "alright", "congrats", "congratulations",
		"cheers", "dear", "happy", "glad", "sounds", "haha", "lol", "hmm",
		"today", "tomorrow", "yesterday", "tonight",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()
