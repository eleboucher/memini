"""Unit tests for the memini Open WebUI integration helpers.

Run: cd integrations/openwebui && python -m unittest

Covers the pure helpers, the filter's recall/capture flow with a stubbed HTTP
call, and the POST /v1/handshake wire contract (namespace precedence,
fail-soft, memoization) with a stubbed aiohttp session. Requires pydantic and
aiohttp (both bundled with Open WebUI); skips the flow/handshake tests if they
are not importable.
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

# Point XDG_CONFIG_HOME at an empty temp dir -- harmless now that no file lives
# there, kept for parity with the other integrations' test setup.
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

    def test_header_safe_drops_control_characters_but_keeps_slashes(self):
        # A CR/LF in the value would split the X-Memini-Namespace header (or
        # inject a bogus field into the handshake's JSON body).
        self.assertEqual(flt.header_safe("  acme/api  "), "acme/api")
        self.assertEqual(flt.header_safe("acme\r\nX-Evil: 1"), "acmeX-Evil: 1")

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


async def _no_handshake():
    """A _handshake stub that always fail-softs to None, matching what a
    real handshake does against an unreachable server -- used so tests that
    only care about the recall/capture flow (not namespace resolution) never
    make a real network call."""
    return None


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class FilterFlow(unittest.TestCase):
    def _filter(self, captured_calls):
        f = flt.Filter()
        # Hermetic: _namespace()/_resolve_namespace() still runs for real (it's
        # not stubbed), but its handshake leg is -- these tests care about the
        # recall/capture flow, not namespace resolution, and must never touch
        # the network.
        f._handshake = _no_handshake

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
        # No handshake configured -> the namespace valve default.
        self.assertEqual(calls[0][2], "openwebui")

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
        f._handshake = _no_handshake
        f.valves.scope_by_user = True
        f.valves.namespace = "team"
        self.assertEqual(asyncio.run(f._namespace({"id": "alice"})), "team-alice")
        self.assertEqual(asyncio.run(f._namespace(None)), "team")

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
        t._handshake = _no_handshake
        t.valves.home = "personal/acme"
        headers = asyncio.run(t._headers(""))
        self.assertEqual(headers["X-Memini-Home"], "personal/acme")

    def test_headers_omit_x_memini_home_when_unset(self):
        t = tls.Tools()
        t._handshake = _no_handshake
        t.valves.home = ""
        headers = asyncio.run(t._headers(""))
        self.assertNotIn("X-Memini-Home", headers)


# --- Handshake (POST /v1/handshake wire contract) -------------------------
#
# flt and tls each `import aiohttp` themselves, but both resolve to the SAME
# cached module in sys.modules -- patching flt.aiohttp.ClientSession affects
# both. _AiohttpSessionPatchMixin always patches through flt for that reason,
# and every test restores the real ClientSession via addCleanup.


class _FakeResponse:
    def __init__(self, status, payload):
        self.status = status
        self._payload = payload

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc, tb):
        return False

    async def json(self):
        return self._payload

    async def text(self):
        return json.dumps(self._payload)


class _FakeSession:
    def __init__(self, responder):
        self._responder = responder

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc, tb):
        return False

    def post(self, url, json=None, headers=None):  # noqa: A002 - matches aiohttp's own signature
        return self._responder(url, json, headers)

    def delete(self, url, headers=None):
        return self._responder(url, None, headers)


def _session_factory(responder):
    def _make_session(*_a, **_k):
        return _FakeSession(responder)

    return _make_session


def _session_returning(status, payload):
    return _session_factory(lambda url, body, headers: _FakeResponse(status, payload))


def _session_raising(exc):
    def _make_session(*_a, **_k):
        raise exc

    return _make_session


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class _AiohttpSessionPatchMixin:
    def _patch_session(self, factory):
        real = flt.aiohttp.ClientSession
        flt.aiohttp.ClientSession = factory
        self.addCleanup(lambda: setattr(flt.aiohttp, "ClientSession", real))


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class FilterHandshakeTest(_AiohttpSessionPatchMixin, unittest.TestCase):
    def test_facts_carries_the_valve_as_declared_namespace(self):
        f = flt.Filter()
        f.valves.namespace = "acme/api"
        facts = f._facts()
        self.assertEqual(facts["declared_namespace"], "acme/api")
        self.assertTrue(facts["cwd_basename"])

    def test_facts_omits_declared_namespace_when_the_valve_is_blank(self):
        f = flt.Filter()
        f.valves.namespace = "   "
        facts = f._facts()
        self.assertNotIn("declared_namespace", facts)

    def test_resolve_namespace_uses_the_handshakes_namespace_when_present(self):
        self._patch_session(
            _session_returning(200, {"namespace": "acme/widget", "namespace_source": "declared"})
        )
        f = flt.Filter()
        f.valves.namespace = "openwebui"
        ns, source = asyncio.run(f._resolve_namespace())
        self.assertEqual(ns, "acme/widget")
        self.assertEqual(source, "server:declared")

    def test_resolve_namespace_falls_back_to_the_valve_on_any_handshake_failure(self):
        self._patch_session(_session_raising(RuntimeError("boom")))
        f = flt.Filter()
        f.valves.namespace = "team"
        ns, source = asyncio.run(f._resolve_namespace())
        self.assertEqual(ns, "team")
        self.assertEqual(source, "valve")

    def test_resolve_namespace_falls_back_to_the_valve_on_a_non_2xx(self):
        self._patch_session(_session_returning(500, {}))
        f = flt.Filter()
        f.valves.namespace = "team"
        ns, source = asyncio.run(f._resolve_namespace())
        self.assertEqual(ns, "team")
        self.assertEqual(source, "valve")

    def test_per_user_suffix_applies_after_the_handshakes_namespace(self):
        # The per-user suffix isolates *who*, not *what*: it must not be
        # skipped just because the server resolved a namespace.
        self._patch_session(
            _session_returning(200, {"namespace": "acme/widget", "namespace_source": "declared"})
        )
        f = flt.Filter()
        f.valves.scope_by_user = True
        ns = asyncio.run(f._namespace({"id": "alice"}))
        self.assertEqual(ns, "acme/widget-alice")

    def test_handshake_is_memoized_within_the_ttl(self):
        calls = []

        def responder(url, body, headers):
            calls.append(url)
            return _FakeResponse(200, {"namespace": "ns", "namespace_source": "declared"})

        self._patch_session(_session_factory(responder))
        f = flt.Filter()
        asyncio.run(f._resolve_namespace())
        asyncio.run(f._resolve_namespace())
        self.assertEqual(len(calls), 1, "the second call must reuse the memoized handshake")

    def test_handshake_refetches_after_the_ttl_expires(self):
        calls = []

        def responder(url, body, headers):
            calls.append(url)
            return _FakeResponse(200, {"namespace": "ns", "namespace_source": "declared"})

        self._patch_session(_session_factory(responder))
        f = flt.Filter()
        real_monotonic = flt.time.monotonic
        clock = {"t": 0.0}
        flt.time.monotonic = lambda: clock["t"]
        self.addCleanup(lambda: setattr(flt.time, "monotonic", real_monotonic))

        asyncio.run(f._resolve_namespace())
        self.assertEqual(len(calls), 1)
        clock["t"] = flt.HANDSHAKE_TTL_S + 1
        asyncio.run(f._resolve_namespace())
        self.assertEqual(len(calls), 2, "must refetch once the TTL has expired")

    def test_handshake_carries_the_home_header_and_client_identification(self):
        captured = []

        def responder(url, body, headers):
            captured.append((url, body, headers))
            return _FakeResponse(200, {})

        self._patch_session(_session_factory(responder))
        f = flt.Filter()
        f.valves.home = "personal/acme"
        asyncio.run(f._handshake())
        url, body, headers = captured[0]
        self.assertTrue(url.endswith("/v1/handshake"))
        self.assertEqual(headers["X-Memini-Home"], "personal/acme")
        self.assertEqual(body["client"]["name"], "openwebui-memini-filter")
        self.assertIn("version", body["client"])


@unittest.skipUnless(_HAVE_DEPS, "pydantic/aiohttp not installed")
class ToolsHandshakeTest(_AiohttpSessionPatchMixin, unittest.TestCase):
    def test_facts_carries_the_valve_as_declared_namespace(self):
        t = tls.Tools()
        t.valves.namespace = "acme/api"
        facts = t._facts()
        self.assertEqual(facts["declared_namespace"], "acme/api")

    def test_resolve_namespace_uses_the_handshakes_namespace_when_present(self):
        self._patch_session(
            _session_returning(200, {"namespace": "acme/widget", "namespace_source": "declared"})
        )
        t = tls.Tools()
        t.valves.namespace = "openwebui"
        ns, source = asyncio.run(t._resolve_namespace())
        self.assertEqual(ns, "acme/widget")
        self.assertEqual(source, "server:declared")

    def test_resolve_namespace_falls_back_to_the_valve_on_any_handshake_failure(self):
        self._patch_session(_session_raising(RuntimeError("boom")))
        t = tls.Tools()
        t.valves.namespace = "team"
        ns, source = asyncio.run(t._resolve_namespace())
        self.assertEqual(ns, "team")
        self.assertEqual(source, "valve")

    def test_headers_use_the_handshakes_namespace(self):
        self._patch_session(
            _session_returning(200, {"namespace": "acme/widget", "namespace_source": "declared"})
        )
        t = tls.Tools()
        headers = asyncio.run(t._headers(""))
        self.assertEqual(headers["X-Memini-Namespace"], "acme/widget")

    def test_redact_secret_fingerprints_and_elides(self):
        self.assertEqual(tls.redact_secret("sk-0123456789abcd4f2a"), "sk-…4f2a")
        self.assertEqual(tls.redact_secret("short"), "***")
        self.assertEqual(tls.redact_secret(""), "")

    def test_status_reports_the_server_resolved_namespace_and_redacts_the_token(self):
        self._patch_session(
            _session_returning(200, {"namespace": "acme/widget", "namespace_source": "declared"})
        )
        os.environ["MEMINI_API_KEY"] = "sk-0123456789abcd4f2a"
        self.addCleanup(lambda: os.environ.pop("MEMINI_API_KEY", None))
        t = tls.Tools()
        t.valves.namespace = "openwebui"
        t.valves.base_url = "http://memini.example.com"
        out = asyncio.run(t.memini_status())
        self.assertIn("acme/widget", out)
        self.assertIn("<- server:declared", out)
        # What the server resolved, versus what the valve alone says --
        # analogous to the other integrations' "without the pin" line.
        self.assertIn("the namespace valve says", out)
        self.assertIn("plaintext-bearer", out)
        # The secret is fingerprinted, never printed.
        self.assertNotIn("0123456789", out)
        self.assertIn("sk-…4f2a", out)

    def test_status_falls_back_to_the_valve_when_the_handshake_fails(self):
        self._patch_session(_session_raising(RuntimeError("boom")))
        t = tls.Tools()
        t.valves.namespace = "team"
        out = asyncio.run(t.memini_status())
        self.assertIn("<- valve", out)
        self.assertNotIn("the namespace valve says", out)


if __name__ == "__main__":
    unittest.main()
