package bench

import "math"

// McNemarExact returns the two-sided exact-binomial p-value for a paired
// comparison with discordant counts b and c: b questions where arm A is correct
// and arm B is wrong, c where B is correct and A is wrong (concordant pairs
// carry no information about which arm is better). Under H0 each discordant pair
// is a fair coin, so the count is Binomial(n=b+c, p=0.5); the two-sided p-value
// is the total probability of a split at least as lopsided as observed. This
// exact form is used instead of the chi-square approximation because the pilot's
// discordant counts are small (n≈45 questions), where the approximation is
// unreliable.
func McNemarExact(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1.0
	}
	k := min(b, c)
	// Two-sided: sum both tails of the symmetric Binomial(n, 0.5). By symmetry
	// that is 2× the lower tail P(X <= k); clamp at 1 for the near-even case.
	var tail float64
	for i := 0; i <= k; i++ {
		tail += binomCoeff(n, i)
	}
	p := 2 * tail * math.Pow(0.5, float64(n))
	return math.Min(1.0, p)
}

// binomCoeff returns C(n, k) as a float64, computed multiplicatively to avoid
// overflow on the factorials for the small n the bench produces.
func binomCoeff(n, k int) float64 {
	if k < 0 || k > n {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	c := 1.0
	for i := 0; i < k; i++ {
		c = c * float64(n-i) / float64(i+1)
	}
	return c
}
