# Tests for the memini Hermes memory provider. Pure stdlib (unittest), no
# Hermes install needed — the plugin falls back to an inline ABC when
# `agent.memory_provider` is absent. Run: python3 -m unittest (from this dir).
#
# This file is a sibling of the memini/ package so the release mirror (which
# copies memini/. into eleboucher/memini-hermes) does not ship it.
import json
import os
import subprocess
import sys
import tempfile
import unittest
from urllib.error import URLError

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import memini  # noqa: E402

# A developer shell may export the real memini config; clear it so each test's
# explicit env is authoritative.
for _v in ("MEMINI_BASE_URL", "MEMINI_API_KEY", "MEMINI_HOME"):
    os.environ.pop(_v, None)
# Point XDG_CONFIG_HOME at an empty temp dir -- harmless now that no file lives
# there, kept for parity with the other integrations' test setup.
os.environ["XDG_CONFIG_HOME"] = tempfile.mkdtemp(prefix="memini-test-config-")

# Hermetic: no test may make a real network call. initialize() now calls
# _cached_handshake (POST /v1/handshake) on every provider construction; stub
# urlopen at the module level so it fails fast and deterministically
# (fail-soft -> local derivation) instead of depending on there being no real
# memini listening on localhost:8080. Tests that need a specific handshake
# response swap memini.urlopen for their own duration (see HomeHeaderTest /
# HandshakeTest) and must restore it (addCleanup or try/finally).
def _no_network_urlopen(req, timeout=None):
    raise URLError("no network in tests")


memini.urlopen = _no_network_urlopen


def make_provider(call_stub, result_stub=None):
    """A provider initialized against a dummy URL with its network _call stubbed.

    _call_result (the non-degrading write path the remember tool uses) defaults to
    the same stub with an empty error, so a test that only cares about the request
    body does not have to stub twice. Pass result_stub to exercise a server error.
    """
    os.environ["MEMINI_BASE_URL"] = "http://localhost:8080"
    os.environ.pop("MEMINI_NAMESPACE", None)
    p = memini.MeminiMemoryProvider()
    p.initialize("sess-1")
    p._call = call_stub
    p._call_result = result_stub or (
        lambda path, body, method="POST": (call_stub(path, body, method), "")
    )
    return p


class SanitizeNamespaceTest(unittest.TestCase):
    def test_collapses_unsafe_chars_for_the_header(self):
        self.assertEqual(memini._sanitize_namespace("my project (wip)"), "my-project-wip")
        self.assertEqual(memini._sanitize_namespace("repo.name_ok-1"), "repo.name_ok-1")
        self.assertEqual(memini._sanitize_namespace("  --x--  "), "x")

    def test_initialize_uses_the_env_namespace_raw_trimmed(self):
        # MEMINI_NAMESPACE wins and is used raw-trimmed (the server validates the
        # header); a hierarchical value keeps its "/" instead of flattening, so
        # it matches the other integrations.
        os.environ["MEMINI_BASE_URL"] = "http://localhost:8080"
        os.environ["MEMINI_NAMESPACE"] = "  team space/eu  "
        try:
            p = memini.MeminiMemoryProvider()
            p.initialize("sess-1")
            self.assertEqual(p._namespace, "team space/eu")
            self.assertEqual(p._namespace_source, "env")
        finally:
            os.environ.pop("MEMINI_NAMESPACE", None)


class ApiErrorSurfacingTest(unittest.TestCase):
    def test_failure_is_logged_not_silent(self):
        # A swallowed capture/recall failure looks like "memory isn't working";
        # _api must degrade to None but say why on stderr.
        import io
        from contextlib import redirect_stderr

        err = io.StringIO()
        with redirect_stderr(err):
            out = memini._api("http://127.0.0.1:1", "/v1/search", {}, "ns", "")
        self.assertIsNone(out)
        self.assertIn("[memini] POST /v1/search failed:", err.getvalue())


