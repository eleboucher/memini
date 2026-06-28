# Tests for the memini Hermes memory provider. Pure stdlib (unittest), no
# Hermes install needed — the plugin falls back to an inline ABC when
# `agent.memory_provider` is absent. Run: python3 -m unittest (from this dir).
#
# This file is a sibling of the memini/ package so the release mirror (which
# copies memini/. into eleboucher/memini-hermes) does not ship it.
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import memini  # noqa: E402

# A developer shell may export the real memini config; clear it so each test's
# explicit env is authoritative (canonical MEMINI_BASE_URL would otherwise win
# over a test that sets only MEMINI_URL).
for _v in ("MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN"):
    os.environ.pop(_v, None)


def make_provider(call_stub):
    """A provider initialized against a dummy URL with its network _call stubbed."""
    os.environ["MEMINI_URL"] = "http://localhost:8080"
    os.environ.pop("MEMINI_NAMESPACE", None)
    p = memini.MeminiMemoryProvider()
    p.initialize("sess-1")
    p._call = call_stub
    return p


class ListPathTest(unittest.TestCase):
    def test_builds_repeatable_and_escaped_params(self):
        path = memini._list_path(
            {"tiers": ["procedural"], "tags": ["auth"], "metadata": {"category": "bug_fixes"}, "limit": 20}
        )
        self.assertEqual(path, "/v1/memories?tier=procedural&tag=auth&meta=category%3Dbug_fixes&limit=20")

    def test_empty(self):
        self.assertEqual(memini._list_path({}), "/v1/memories")

    def test_nonpositive_limit_omitted(self):
        self.assertEqual(memini._list_path({"limit": 0}), "/v1/memories")


class ToolSchemasTest(unittest.TestCase):
    def test_tools(self):
        names = [t["name"] for t in memini.MeminiMemoryProvider().get_tool_schemas()]
        self.assertEqual(sorted(names), ["memory_forget", "memory_list", "memory_recall", "memory_remember"])


class MemoryForgetToolTest(unittest.TestCase):
    def test_forget_deletes_by_id(self):
        calls = []

        def stub(path, body, method="POST"):
            calls.append((path, method))
            return {}

        out = make_provider(stub).handle_tool_call("memory_forget", {"id": "mem 1/x"})
        self.assertEqual(calls, [("/v1/memories/mem%201%2Fx", "DELETE")])
        self.assertEqual(json.loads(out), {"forgotten": True})

    def test_forget_without_id_makes_no_request(self):
        calls = []
        out = make_provider(lambda *a, **k: calls.append(a) or {}).handle_tool_call("memory_forget", {})
        self.assertEqual(calls, [])
        self.assertEqual(json.loads(out)["forgotten"], False)


class OnPreCompressTest(unittest.TestCase):
    def test_returns_block_and_does_not_mutate(self):
        # Regression guard: Hermes injects the RETURN value into the compaction
        # prompt and ignores in-place edits to `messages`. The provider must
        # return the context string, not insert it into the list.
        def stub(path, body, method="POST"):
            self.assertEqual(path, "/v1/search")
            return {"results": [{"memory": {"content": "did X last time"}, "score": 0.9}]}

        p = make_provider(stub)
        msgs = [{"role": "user", "content": "what next?"}]
        out = p.on_pre_compress(msgs)
        self.assertIsInstance(out, str)
        self.assertIn("did X last time", out)
        self.assertEqual(len(msgs), 1, "on_pre_compress must not mutate the messages list")

    def test_empty_when_no_hits(self):
        p = make_provider(lambda *a, **k: None)
        self.assertEqual(p.on_pre_compress([{"role": "user", "content": "q"}]), "")

    def test_excludes_own_session(self):
        # Recall must drop this session's own captured turns (they're still in
        # the live transcript) by passing exclude_metadata.session_id.
        captured = {}

        def stub(path, body, method="POST"):
            captured["body"] = body
            return {"results": []}

        make_provider(stub).on_pre_compress([{"role": "user", "content": "q"}])
        self.assertEqual(captured["body"]["exclude_metadata"], {"session_id": "sess-1"})


