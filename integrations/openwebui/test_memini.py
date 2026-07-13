"""Unit tests for the memini Open WebUI integration helpers.

Run: cd integrations/openwebui && python -m unittest

Covers the pure helpers and the filter's recall/capture flow with a stubbed
HTTP call. Requires pydantic and aiohttp (both bundled with Open WebUI); skips
the filter-flow tests if they are not importable.
"""

import asyncio
import importlib.util
import json
import os
import tempfile
import unittest


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


HERE = os.path.dirname(os.path.abspath(__file__))

# Point XDG_CONFIG_HOME at an empty temp dir so a developer's real
# ~/.config/memini/overrides.json can't decide the namespace under test (both
# modules read it at call time). The override tests write their own.
os.environ["XDG_CONFIG_HOME"] = tempfile.mkdtemp(prefix="memini-test-config-")

try:
    flt = _load("memini_memory", os.path.join(HERE, "filter", "memini_memory.py"))
    tls = _load("memini_tools", os.path.join(HERE, "tools", "memini_tools.py"))
    _HAVE_DEPS = True
except ModuleNotFoundError:
    flt = None
    tls = None
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
                [
                    {"role": "user", "content": "x"},
                    {"role": "assistant", "content": "y", "error": "boom"},
                ]
            )
        )
        self.assertFalse(
            flt.last_assistant_failed([{"role": "assistant", "content": "ok"}])
        )
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
                return {
                    "results": [
                        {"memory": {"summary": "prior note", "tier": "episodic"}}
                    ]
                }
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
        self.assertNotIn("tier", memory_writes[0][1])

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

    def test_headers_carry_x_memini_home_when_configured(self):
        f = flt.Filter()
        f.valves.home = "personal/acme"
        headers = f._headers("proj", "")
        self.assertEqual(headers["X-Memini-Home"], "personal/acme")

    def test_headers_omit_x_memini_home_when_unset(self):
        f = flt.Filter()
        f.valves.home = ""
        headers = f._headers("proj", "")
        self.assertNotIn("X-Memini-Home", headers)


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class ToolsHeaders(unittest.TestCase):
    def test_headers_carry_x_memini_home_when_configured(self):
        t = tls.Tools()
        t.valves.home = "personal/acme"
        headers = t._headers("")
        self.assertEqual(headers["X-Memini-Home"], "personal/acme")

    def test_headers_omit_x_memini_home_when_unset(self):
        t = tls.Tools()
        t.valves.home = ""
        headers = t._headers("")
        self.assertNotIn("X-Memini-Home", headers)


