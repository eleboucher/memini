package search

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/store"
)

// Temporal targeting boosts candidates dated near the time a query references
// ("what did I do three weeks ago") rather than near now, which the monotonic
// recency factor cannot do. It only fires when the query names a relative time.
//
// DefaultTemporalBoost is the maximum relevance amplification a perfectly
// on-target candidate gets (score × (1 + boost) at zero distance); it ramps to
// 0 by 3x the tolerance. Multiplicative so proximity can reorder comparably
// relevant candidates without letting a date-near but weakly relevant memory
// displace a strong match.
const DefaultTemporalBoost = 0.40

// TimeAnchor is a resolved relative-time reference: an answer is expected
// roughly Days before the query's "now", give or take Tolerance days.
type TimeAnchor struct {
	Days      int
	Tolerance int
}

// AnchorExtractor resolves a query's time reference, if any, against the
// query's "now" (absolute references like "in March" need it). The regex
// implementation is the no-LLM default; an LLM extractor (looser phrasing like
// "a couple weeks before my trip") can plug in via the same interface.
type AnchorExtractor interface {
	Anchor(query string, now time.Time) (TimeAnchor, bool)
}

// RegexAnchorExtractor matches common English time phrases — relative ("3
// weeks ago") and absolute ("in March", "March 14th", "last summer", "in
// 2025") — firing only on templated expressions.
type RegexAnchorExtractor struct{}

var temporalPatterns = []struct {
	re        *regexp.Regexp
	days, tol int
	numMul    int // when != 0, multiply the captured integer by this
}{
	{regexp.MustCompile(`(\d+)\s+days?\s+ago`), 0, 2, 1},
	{regexp.MustCompile(`a\s+couple\s+(?:of\s+)?days?\s+ago`), 2, 2, 0},
	{regexp.MustCompile(`yesterday`), 1, 1, 0},
	{regexp.MustCompile(`(\d+)\s+weeks?\s+ago`), 0, 5, 7},
	{regexp.MustCompile(`(?:a|last)\s+week\b`), 7, 3, 0},
	{regexp.MustCompile(`(\d+)\s+months?\s+ago`), 0, 10, 30},
	{regexp.MustCompile(`(?:a|last)\s+month\b`), 30, 7, 0},
	{regexp.MustCompile(`(?:a|last)\s+year\b`), 365, 30, 0},
	{regexp.MustCompile(`recently`), 14, 14, 0},
}

// Anchor implements AnchorExtractor. Relative phrases resolve without now;
// absolute references (month, month+day, season, year) resolve to the most
// recent past occurrence relative to now and are skipped when now is zero.
func (RegexAnchorExtractor) Anchor(query string, now time.Time) (TimeAnchor, bool) {
	q := strings.ToLower(query)
	for _, p := range temporalPatterns {
		m := p.re.FindStringSubmatch(q)
		if m == nil {
			continue
		}
		if p.numMul != 0 && len(m) > 1 {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			return TimeAnchor{Days: n * p.numMul, Tolerance: p.tol}, true
		}
		return TimeAnchor{Days: p.days, Tolerance: p.tol}, true
	}
	if now.IsZero() {
		return TimeAnchor{}, false
	}
	return absoluteAnchor(q, now)
}

var monthsByName = map[string]time.Month{
	"january": time.January, "february": time.February, "march": time.March,
	"april": time.April, "may": time.May, "june": time.June, "july": time.July,
	"august": time.August, "september": time.September, "october": time.October,
	"november": time.November, "december": time.December,
}

const monthAlternation = `(january|february|march|april|may|june|july|august|september|october|november|december)`

var (
	// "October 13, 2023" — a fully-specified date.
	monthDayYearPattern = regexp.MustCompile(`\b` + monthAlternation + `\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+((?:19|20)\d{2})\b`)
	// "July 2023" — a month in an explicit year.
	monthYearPattern = regexp.MustCompile(`\b` + monthAlternation + `\s+((?:19|20)\d{2})\b`)
	// "March 14", "on march 14th" — a specific day.
	monthDayPattern = regexp.MustCompile(`\b` + monthAlternation + `\s+(\d{1,2})(?:st|nd|rd|th)?\b`)
	// "in March", "last march", "back in march", "since march" — a month. The
	// leading preposition keeps bare month mentions ("May I ask") from firing.
	monthPattern = regexp.MustCompile(`\b(?:in|last|back in|since|during)\s+` + monthAlternation + `\b`)
	// "last summer", "in the winter", "this past fall" — a season (northern
	// mid-season dates; the tolerance absorbs the hemisphere ambiguity).
	seasonPattern = regexp.MustCompile(`\b(?:last|in the|this past)\s+(spring|summer|fall|autumn|winter)\b`)
	// "in 2025" — a year.
	yearPattern = regexp.MustCompile(`\bin\s+((?:19|20)\d{2})\b`)
)

// seasonMidMonth anchors each season at its northern mid-point.
var seasonMidMonth = map[string]time.Month{
	"spring": time.April, "summer": time.July,
	"fall": time.October, "autumn": time.October, "winter": time.January,
}