class HomeHeaderTest(unittest.TestCase):
    def test_home_header_emitted_when_set_omitted_otherwise(self):
        # _api's Request goes through urllib, which capitalizes header keys as
        # "X-memini-home" (Request.add_header capitalizes only the first
        # character); assert on that exact key.
        captured = []

        class FakeResp:
            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def read(self):
                return b"{}"

        def fake_urlopen(req, timeout=None):
            captured.append(req)
            return FakeResp()

        real_urlopen = memini.urlopen
        memini.urlopen = fake_urlopen
        try:
            os.environ["MEMINI_HOME"] = "personal/acme"
            memini._api("http://localhost:8080", "/v1/search", {}, "ns", "")
            os.environ.pop("MEMINI_HOME", None)
            memini._api("http://localhost:8080", "/v1/search", {}, "ns", "")
        finally:
            memini.urlopen = real_urlopen

        self.assertEqual(len(captured), 2)
        self.assertEqual(captured[0].get_header("X-memini-home"), "personal/acme")
        self.assertIsNone(captured[1].get_header("X-memini-home"))

    def test_namespace_header_omitted_when_empty(self):
        # /v1/handshake has no namespace yet to send; _api must skip the
        # header entirely rather than send it empty.
        captured = []

        class FakeResp:
            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def read(self):
                return b"{}"

        def fake_urlopen(req, timeout=None):
            captured.append(req)
            return FakeResp()

        real_urlopen = memini.urlopen
        memini.urlopen = fake_urlopen
        try:
            memini._api("http://localhost:8080", "/v1/handshake", {}, "", "")
        finally:
            memini.urlopen = real_urlopen

        self.assertIsNone(captured[0].get_header("X-memini-namespace"))


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
        self.assertEqual(
            sorted(names),
            [
                "memory_briefing",
                "memory_forget",
                "memory_list",
                "memory_recall",
                "memory_remember",
                "memory_status",
            ],
        )

    def test_recall_and_briefing_offer_the_semantic_scope_vocabulary(self):
        # These schemas are this harness's whole model-facing surface (it does
        # not proxy MCP), so a lever missing here is a lever the model does not
        # have. The deprecated REST aliases exact/subtree stay off the list.
        schemas = {t["name"]: t for t in memini.MeminiMemoryProvider().get_tool_schemas()}
        for name in ("memory_recall", "memory_briefing"):
            scope = schemas[name]["parameters"]["properties"]["scope"]
            self.assertEqual(scope["enum"], ["project", "full", "everywhere"])
            self.assertEqual(scope["default"], "full")
            self.assertNotIn("scope", schemas[name]["parameters"].get("required", []))

    def test_remember_offers_visibility_without_a_client_side_enum(self):
        schemas = {t["name"]: t for t in memini.MeminiMemoryProvider().get_tool_schemas()}
        vis = schemas["memory_remember"]["parameters"]["properties"]["visibility"]
        # No enum on purpose: 'project'/'personal' are fixed, but any other value
        # names an ancestor of THIS namespace, which only the server can resolve.
        self.assertNotIn("enum", vis)
        self.assertIn("personal", vis["description"])


class BriefingPathTest(unittest.TestCase):
    def test_forwards_only_a_known_scope(self):
        self.assertEqual(memini._briefing_path({}), "/v1/namespaces/briefing")
        self.assertEqual(
            memini._briefing_path({"scope": "everywhere"}),
            "/v1/namespaces/briefing?scope=everywhere",
        )
        # The server 400s on an unknown scope; a bad guess must not turn
        # orientation into an error.
        self.assertEqual(memini._briefing_path({"scope": "subtree"}), "/v1/namespaces/briefing")
        self.assertEqual(memini._briefing_path({"scope": "acme"}), "/v1/namespaces/briefing")