class OverrideMixin:
    """Writes $XDG_CONFIG_HOME/memini/overrides.json and runs from a temp
    project dir — the override is keyed on the git toplevel (or, outside a repo,
    the cwd), which is the directory Open WebUI was launched in."""

    def _project(self):
        proj = os.path.realpath(tempfile.mkdtemp(prefix="memini-override-"))
        prev_cwd = os.getcwd()
        os.chdir(proj)
        self.addCleanup(lambda: os.chdir(prev_cwd))
        return proj

    def _write_overrides(self, overrides, raw=None):
        xdg = tempfile.mkdtemp(prefix="memini-test-config-")
        os.makedirs(os.path.join(xdg, "memini"))
        with open(os.path.join(xdg, "memini", "overrides.json"), "w") as f:
            f.write(raw if raw is not None else json.dumps({"version": 1, "overrides": overrides}))
        prev = os.environ["XDG_CONFIG_HOME"]
        os.environ["XDG_CONFIG_HOME"] = xdg
        self.addCleanup(lambda: os.environ.__setitem__("XDG_CONFIG_HOME", prev))


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class FilterOverride(OverrideMixin, unittest.TestCase):
    def test_override_beats_the_namespace_valve(self):
        proj = self._project()
        self._write_overrides({proj: {"namespace": "acme/api", "setAt": "2026-07-12T20:30:00Z"}})
        f = flt.Filter()
        f.valves.namespace = "openwebui"
        self.assertEqual(f._resolve_namespace(), ("acme/api", "override"))
        # A hierarchical override keeps its "/" — flattened to acme-api it would
        # write to a namespace no other integration reads.
        self.assertEqual(f._namespace(None), "acme/api")

    def test_scope_by_user_still_isolates_under_an_override(self):
        # The per-user suffix isolates *who*, not *what*: an override must not
        # collapse every user of a shared server into one namespace.
        proj = self._project()
        self._write_overrides({proj: {"namespace": "acme/api", "setAt": ""}})
        f = flt.Filter()
        f.valves.scope_by_user = True
        self.assertEqual(f._namespace({"id": "alice"}), "acme/api-alice")

    def test_malformed_or_absent_file_degrades_to_the_valve(self):
        self._project()
        f = flt.Filter()
        f.valves.namespace = "team"
        # Absent (the module-level XDG temp dir holds no overrides.json).
        self.assertEqual(f._namespace(None), "team")
        # Hand-edited into invalid JSON: degrade, never raise into Open WebUI.
        self._write_overrides(None, raw="{ not json")
        self.assertIsNone(flt.read_override(os.getcwd()))
        self.assertEqual(f._namespace(None), "team")

    def test_wrong_shape_and_blank_namespace_are_not_overrides(self):
        proj = self._project()
        self._write_overrides(None, raw=json.dumps({"version": 1, "overrides": []}))
        self.assertIsNone(flt.read_override(proj))
        self._write_overrides({proj: {"namespace": "  "}})
        self.assertIsNone(flt.read_override(proj))

    def test_override_key_uses_the_git_toplevel(self):
        # An override set at the top of a repo applies from a subdirectory too.
        import subprocess

        repo = os.path.realpath(tempfile.mkdtemp(prefix="memini-override-repo-"))
        subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
        nested = os.path.join(repo, "services", "api")
        os.makedirs(nested)
        self.assertEqual(flt.override_key(nested), repo)

    def test_header_safe_drops_control_characters_but_keeps_slashes(self):
        # A CR/LF in the value would split the X-Memini-Namespace header.
        self.assertEqual(flt.header_safe("  acme/api  "), "acme/api")
        self.assertEqual(flt.header_safe("acme\r\nX-Evil: 1"), "acmeX-Evil: 1")


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class ToolsOverrideAndStatus(OverrideMixin, unittest.TestCase):
    def test_headers_carry_the_override_namespace(self):
        proj = self._project()
        self._write_overrides({proj: {"namespace": "acme/api", "setAt": ""}})
        t = tls.Tools()
        t.valves.namespace = "openwebui"
        self.assertEqual(t._headers("")["X-Memini-Namespace"], "acme/api")

    def test_redact_secret_fingerprints_and_elides(self):
        self.assertEqual(tls.redact_secret("sk-0123456789abcd4f2a"), "sk-…4f2a")
        self.assertEqual(tls.redact_secret("short"), "***")
        self.assertEqual(tls.redact_secret(""), "")

    def test_status_reports_provenance_and_redacts_the_token(self):
        proj = self._project()
        self._write_overrides({proj: {"namespace": "acme/api", "setAt": "2026-07-12T20:30:00Z"}})
        os.environ["MEMINI_API_KEY"] = "sk-0123456789abcd4f2a"
        self.addCleanup(lambda: os.environ.pop("MEMINI_API_KEY", None))
        t = tls.Tools()
        t.valves.namespace = "openwebui"
        t.valves.base_url = "http://memini.example.com"
        out = asyncio.run(t.memini_status())
        self.assertIn("acme/api", out)
        self.assertIn("<- override", out)
        # What the override is masking, which a bare value dump would not show.
        self.assertIn("without the override", out)
        self.assertIn("openwebui", out)
        self.assertIn("override-active", out)
        self.assertIn("plaintext-bearer", out)
        # The secret is fingerprinted, never printed.
        self.assertNotIn("0123456789", out)
        self.assertIn("sk-…4f2a", out)

    def test_status_without_an_override_reports_the_valve(self):
        self._project()
        t = tls.Tools()
        t.valves.namespace = "team"
        out = asyncio.run(t.memini_status())
        self.assertIn("<- valve", out)
        self.assertNotIn("override-active", out)


if __name__ == "__main__":
    unittest.main()
