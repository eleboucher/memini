package render

import "strings"

// ItemOverheadTokens is the per-item overhead a server-enforced token budget
// charges on top of an item's shipped text: the render skeleton the client
// wraps around it (bullet prefix, score, labels, the [m:id] handle, a section
// header's amortized share). The server cannot see the client's actual
// template, so this is a deliberate small constant — an estimate, like
// ApproxTokens itself — shared by the /v1/search and briefing budgets.
const ItemOverheadTokens = 10

// ApproxTokens is the server half of the shared token estimator: a cheap
// ~0.75-tokens-per-word estimate, NOT a tokenizer. It replicates the plugin
// client's approxTokens (plugin/scripts/_shared.mjs) exactly — 0 for empty
// text, else max(1, ceil(words*4/3)) over the whitespace-split word count,
// where whitespace-only text (truthy for the client) floors to 1 — so a
// budget means the same thing on both ends of the wire. Documented as an
// estimate: budgets built on it bound token spend approximately, they do not
// guarantee an exact token count.
func ApproxTokens(text string) int {
	if text == "" {
		return 0
	}
	words := len(strings.Fields(text))
	if t := (words*4 + 2) / 3; t > 1 { // integer ceil(words*4/3)
		return t
	}
	return 1
}