class PrefetchTest(unittest.TestCase):
    def test_excludes_own_session(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["path"], captured["body"] = path, body
            return {"results": [{"memory": {"summary": "prior note"}}]}

        out = make_provider(stub).prefetch("what did we decide?")
        self.assertEqual(captured["path"], "/v1/search")
        self.assertEqual(captured["body"]["exclude_metadata"], {"session_id": "sess-1"})
        self.assertIn("prior note", out)


class OnMemoryWriteTest(unittest.TestCase):
    def _provider(self, captured):
        p = make_provider(lambda *a, **k: None)
        # on_memory_write writes via the background helper; capture it directly.
        p._call_bg = lambda path, body: captured.update(path=path, body=body)
        return p

    def test_mirrors_add_and_replace(self):
        # Hermes emits action ∈ {add, replace, remove}; add and replace mirror.
        for action in ("add", "replace"):
            captured = {}
            self._provider(captured).on_memory_write(action, "memory", "a durable fact")
            self.assertEqual(captured["path"], "/v1/memories")
            self.assertEqual(captured["body"]["tier"], "semantic")
            self.assertEqual(captured["body"]["content"], "a durable fact")

    def test_ignores_remove_and_empty_content(self):
        captured = {}
        p = self._provider(captured)
        p.on_memory_write("remove", "memory", "gone")
        p.on_memory_write("add", "memory", "   ")
        self.assertEqual(captured, {})


class HandleToolCallTest(unittest.TestCase):
    def test_remember_maps_category_and_defaults_tier(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["path"], captured["body"] = path, body
            return {"id": "m1"}

        out = make_provider(stub).handle_tool_call(
            "memory_remember", {"content": "fact", "category": "bug_fixes", "tags": ["x"]}
        )
        self.assertEqual(captured["path"], "/v1/memories")
        self.assertEqual(
            captured["body"],
            {"content": "fact", "tier": "semantic", "tags": ["x"], "metadata": {"category": "bug_fixes"}},
        )
        self.assertEqual(json.loads(out), {"id": "m1", "success": True})

    def test_remember_rejects_unknown_tier(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["body"] = body
            return {"id": "m2"}

        make_provider(stub).handle_tool_call("memory_remember", {"content": "f", "tier": "bogus"})
        self.assertEqual(captured["body"]["tier"], "semantic")

    def test_list_issues_a_get(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["path"], captured["method"] = path, method
            return {"memories": [{"id": "m1", "tier": "procedural", "metadata": {"category": "bug_fixes"}}]}

        out = make_provider(stub).handle_tool_call(
            "memory_list", {"tiers": ["procedural"], "metadata": {"category": "bug_fixes"}}
        )
        self.assertEqual(captured["method"], "GET")
        self.assertTrue(captured["path"].startswith("/v1/memories?"))
        self.assertEqual(json.loads(out)["memories"][0]["metadata"]["category"], "bug_fixes")

    def test_unknown_tool_returns_error(self):
        out = make_provider(lambda *a, **k: None).handle_tool_call("nope", {})
        self.assertIn("error", json.loads(out))


class RecallShapingTest(unittest.TestCase):
    KNOBS = (
        "MEMINI_RECALL_LIMIT",
        "MEMINI_INJECT_RECALL_MIN_SCORE",
        "MEMINI_INJECT_RECALL_MAX_TOK",
        "MEMINI_INJECT_LABELS",
    )

    def setUp(self):
        for k in self.KNOBS:
            os.environ.pop(k, None)
        self.addCleanup(lambda: [os.environ.pop(k, None) for k in self.KNOBS])

    @staticmethod
    def _hits(*hits):
        def stub(path, body, method="POST"):
            stub.body = body
            return {"results": list(hits)}

        return stub

    def test_body_uses_configured_limit_and_min_score(self):
        os.environ["MEMINI_RECALL_LIMIT"] = "8"
        os.environ["MEMINI_INJECT_RECALL_MIN_SCORE"] = "0.3"
        stub = self._hits()
        make_provider(stub).prefetch("q")
        self.assertEqual(stub.body["limit"], 8)
        self.assertEqual(stub.body["min_score"], 0.3)

    def test_default_body_omits_min_score_and_defaults_limit_3(self):
        stub = self._hits()
        make_provider(stub).prefetch("q")
        self.assertEqual(stub.body["limit"], 3)
        self.assertNotIn("min_score", stub.body)

    def test_default_renders_plain_bullet(self):
        stub = self._hits({"memory": {"summary": "note A", "tier": "semantic"}, "score": 0.9})
        out = make_provider(stub).prefetch("q")
        self.assertIn("- note A", out)
        self.assertNotIn("[semantic]", out)

    def test_labels_render_tier_prefix(self):
        os.environ["MEMINI_INJECT_LABELS"] = "tier"
        stub = self._hits({"memory": {"summary": "note A", "tier": "semantic"}, "score": 0.9})
        out = make_provider(stub).prefetch("q")
        self.assertIn("- [semantic] note A", out)

    def test_client_side_min_score_filter_drops_low_hits(self):
        os.environ["MEMINI_INJECT_RECALL_MIN_SCORE"] = "0.5"
        stub = self._hits(
            {"memory": {"summary": "keep me"}, "score": 0.8},
            {"memory": {"summary": "drop me"}, "score": 0.2},
        )
        out = make_provider(stub).prefetch("q")
        self.assertIn("keep me", out)
        self.assertNotIn("drop me", out)

    def test_token_budget_truncates_tail_with_footer(self):
        # "- alpha beta gamma" ≈ 6 tokens, so a 6-token cap keeps only the first.
        os.environ["MEMINI_INJECT_RECALL_MAX_TOK"] = "6"
        stub = self._hits(
            {"memory": {"summary": "alpha beta gamma"}, "score": 0.9},
            {"memory": {"summary": "delta epsilon zeta"}, "score": 0.8},
        )
        out = make_provider(stub).prefetch("q")
        self.assertIn("alpha beta gamma", out)
        self.assertNotIn("delta", out)
        self.assertIn("truncated by token budget", out)


class IsAvailableTest(unittest.TestCase):
    def test_valid_url_is_available(self):
        os.environ["MEMINI_URL"] = "http://localhost:8080"
        self.addCleanup(lambda: os.environ.pop("MEMINI_URL", None))
        self.assertTrue(memini.MeminiMemoryProvider().is_available())

    def test_invalid_url_is_unavailable(self):
        os.environ["MEMINI_URL"] = "not-a-url"
        self.addCleanup(lambda: os.environ.pop("MEMINI_URL", None))
        self.assertFalse(memini.MeminiMemoryProvider().is_available())

    def test_base_url_canonical_takes_precedence_over_url_alias(self):
        os.environ["MEMINI_BASE_URL"] = "http://localhost:8080"
        os.environ["MEMINI_URL"] = "not-a-url"
        self.addCleanup(lambda: os.environ.pop("MEMINI_BASE_URL", None))
        self.addCleanup(lambda: os.environ.pop("MEMINI_URL", None))
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-base-url")
        self.assertEqual(p._base, "http://localhost:8080")

    def test_token_reads_api_key_then_token_alias(self):
        os.environ["MEMINI_TOKEN"] = "tok-alias"
        self.addCleanup(lambda: os.environ.pop("MEMINI_TOKEN", None))
        self.addCleanup(lambda: os.environ.pop("MEMINI_API_KEY", None))
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-token-alias")
        self.assertEqual(p._secret, "tok-alias")
        os.environ["MEMINI_API_KEY"] = "key-canonical"
        p2 = memini.MeminiMemoryProvider()
        p2.initialize("sess-token-canonical")
        self.assertEqual(p2._secret, "key-canonical")


class SaveConfigTest(unittest.TestCase):
    def test_does_not_persist_secret_to_disk(self):
        import tempfile

        p = memini.MeminiMemoryProvider()
        with tempfile.TemporaryDirectory() as home:
            p.save_config({"url": "http://localhost:8080", "namespace": "x", "secret": "tok-123"}, home)
            with open(os.path.join(home, "memini.json")) as f:
                saved = json.load(f)
        self.assertEqual(saved.get("url"), "http://localhost:8080")
        self.assertEqual(saved.get("namespace"), "x")
        self.assertNotIn("secret", saved, "bearer token must not be written to disk")


if __name__ == "__main__":
    unittest.main()