// absoluteAnchor resolves date references, most specific first: an explicit
// year pins the target exactly; without one, the most recent past occurrence
// relative to now wins.
func absoluteAnchor(q string, now time.Time) (TimeAnchor, bool) {
	if m := monthDayYearPattern.FindStringSubmatch(q); m != nil {
		day, errD := strconv.Atoi(m[2])
		year, errY := strconv.Atoi(m[3])
		if errD == nil && errY == nil && day >= 1 && day <= 31 {
			target := time.Date(year, monthsByName[m[1]], day, 0, 0, 0, 0, time.UTC)
			return TimeAnchor{Days: daysAgo(target, now), Tolerance: 2}, true
		}
	}
	if m := monthYearPattern.FindStringSubmatch(q); m != nil {
		if year, err := strconv.Atoi(m[2]); err == nil {
			target := time.Date(year, monthsByName[m[1]], 15, 0, 0, 0, 0, time.UTC)
			return TimeAnchor{Days: daysAgo(target, now), Tolerance: 15}, true
		}
	}
	if m := monthDayPattern.FindStringSubmatch(q); m != nil {
		day, err := strconv.Atoi(m[2])
		if err == nil && day >= 1 && day <= 31 {
			target := recentOccurrence(monthsByName[m[1]], day, now)
			return TimeAnchor{Days: daysAgo(target, now), Tolerance: 2}, true
		}
	}
	if m := monthPattern.FindStringSubmatch(q); m != nil {
		target := recentOccurrence(monthsByName[m[1]], 15, now)
		return TimeAnchor{Days: daysAgo(target, now), Tolerance: 15}, true
	}
	if m := seasonPattern.FindStringSubmatch(q); m != nil {
		target := recentOccurrence(seasonMidMonth[m[1]], 15, now)
		return TimeAnchor{Days: daysAgo(target, now), Tolerance: 45}, true
	}
	if m := yearPattern.FindStringSubmatch(q); m != nil {
		year, err := strconv.Atoi(m[1])
		if err == nil {
			target := time.Date(year, time.July, 1, 0, 0, 0, 0, time.UTC)
			return TimeAnchor{Days: daysAgo(target, now), Tolerance: 120}, true
		}
	}
	return TimeAnchor{}, false
}

// recentOccurrence is the most recent month/day occurrence at or before now
// (this year when already past, otherwise last year).
func recentOccurrence(month time.Month, day int, now time.Time) time.Time {
	t := time.Date(now.Year(), month, day, 0, 0, 0, 0, time.UTC)
	if t.After(now) {
		t = t.AddDate(-1, 0, 0)
	}
	return t
}

func daysAgo(target, now time.Time) int {
	return int(now.Sub(target).Hours() / 24)
}

// RerankTemporal re-ranks with the composite weights, then — when ex resolves a
// relative-time reference in query — scales each score by a date-proximity
// factor toward (now - anchor) so a candidate dated near the referenced time can climb past a
// marginally-more-similar one. With no time reference (or no extractor) it is
// exactly RerankWith. boost <= 0 also degrades to plain composite re-rank.
func RerankTemporal(
	results []store.Scored, query string, now time.Time, w RerankWeights, ex AnchorExtractor, boost float64,
) []store.Scored {
	base := RerankWith(results, now, w)
	if ex == nil || boost <= 0 || now.IsZero() {
		return base
	}
	anchor, ok := ex.Anchor(query, now)
	if !ok {
		return base
	}
	target := now.AddDate(0, 0, -anchor.Days)

	type ranked struct {
		sc  store.Scored
		val float64
		pos int
	}
	out := make([]ranked, len(base))
	for i, r := range base {
		// A backdated fact's ValidFrom is when it was true — that, not the
		// row's write date, is what a time reference points at.
		date := r.Memory.CreatedAt
		if r.Memory.ValidFrom != nil {
			date = *r.Memory.ValidFrom
		}
		// Multiplicative, not additive: proximity amplifies a candidate's own
		// relevance (up to +boost×100%) rather than adding a flat bonus. A
		// date-near memory that is also relevant rises; a date-near but weakly
		// relevant one has little score to amplify, so it cannot leapfrog a
		// strong match on date alone — temporal targeting reorders among
		// comparable candidates instead of displacing relevance.
		val := r.Score * (1 + boost*temporalProximity(date, target, anchor.Tolerance))
		sc := r
		sc.Score = val
		out[i] = ranked{sc: sc, val: val, pos: i}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].val != out[j].val {
			return out[i].val > out[j].val
		}
		return out[i].pos < out[j].pos
	})
	reranked := make([]store.Scored, len(out))
	for i, r := range out {
		reranked[i] = r.sc
	}
	return reranked
}

// temporalProximity is 1 when date is within tol days of target, ramps linearly
// to 0 at 3*tol, and is 0 beyond that or when the date is unknown.
func temporalProximity(date, target time.Time, tol int) float64 {
	if date.IsZero() || tol <= 0 {
		return 0
	}
	delta := math.Abs(target.Sub(date).Hours() / 24)
	switch {
	case delta <= float64(tol):
		return 1
	case delta <= float64(3*tol):
		return 1 - (delta-float64(tol))/float64(2*tol)
	default:
		return 0
	}
}