class ScopeAndVisibilityToolTest(unittest.TestCase):
    def test_recall_forwards_a_known_scope_and_drops_the_rest(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["body"] = body
            return {"results": []}

        p = make_provider(stub)
        p.handle_tool_call("memory_recall", {"query": "q", "scope": "everywhere"})
        self.assertEqual(captured["body"]["scope"], "everywhere")

        # Omitted: nothing on the wire, so the server's "full" default applies.
        p.handle_tool_call("memory_recall", {"query": "q"})
        self.assertNotIn("scope", captured["body"])

        p.handle_tool_call("memory_recall", {"query": "q", "scope": "exact"})
        self.assertNotIn("scope", captured["body"])

    def test_recall_passes_read_provenance_through(self):
        def stub(path, body, method="POST"):
            return {
                "results": [
                    {"memory": {"id": "m1", "content": "own", "namespace": "ns"}, "score": 0.9},
                    {
                        "memory": {"id": "m2", "content": "inherited", "namespace": "acme"},
                        "score": 0.5,
                        "from": "acme",
                    },
                ]
            }

        out = json.loads(make_provider(stub).handle_tool_call("memory_recall", {"query": "q"}))
        # A primary-namespace hit carries no "from" at all — its absence is what
        # tells the model "this project's own memory".
        self.assertNotIn("from", out["results"][0])
        self.assertEqual(out["results"][0]["namespace"], "ns")
        self.assertEqual(out["results"][1]["from"], "acme")

    def test_remember_forwards_visibility_verbatim(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["body"] = body
            return {"id": "m1"}

        p = make_provider(stub)
        p.handle_tool_call("memory_remember", {"content": "f", "visibility": "personal"})
        self.assertEqual(captured["body"]["visibility"], "personal")

        # An ancestor name is in no client-side enum: only the server knows this
        # namespace's chain, so the name goes through untouched.
        p.handle_tool_call("memory_remember", {"content": "f", "visibility": "acme"})
        self.assertEqual(captured["body"]["visibility"], "acme")

        p.handle_tool_call("memory_remember", {"content": "f"})
        self.assertNotIn("visibility", captured["body"])

    def test_rejected_visibility_returns_the_servers_valid_chain(self):
        error = 'remember: visibility "widgets" not in scope; valid: project, personal, acme'
        p = make_provider(
            lambda *a, **k: {},
            result_stub=lambda path, body, method="POST": (None, error),
        )
        out = json.loads(
            p.handle_tool_call("memory_remember", {"content": "f", "visibility": "widgets"})
        )
        self.assertFalse(out["success"])
        # Without the server's error text the model has nothing to correct
        # against — it would just retry the same bad name.
        self.assertIn("valid: project, personal, acme", out["error"])


class BriefingToolTest(unittest.TestCase):
    def test_briefing_gets_the_header_scoped_endpoint_and_keeps_the_scope_line(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["path"], captured["method"] = path, method
            return {
                "namespace": "ns",
                "scope_header": "Scope: ns ← acme(4) ← personal(2)",
                "pinned": [{"memory": {"id": "p1", "content": "pinned", "tier": "semantic"}}],
                "facts": [
                    {
                        "memory": {"id": "f1", "content": "org", "namespace": "acme"},
                        "from": "acme",
                    }
                ],
            }

        out = json.loads(
            make_provider(stub).handle_tool_call("memory_briefing", {"scope": "full"})
        )
        self.assertEqual(captured["method"], "GET")
        # Header-scoped: the namespace is never in the path — the model never
        # names one.
        self.assertEqual(captured["path"], "/v1/namespaces/briefing?scope=full")
        self.assertEqual(out["scope_header"], "Scope: ns ← acme(4) ← personal(2)")
        self.assertEqual(out["pinned"][0]["id"], "p1")
        self.assertEqual(out["facts"][0]["from"], "acme")

    def test_briefing_answers_rather_than_raising_when_memini_is_unreachable(self):
        out = json.loads(
            make_provider(lambda *a, **k: None).handle_tool_call("memory_briefing", {})
        )
        self.assertIsNone(out["briefing"])


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


class SyncTurnTest(unittest.TestCase):
    def test_adopts_host_session_id_for_capture_and_recall(self):
        # Hermes can switch session_id mid-process (branch/rewind/rotation) and
        # passes the current one to sync_turn. Capture must carry it AND later
        # recalls must exclude the same value — a mismatch means the session's
        # own captured turns echo back on the next prefetch.
        writes = []
        p = make_provider(lambda *a, **k: {"results": []})
        p._call_bg = lambda path, body: writes.append((path, body))
        p.sync_turn("q", "a", session_id="sess-2")
        self.assertEqual(writes[0][1]["metadata"]["session_id"], "sess-2")

        captured = {}

        def stub(path, body, method="POST"):
            captured["body"] = body
            return {"results": []}

        p._call = stub
        p.prefetch("next question")
        self.assertEqual(captured["body"]["exclude_metadata"], {"session_id": "sess-2"})

    def test_skips_capture_without_any_session_id(self):
        # A capture without a session id can never be excluded on recall, so
        # it would echo this session's turns back forever.
        writes = []
        p = make_provider(lambda *a, **k: None)
        p._session_id = ""
        p._call_bg = lambda path, body: writes.append(path)
        p.sync_turn("q", "a")
        self.assertEqual(writes, [])


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
            # Tier is omitted so the server classifies the content.
            self.assertNotIn("tier", captured["body"])
            self.assertEqual(captured["body"]["content"], "a durable fact")

    def test_ignores_remove_and_empty_content(self):
        captured = {}
        p = self._provider(captured)
        p.on_memory_write("remove", "memory", "gone")
        p.on_memory_write("add", "memory", "   ")
        self.assertEqual(captured, {})


class HandleToolCallTest(unittest.TestCase):
    def test_remember_maps_category_and_omits_tier(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["path"], captured["body"] = path, body
            return {"id": "m1"}

        out = make_provider(stub).handle_tool_call(
            "memory_remember", {"content": "fact", "category": "bug_fixes", "tags": ["x"]}
        )
        self.assertEqual(captured["path"], "/v1/memories")
        # Tier omitted so the server classifies; a valid explicit tier is still forwarded.
        self.assertEqual(
            captured["body"],
            {"content": "fact", "tags": ["x"], "metadata": {"category": "bug_fixes"}},
        )
        self.assertEqual(json.loads(out), {"id": "m1", "success": True})

    def test_remember_drops_unknown_tier(self):
        captured = {}

        def stub(path, body, method="POST"):
            captured["body"] = body
            return {"id": "m2"}

        make_provider(stub).handle_tool_call("memory_remember", {"content": "f", "tier": "bogus"})
        self.assertNotIn("tier", captured["body"])
        make_provider(stub).handle_tool_call("memory_remember", {"content": "f", "tier": "procedural"})
        self.assertEqual(captured["body"]["tier"], "procedural")

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


# --- Handshake (POST /v1/handshake wire contract) -------------------------


class _FakeResp:
    def __init__(self, body: bytes):
        self._body = body

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def read(self):
        return self._body


def _urlopen_returning(payload):
    def _urlopen(req, timeout=None):
        return _FakeResp(json.dumps(payload).encode())

    return _urlopen


def _urlopen_raising(exc):
    def _urlopen(req, timeout=None):
        raise exc

    return _urlopen


class HandshakeTest(unittest.TestCase):
    """precedence, fail-soft, TTL memo, and the settings fallback chain for
    POST /v1/handshake (api/openapi.yaml)."""

    def setUp(self):
        self._real_urlopen = memini.urlopen
        memini._handshake_cache.clear()
        self.addCleanup(self._restore)

    def _restore(self):
        memini.urlopen = self._real_urlopen
        memini._handshake_cache.clear()

    def test_facts_sends_cwd_basename_git_remote_toplevel_and_env_namespace(self):
        parent = os.path.realpath(tempfile.mkdtemp(prefix="memini-facts-"))
        proj = os.path.join(parent, "widget")
        os.makedirs(proj)
        subprocess.run(["git", "init", "-q"], cwd=proj, check=True)
        subprocess.run(
            ["git", "remote", "add", "origin", "https://github.com/eleboucher/widget.git"],
            cwd=proj,
            check=True,
        )
        os.environ["MEMINI_NAMESPACE"] = "team/eu"
        self.addCleanup(lambda: os.environ.pop("MEMINI_NAMESPACE", None))
        facts = memini._facts(proj)
        self.assertEqual(facts["cwd_basename"], "widget")
        self.assertEqual(facts["remote_url"], "https://github.com/eleboucher/widget.git")
        self.assertTrue(facts["toplevel_path"])
        self.assertEqual(facts["toplevel_basename"], os.path.basename(facts["toplevel_path"]))
        self.assertEqual(facts["env_namespace"], "team/eu")

    def test_facts_omits_remote_toplevel_and_env_namespace_when_absent(self):
        proj = os.path.realpath(tempfile.mkdtemp(prefix="memini-facts-nogit-"))
        os.environ.pop("MEMINI_NAMESPACE", None)
        facts = memini._facts(proj)
        self.assertEqual(facts["cwd_basename"], os.path.basename(proj))
        self.assertNotIn("remote_url", facts)
        self.assertNotIn("toplevel_path", facts)
        self.assertNotIn("env_namespace", facts)

    def test_derive_local_namespace_prefers_remote_over_toplevel_over_cwd(self):
        parent = os.path.realpath(tempfile.mkdtemp(prefix="memini-derive-"))
        proj = os.path.join(parent, "checkout-dir")
        os.makedirs(proj)
        subprocess.run(["git", "init", "-q"], cwd=proj, check=True)
        subprocess.run(
            ["git", "remote", "add", "origin", "https://github.com/eleboucher/widget.git"],
            cwd=proj,
            check=True,
        )
        # The git remote name wins over the cwd basename (checkout-dir).
        self.assertEqual(memini._derive_local_namespace(proj), ("widget", "remote"))

        no_remote = os.path.realpath(tempfile.mkdtemp(prefix="memini-derive-noremote-"))
        subprocess.run(["git", "init", "-q"], cwd=no_remote, check=True)
        self.assertEqual(
            memini._derive_local_namespace(no_remote),
            (os.path.basename(no_remote), "toplevel"),
        )

        no_git = os.path.realpath(tempfile.mkdtemp(prefix="memini-derive-nogit-"))
        self.assertEqual(memini._derive_local_namespace(no_git), (os.path.basename(no_git), "cwd"))

    def test_resolve_namespace_precedence_env_beats_handshake_beats_local(self):
        proj = os.path.realpath(tempfile.mkdtemp(prefix="memini-precedence-"))
        hs = {"namespace": "acme/widget", "namespace_source": "remote", "settings": {}}

        os.environ["MEMINI_NAMESPACE"] = "pinned"
        self.addCleanup(lambda: os.environ.pop("MEMINI_NAMESPACE", None))
        self.assertEqual(memini._resolve_namespace(proj, hs), ("pinned", "env"))

        os.environ.pop("MEMINI_NAMESPACE", None)
        self.assertEqual(memini._resolve_namespace(proj, hs), ("acme/widget", "server:remote"))

        # No handshake (fail-soft) -> local derivation, "local-*" labeled.
        self.assertEqual(
            memini._resolve_namespace(proj, None),
            (os.path.basename(proj), "local-cwd"),
        )

    def test_handshake_is_fail_soft_on_any_urlopen_error(self):
        memini.urlopen = _urlopen_raising(URLError("boom"))
        result = memini._handshake("http://localhost:8080", "", {"cwd_basename": "x"})
        self.assertIsNone(result)

    def test_cached_handshake_memoizes_until_the_ttl_expires(self):
        calls = []

        def urlopen(req, timeout=None):
            calls.append(1)
            return _FakeResp(
                json.dumps({"namespace": "ns", "namespace_source": "cwd", "settings": {}}).encode()
            )

        memini.urlopen = urlopen
        time_box = {"t": 0.0}

        first = memini._cached_handshake(
            "http://localhost:8080", "", {"cwd_basename": "x"}, now=lambda: time_box["t"]
        )
        self.assertEqual(first["namespace"], "ns")
        self.assertEqual(len(calls), 1)

        memini._cached_handshake(
            "http://localhost:8080", "", {"cwd_basename": "x"}, now=lambda: time_box["t"]
        )
        self.assertEqual(len(calls), 1, "must reuse the memoized result within the TTL")

        time_box["t"] = memini._HANDSHAKE_TTL + 1
        memini._cached_handshake(
            "http://localhost:8080", "", {"cwd_basename": "x"}, now=lambda: time_box["t"]
        )
        self.assertEqual(len(calls), 2, "must refetch after the TTL expires")

    def test_initialize_uses_the_handshakes_namespace_absent_env(self):
        os.environ.pop("MEMINI_NAMESPACE", None)
        os.environ["MEMINI_BASE_URL"] = "http://localhost:8080"
        self.addCleanup(lambda: os.environ.pop("MEMINI_BASE_URL", None))
        memini.urlopen = _urlopen_returning(
            {"namespace": "acme/widget", "namespace_source": "remote", "settings": {}}
        )
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-hs")
        self.assertEqual(p._namespace, "acme/widget")
        self.assertEqual(p._namespace_source, "server:remote")

    def test_initialize_falls_back_to_local_derivation_when_handshake_fails(self):
        os.environ.pop("MEMINI_NAMESPACE", None)
        os.environ["MEMINI_BASE_URL"] = "http://localhost:8080"
        self.addCleanup(lambda: os.environ.pop("MEMINI_BASE_URL", None))
        memini.urlopen = _urlopen_raising(URLError("connection refused"))
        proj = os.path.realpath(tempfile.mkdtemp(prefix="memini-fallback-"))
        prev_cwd = os.getcwd()
        os.chdir(proj)
        self.addCleanup(lambda: os.chdir(prev_cwd))
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-fallback")
        self.assertEqual(p._namespace, os.path.basename(proj))
        self.assertEqual(p._namespace_source, "local-cwd")

    def test_initialize_settings_fallback_chain_env_beats_server_beats_default(self):
        os.environ.pop("MEMINI_NAMESPACE", None)
        os.environ["MEMINI_BASE_URL"] = "http://localhost:8080"
        self.addCleanup(lambda: os.environ.pop("MEMINI_BASE_URL", None))
        memini.urlopen = _urlopen_returning(
            {
                "namespace": "ns",
                "namespace_source": "cwd",
                "settings": {
                    "recall": False,
                    "capture": False,
                    "recall_limit": 7,
                    "inject_recall_max_tok": 500,
                    "inject_recall_min_score": 0.3,
                },
            }
        )
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-settings")
        self.assertFalse(p._recall_enabled)
        self.assertFalse(p._capture_enabled)
        self.assertEqual(p._recall_limit, 7)
        self.assertEqual(p._recall_max_tokens, 500)
        self.assertEqual(p._recall_min_score, 0.3)

        # An explicit env value still beats the server's settings.
        memini._handshake_cache.clear()
        os.environ["MEMINI_RECALL_LIMIT"] = "9"
        self.addCleanup(lambda: os.environ.pop("MEMINI_RECALL_LIMIT", None))
        p2 = memini.MeminiMemoryProvider()
        p2.initialize("sess-settings-2")
        self.assertEqual(p2._recall_limit, 9)

    def test_prefetch_and_sync_turn_are_gated_by_the_servers_recall_capture_settings(self):
        os.environ.pop("MEMINI_NAMESPACE", None)
        os.environ["MEMINI_BASE_URL"] = "http://localhost:8080"
        self.addCleanup(lambda: os.environ.pop("MEMINI_BASE_URL", None))
        memini.urlopen = _urlopen_returning(
            {"namespace": "ns", "namespace_source": "cwd", "settings": {"recall": False, "capture": False}}
        )
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-gate")
        calls = []
        p._call = lambda *a, **k: calls.append(a) or {"results": []}
        p._call_bg = lambda *a, **k: calls.append(a)
        self.assertEqual(p.prefetch("q"), "")
        p.sync_turn("q", "a", session_id="s1")
        self.assertEqual(calls, [], "recall/capture disabled server-side must not call out")

    def test_handshake_carries_the_home_header_and_client_identification(self):
        captured = []

        def urlopen(req, timeout=None):
            captured.append(req)
            return _FakeResp(json.dumps({}).encode())

        memini.urlopen = urlopen
        os.environ["MEMINI_HOME"] = "personal/acme"
        self.addCleanup(lambda: os.environ.pop("MEMINI_HOME", None))
        memini._handshake("http://localhost:8080", "", {"cwd_basename": "x"})
        req = captured[0]
        self.assertEqual(req.get_full_url(), "http://localhost:8080/v1/handshake")
        self.assertEqual(req.get_header("X-memini-home"), "personal/acme")
        body = json.loads(req.data.decode())
        self.assertEqual(body["client"]["name"], "hermes-memini")
        self.assertIn("version", body["client"])


class StatusToolTest(unittest.TestCase):
    ENV = (
        "MEMINI_NAMESPACE",
        "MEMINI_BASE_URL",
        "MEMINI_API_KEY",
        "MEMINI_HOME",
    )

    def setUp(self):
        for k in self.ENV:
            os.environ.pop(k, None)
        self.addCleanup(lambda: [os.environ.pop(k, None) for k in self.ENV])
        self.proj = os.path.realpath(tempfile.mkdtemp(prefix="memini-status-"))
        prev_cwd = os.getcwd()
        os.chdir(self.proj)
        self.addCleanup(lambda: os.chdir(prev_cwd))

    def _status(self):
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-status")
        return p.handle_tool_call("memory_status", {})

    def test_redact_fingerprints_a_token_and_elides_a_short_one(self):
        self.assertEqual(memini._redact("sk-0123456789abcd4f2a"), "sk-…4f2a")
        self.assertEqual(memini._redact("short"), "***")
        self.assertEqual(memini._redact(""), "")
        self.assertTrue(memini._is_sensitive("MEMINI_API_KEY"))
        self.assertTrue(memini._is_sensitive("MEMINI_TOKEN"))
        self.assertFalse(memini._is_sensitive("MEMINI_BASE_URL"))

    def test_status_exposes_a_global_env_pin_and_redacts_the_token(self):
        os.environ["MEMINI_BASE_URL"] = "http://memini.example.com"
        os.environ["MEMINI_NAMESPACE"] = "pinned"
        os.environ["MEMINI_API_KEY"] = "sk-0123456789abcd4f2a"
        out = self._status()
        # Provenance, not just the value: "pinned <- env", and what the project
        # would otherwise resolve to.
        self.assertIn("<- env", out)
        self.assertIn(os.path.basename(self.proj), out)
        self.assertIn("global-namespace-pin", out)
        self.assertIn("plaintext-bearer", out)
        # The secret is fingerprinted, never printed.
        self.assertNotIn("0123456789", out)
        self.assertIn("sk-…4f2a", out)

    def test_status_flags_a_namespace_resolved_before_a_later_change_took_effect(self):
        # The namespace is resolved once, in initialize; a namespace that would
        # now resolve differently (a new pin, a settings change, a handshake
        # that only now succeeds) only reaches the wire after a restart. Say so
        # rather than reporting a namespace this session is not actually using.
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-restart")
        report = memini._describe_settings(self.proj, in_use="stale-namespace")
        codes = [w["code"] for w in report["warnings"]]
        self.assertIn("restart-required", codes)
        self.assertIn("resolved at startup", memini._render_settings(report))


class IsAvailableTest(unittest.TestCase):
    def test_valid_url_is_available(self):
        os.environ["MEMINI_BASE_URL"] = "http://localhost:8080"
        self.addCleanup(lambda: os.environ.pop("MEMINI_BASE_URL", None))
        self.assertTrue(memini.MeminiMemoryProvider().is_available())

    def test_invalid_url_is_unavailable(self):
        os.environ["MEMINI_BASE_URL"] = "not-a-url"
        self.addCleanup(lambda: os.environ.pop("MEMINI_BASE_URL", None))
        self.assertFalse(memini.MeminiMemoryProvider().is_available())


class SaveConfigTest(unittest.TestCase):
    def test_does_not_persist_secret_to_disk(self):
        import tempfile as _tempfile

        p = memini.MeminiMemoryProvider()
        with _tempfile.TemporaryDirectory() as home:
            p.save_config({"url": "http://localhost:8080", "namespace": "x", "secret": "tok-123"}, home)
            with open(os.path.join(home, "memini.json")) as f:
                saved = json.load(f)
        self.assertEqual(saved.get("url"), "http://localhost:8080")
        self.assertEqual(saved.get("namespace"), "x")
        self.assertNotIn("secret", saved, "bearer token must not be written to disk")


class ReinforcedFlagTest(unittest.TestCase):
    def test_reinforced_is_surfaced_so_a_no_op_write_is_not_a_new_save(self):
        # The fact was already known: the server strengthened the existing memory
        # and returned its id. Dropping the flag would let the model claim a
        # fresh save.
        p = make_provider(
            lambda *a, **k: {},
            result_stub=lambda path, body, method="POST": (
                {"id": "existing-1", "reinforced": True},
                "",
            ),
        )
        out = json.loads(p.handle_tool_call("memory_remember", {"content": "known fact"}))
        self.assertEqual(out, {"id": "existing-1", "success": True, "reinforced": True})

    def test_a_genuinely_new_write_carries_no_flag(self):
        p = make_provider(lambda *a, **k: {"id": "new-1"})
        out = json.loads(p.handle_tool_call("memory_remember", {"content": "novel fact"}))
        self.assertNotIn("reinforced", out)


if __name__ == "__main__":
    unittest.main()
