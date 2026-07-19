# Golden-vector runner for the hermes memory provider's copies of the shared
# injection-enforcement core (packages/memini-client/src/enforce/).
#
# hermes ships stdlib-only, so it carries COPIES of the core functions rather
# than importing @memini/client. The copies are only worth having if they ARE
# the same functions; packages/memini-client/vectors/enforcement.json (the
# pinned contract every port is verified against) is what enforces that. Add
# cases to the vector file, not here — a case added for any client is then
# enforced on all of them.
#
# Run: python3 -m unittest (from this dir); pytest collects it too. Like
# test_memini.py, this file is a sibling of the memini/ package so the release
# mirror does not ship it.

import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import memini  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
VECTORS_PATH = os.path.join(
    HERE, "..", "..", "..", "packages", "memini-client", "vectors", "enforcement.json"
)

with open(VECTORS_PATH, encoding="utf-8") as _fh:
    VECTORS = json.load(_fh)

# Adapter table: vector `fn` name -> the hermes callable, mapping the vector's
# camelCase input fields onto hermes' snake_case keyword signature. Every
# adapter calls the REAL module function — no reimplementation.
ADAPTERS = {
    "injectedSuppressed": lambda i: memini._injected_suppressed(
        i["entry"],
        i["identity"],
        now=i["opts"]["now"],
        counter=i["opts"]["counter"],
        cooldown_ms=i["opts"]["cooldownMs"],
        cooldown_prompts=i["opts"]["cooldownPrompts"],
    ),
    "injectedIdentity": lambda i: memini._injected_identity(i["m"]),
    "approxTokens": lambda i: memini._approx_tokens(i["text"]),
}

# Core functions hermes deliberately does NOT implement, with why. A new fn
# appearing in the vector file lands in NEITHER table and fails the coverage
# test below, forcing an explicit decision (adapt it or skip it) instead of
# silently testing nothing.
SKIPPED_FNS = {
    "cooldownIds": "hermes keeps _injected_ids + _prefetch_n on the provider, not a "
    "{n, ids} state object; the equivalent id-only listing is exercised through "
    "_recall_body in test_memini.py.",
    "mergeInjectedStates": "hermes' state is in-memory per provider instance; there is "
    "no persisted state file and so no concurrent read-merge-write to reconcile.",
    "fitByTokens": "hermes' _fit_by_tokens deliberately keeps whole bullets (drops, "
    "never partially truncates an item) and returns (kept, dropped) — a simplified "
    "variant, not the core function.",
    "formatRecallHit": "hermes renders its own bullet contract (_format_lines: "
    "summary-first, 300-char cap, env-driven labels), not the core's hit renderer.",
    "pretoolFingerprint": "hermes has no per-file pre-tool-use surface.",
    "briefingContentHash": "hermes has no cache-stable briefing injection guard; the "
    "briefing is a model-invoked tool, not a hook injection.",
}


class VectorCoverageTest(unittest.TestCase):
    def test_every_vector_fn_is_adapted_or_deliberately_skipped(self):
        fns = {v["fn"] for v in VECTORS}
        covered = set(ADAPTERS) | set(SKIPPED_FNS)
        self.assertEqual(
            fns,
            covered,
            "a vector fn is neither adapted nor in SKIPPED_FNS (or a stale entry "
            "lingers): decide explicitly before the suite can go green",
        )

    def test_no_fn_is_both_adapted_and_skipped(self):
        self.assertEqual(set(ADAPTERS) & set(SKIPPED_FNS), set())

    def test_the_skip_list_only_names_real_vector_fns(self):
        fns = {v["fn"] for v in VECTORS}
        self.assertLessEqual(set(SKIPPED_FNS), fns)


class EnforcementVectorsTest(unittest.TestCase):
    """Replays every vector case for the functions hermes implements."""

    def test_vectors(self):
        ran = 0
        for case in VECTORS:
            adapter = ADAPTERS.get(case["fn"])
            if adapter is None:
                continue
            with self.subTest(f"{case['fn']}/{case['name']}"):
                self.assertEqual(adapter(case["input"]), case["expected"])
                ran += 1
        self.assertGreater(ran, 0, "no vector case ran — the adapter table is empty?")


if __name__ == "__main__":
    unittest.main()
