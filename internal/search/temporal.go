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
// DefaultTemporalBoost is the additive bonus a perfectly on-target candidate
// gets on the [0,1] composite score; it ramps to 0 by 3x the tolerance.
const DefaultTemporalBoost = 0.40

// TimeAnchor is a resolved relative-time reference: an answer is expected
// roughly Days before the query's "now", give or take Tolerance days.
type TimeAnchor struct {
	Days      int
	Tolerance int
}

// AnchorExtractor resolves a query's relative-time reference, if any. The regex
// implementation is the no-LLM default; an LLM extractor (looser phrasing like
// "a couple weeks before my trip") can plug in via the same interface.
type AnchorExtractor interface {
	Anchor(query string) (TimeAnchor, bool)
}

// RegexAnchorExtractor matches common English relative-time phrases, firing
// only on templated expressions.
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

// Anchor implements AnchorExtractor.
func (RegexAnchorExtractor) Anchor(query string) (TimeAnchor, bool) {
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
	return TimeAnchor{}, false
}

// RerankTemporal re-ranks with the composite weights, then — when ex resolves a
// relative-time reference in query — adds a date-proximity bonus toward
// (now - anchor) so a candidate dated near the referenced time can climb past a
// marginally-more-similar one. With no time reference (or no extractor) it is
// exactly RerankWith. boost <= 0 also degrades to plain composite re-rank.
func RerankTemporal(
	results []store.Scored, query string, now time.Time, w RerankWeights, ex AnchorExtractor, boost float64,
) []store.Scored {
	base := RerankWith(results, now, w)
	if ex == nil || boost <= 0 || now.IsZero() {
		return base
	}
	anchor, ok := ex.Anchor(query)
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
		val := r.Score + boost*temporalProximity(r.Memory.CreatedAt, target, anchor.Tolerance)
		out[i] = ranked{sc: store.Scored{Memory: r.Memory, Score: val}, val: val, pos: i}
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
