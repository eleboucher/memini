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
    def test_three_tools(self):
        names = [t["name"] for t in memini.MeminiMemoryProvider().get_tool_schemas()]
        self.assertEqual(sorted(names), ["memory_list", "memory_recall", "memory_remember"])


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


class IsAvailableTest(unittest.TestCase):
    def test_valid_url_is_available(self):
        os.environ["MEMINI_URL"] = "http://localhost:8080"
        self.addCleanup(lambda: os.environ.pop("MEMINI_URL", None))
        self.assertTrue(memini.MeminiMemoryProvider().is_available())

    def test_invalid_url_is_unavailable(self):
        os.environ["MEMINI_URL"] = "not-a-url"
        self.addCleanup(lambda: os.environ.pop("MEMINI_URL", None))
        self.assertFalse(memini.MeminiMemoryProvider().is_available())


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
