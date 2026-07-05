"""Unit tests for the memini Open WebUI integration helpers.

Run: cd integrations/openwebui && python -m unittest

Covers the pure helpers and the filter's recall/capture flow with a stubbed
HTTP call. Requires pydantic and aiohttp (both bundled with Open WebUI); skips
the filter-flow tests if they are not importable.
"""

import asyncio
import importlib.util
import os
import unittest


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


HERE = os.path.dirname(os.path.abspath(__file__))

try:
    flt = _load("memini_memory", os.path.join(HERE, "filter", "memini_memory.py"))
    _HAVE_DEPS = True
except ModuleNotFoundError:
    flt = None
    _HAVE_DEPS = False


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class Helpers(unittest.TestCase):
    def test_sanitize_namespace(self):
        self.assertEqual(flt.sanitize_namespace("My Project!"), "My-Project")
        self.assertEqual(flt.sanitize_namespace("a__b.c-d"), "a__b.c-d")
        self.assertEqual(flt.sanitize_namespace("  --x--  "), "x")

    def test_last_assistant_failed(self):
        self.assertTrue(
            flt.last_assistant_failed(
                [{"role": "user", "content": "x"}, {"role": "assistant", "content": "y", "error": "boom"}]
            )
        )
        self.assertFalse(flt.last_assistant_failed([{"role": "assistant", "content": "ok"}]))
        self.assertFalse(flt.last_assistant_failed(None))

    def test_extract_last_user_string(self):
        messages = [
            {"role": "user", "content": "first"},
            {"role": "assistant", "content": "reply"},
            {"role": "user", "content": "  latest  "},
        ]
        self.assertEqual(flt.extract_last_user(messages), "latest")

    def test_extract_last_user_multimodal(self):
        messages = [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "hello"},
                    {"type": "image_url", "image_url": {"url": "x"}},
                    {"type": "text", "text": "world"},
                ],
            }
        ]
        self.assertEqual(flt.extract_last_user(messages), "hello\nworld")

    def test_extract_last_turn(self):
        messages = [
            {"role": "system", "content": "sys"},
            {"role": "user", "content": "q"},
            {"role": "assistant", "content": "a"},
        ]
        self.assertEqual(flt.extract_last_turn(messages), ("q", "a"))

    def test_format_results(self):
        results = [
            {"memory": {"summary": "fact one", "tier": "semantic"}},
            {"memory": {"content": "fact two"}},
        ]
        out = flt.format_results(results, 5)
        self.assertEqual(out, "- fact one\n- fact two")
        self.assertEqual(flt.format_results([], 5), "")

    def test_uses_plaintext_bearer(self):
        self.assertFalse(flt.uses_plaintext_bearer("http://localhost:8080", "k"))
        self.assertFalse(flt.uses_plaintext_bearer("https://memini.example", "k"))
        self.assertFalse(flt.uses_plaintext_bearer("http://memini.example", ""))
        self.assertTrue(flt.uses_plaintext_bearer("http://memini.example", "k"))


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class FilterFlow(unittest.TestCase):
    def _filter(self, captured_calls):
        f = flt.Filter()

        async def fake_post(path, payload, namespace):
            captured_calls.append((path, payload, namespace))
            if path == "/v1/search":
                return {"results": [{"memory": {"summary": "prior note", "tier": "episodic"}}]}
            return {"id": "mem_1"}

        f._post_json = fake_post
        return f

    def test_inlet_injects_memory(self):
        calls = []
        f = self._filter(calls)
        body = {"messages": [{"role": "user", "content": "what did we decide?"}]}
        out = asyncio.run(f.inlet(body))
        self.assertEqual(out["messages"][0]["role"], "system")
        self.assertIn("prior note", out["messages"][0]["content"])
        self.assertEqual(out["messages"][1]["role"], "user")
        self.assertEqual(calls[0][0], "/v1/search")
        # The default recall_limit (3) must reach the search request body.
        self.assertEqual(calls[0][1]["limit"], 3)

    def test_inlet_excludes_own_chat(self):
        # On inlet the chat id arrives via injected __chat_id__/__metadata__, not
        # body; it must become exclude_metadata.chat_id so own turns aren't echoed.
        calls = []
        f = self._filter(calls)
        body = {"messages": [{"role": "user", "content": "q"}]}
        asyncio.run(f.inlet(body, __chat_id__="c1"))
        search = next(c for c in calls if c[0] == "/v1/search")
        self.assertEqual(search[1]["exclude_metadata"], {"chat_id": "c1"})

    def test_inlet_excludes_own_chat_via_metadata(self):
        # __metadata__["chat_id"] is the other injection path Open WebUI uses.
        calls = []
        f = self._filter(calls)
        body = {"messages": [{"role": "user", "content": "q"}]}
        asyncio.run(f.inlet(body, __metadata__={"chat_id": "c2"}))
        search = next(c for c in calls if c[0] == "/v1/search")
        self.assertEqual(search[1]["exclude_metadata"], {"chat_id": "c2"})

    def test_inlet_without_chat_id_stays_unscoped(self):
        calls = []
        f = self._filter(calls)
        asyncio.run(f.inlet({"messages": [{"role": "user", "content": "q"}]}))
        search = next(c for c in calls if c[0] == "/v1/search")
        self.assertNotIn("exclude_metadata", search[1])

    def test_inlet_disabled(self):
        calls = []
        f = self._filter(calls)
        f.valves.recall = False
        body = {"messages": [{"role": "user", "content": "hi"}]}
        asyncio.run(f.inlet(body))
        self.assertEqual(calls, [])

    def test_outlet_captures_once(self):
        calls = []
        f = self._filter(calls)
        body = {
            "chat_id": "c1",
            "messages": [
                {"role": "user", "content": "q"},
                {"role": "assistant", "content": "a"},
            ],
        }
        asyncio.run(f.outlet(body))
        asyncio.run(f.outlet(body))  # dedup: second call is a no-op
        memory_writes = [c for c in calls if c[0] == "/v1/memories"]
        self.assertEqual(len(memory_writes), 1)
        self.assertEqual(memory_writes[0][1]["tier"], "episodic")

    def test_outlet_without_chat_id_skips_capture(self):
        # A capture without a chat id can never be excluded by inlet recall,
        # so it would echo the chat's own turns back forever.
        calls = []
        f = self._filter(calls)
        body = {
            "messages": [
                {"role": "user", "content": "q"},
                {"role": "assistant", "content": "a"},
            ],
        }
        asyncio.run(f.outlet(body))
        self.assertEqual([c for c in calls if c[0] == "/v1/memories"], [])

    def test_scope_by_user(self):
        f = flt.Filter()
        f.valves.scope_by_user = True
        f.valves.namespace = "team"
        self.assertEqual(f._namespace({"id": "alice"}), "team-alice")
        self.assertEqual(f._namespace(None), "team")


if __name__ == "__main__":
    unittest.main()
