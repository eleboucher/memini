# Tests for the memini Hermes memory provider. Pure stdlib (unittest), no
# Hermes install needed — the plugin falls back to an inline ABC when
# `agent.memory_provider` is absent. Run: python3 -m unittest (from this dir).
#
# This file is a sibling of the memini/ package so the release mirror (which
# copies memini/. into eleboucher/memini-hermes) does not ship it.
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import memini  # noqa: E402

# A developer shell may export the real memini config; clear it so each test's
# explicit env is authoritative (canonical MEMINI_BASE_URL would otherwise win
# over a test that sets only MEMINI_URL).
for _v in ("MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN", "MEMINI_HOME"):
    os.environ.pop(_v, None)
# Point XDG_CONFIG_HOME at an empty temp dir so a developer's real
# ~/.config/memini/config.json can't leak tenant prefixes into these tests.
os.environ["XDG_CONFIG_HOME"] = tempfile.mkdtemp(prefix="memini-test-config-")


def make_provider(call_stub):
    """A provider initialized against a dummy URL with its network _call stubbed."""
    os.environ["MEMINI_URL"] = "http://localhost:8080"
    os.environ.pop("MEMINI_NAMESPACE", None)
    p = memini.MeminiMemoryProvider()
    p.initialize("sess-1")
    p._call = call_stub
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
        os.environ["MEMINI_URL"] = "http://localhost:8080"
        os.environ["MEMINI_NAMESPACE"] = "  team space/eu  "
        try:
            p = memini.MeminiMemoryProvider()
            p.initialize("sess-1")
            self.assertEqual(p._namespace, "team space/eu")
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
            ["memory_forget", "memory_list", "memory_recall", "memory_remember", "memory_status"],
        )


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


class ResolveTenantTest(unittest.TestCase):
    def _write_config(self, config):
        xdg = tempfile.mkdtemp(prefix="memini-test-config-")
        os.makedirs(os.path.join(xdg, "memini"))
        with open(os.path.join(xdg, "memini", "config.json"), "w") as f:
            json.dump(config, f)
        prev = os.environ.get("XDG_CONFIG_HOME")
        os.environ["XDG_CONFIG_HOME"] = xdg
        self.addCleanup(lambda: os.environ.__setitem__("XDG_CONFIG_HOME", prev))

    def test_tenant_separator_preserved_in_namespace(self):
        # work/proj, not work-proj: a flattened separator splits memory from
        # the other integrations, which all send the "/" form.
        # realpath: macOS tempdirs live under /var -> /private/var, but the plugin
        # reads os.getcwd() (symlink-resolved) while _match_tenant compares
        # lexically; an unresolved root would never match.
        parent = os.path.realpath(tempfile.mkdtemp(prefix="memini-tenant-"))
        proj = os.path.join(parent, "proj")
        os.makedirs(proj)
        self._write_config({"tenantRoots": [{"path": parent, "tenant": "work"}]})
        os.environ["MEMINI_URL"] = "http://localhost:8080"
        os.environ.pop("MEMINI_NAMESPACE", None)
        prev_cwd = os.getcwd()
        os.chdir(proj)
        self.addCleanup(lambda: os.chdir(prev_cwd))
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-tenant")
        self.assertEqual(p._namespace, "work/proj")

    def test_empty_path_entry_is_ignored(self):
        # Path("").resolve() used to equal the cwd, turning an empty path into
        # a spurious exact match for whatever directory hermes runs in.
        self._write_config({"tenantRoots": [{"path": "", "tenant": "evil"}]})
        self.assertIsNone(memini._resolve_tenant(os.getcwd()))

    def test_non_dict_entry_skipped_without_aborting_later_roots(self):
        parent = os.path.realpath(tempfile.mkdtemp(prefix="memini-tenant-"))
        self._write_config(
            {"tenantRoots": ["junk", {"tenant": "nopath"}, {"path": parent, "tenant": "work"}]}
        )
        self.assertEqual(memini._resolve_tenant(os.path.join(parent, "proj")), "work")

    def test_empty_xdg_falls_back_to_home_config(self):
        # An empty XDG_CONFIG_HOME must behave like unset (fall back to
        # ~/.config), not build a relative, never-found config path.
        prev = os.environ.get("XDG_CONFIG_HOME")
        self.addCleanup(
            lambda: os.environ.__setitem__("XDG_CONFIG_HOME", prev)
            if prev is not None
            else os.environ.pop("XDG_CONFIG_HOME", None)
        )
        os.environ["XDG_CONFIG_HOME"] = ""
        expected = os.path.join(
            os.path.expanduser("~"), ".config", "memini", "config.json"
        )
        self.assertEqual(str(memini._config_path()), expected)

    def test_config_present_derives_project_from_git(self):
        # Config present -> {project} is the git remote name, so a fork checked
        # out under a different folder name still lands in work/<repo>.
        import subprocess

        parent = os.path.realpath(tempfile.mkdtemp(prefix="memini-tenant-"))
        proj = os.path.join(parent, "memini-fork")
        os.makedirs(proj)
        subprocess.run(["git", "init", "-q"], cwd=proj, check=True)
        subprocess.run(
            ["git", "remote", "add", "origin", "https://github.com/eleboucher/memini.git"],
            cwd=proj,
            check=True,
        )
        self._write_config({"tenantRoots": [{"path": parent, "tenant": "work"}]})
        os.environ["MEMINI_URL"] = "http://localhost:8080"
        os.environ.pop("MEMINI_NAMESPACE", None)
        os.environ.pop("MEMINI_AGENT", None)
        self.addCleanup(lambda: os.environ.pop("MEMINI_AGENT", None))
        prev_cwd = os.getcwd()
        os.chdir(proj)
        self.addCleanup(lambda: os.chdir(prev_cwd))
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-git")
        self.assertEqual(p._namespace, "work/memini")

    def test_non_default_template_reshapes_namespace(self):
        parent = os.path.realpath(tempfile.mkdtemp(prefix="memini-tenant-"))
        proj = os.path.join(parent, "proj")
        os.makedirs(proj)
        self._write_config(
            {"tenantRoots": [{"path": parent, "tenant": "work"}], "template": "{tenant}-{project}"}
        )
        os.environ["MEMINI_URL"] = "http://localhost:8080"
        os.environ.pop("MEMINI_NAMESPACE", None)
        prev_cwd = os.getcwd()
        os.chdir(proj)
        self.addCleanup(lambda: os.chdir(prev_cwd))
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-tmpl")
        # proj is not a git repo, so {project} falls back to the cwd basename.
        self.assertEqual(p._namespace, "work-proj")


class NamespaceOverrideTest(unittest.TestCase):
    """The per-project override in $XDG_CONFIG_HOME/memini/overrides.json — the
    file the client plugins write and `memini doctor` reads."""

    def _write_overrides(self, overrides, raw=None):
        xdg = tempfile.mkdtemp(prefix="memini-test-config-")
        os.makedirs(os.path.join(xdg, "memini"))
        with open(os.path.join(xdg, "memini", "overrides.json"), "w") as f:
            f.write(raw if raw is not None else json.dumps({"version": 1, "overrides": overrides}))
        prev = os.environ.get("XDG_CONFIG_HOME")
        os.environ["XDG_CONFIG_HOME"] = xdg
        self.addCleanup(lambda: os.environ.__setitem__("XDG_CONFIG_HOME", prev))

    def _in(self, cwd):
        prev_cwd = os.getcwd()
        os.chdir(cwd)
        self.addCleanup(lambda: os.chdir(prev_cwd))
        os.environ["MEMINI_URL"] = "http://localhost:8080"
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-override")
        return p

    def test_override_beats_the_env_pin(self):
        # The whole point of the ordering: a globally exported MEMINI_NAMESPACE
        # pins every repo on the machine, and if it won here, setting an override
        # would silently do nothing on exactly the machines that need one.
        proj = os.path.realpath(tempfile.mkdtemp(prefix="memini-override-"))
        self._write_overrides({proj: {"namespace": "acme/api", "setAt": "2026-07-12T20:30:00Z"}})
        os.environ["MEMINI_NAMESPACE"] = "pinned"
        self.addCleanup(lambda: os.environ.pop("MEMINI_NAMESPACE", None))
        p = self._in(proj)
        self.assertEqual(p._namespace, "acme/api")
        self.assertEqual(p._namespace_source, "override")

    def test_override_is_keyed_on_the_git_toplevel(self):
        # An override set at the top of a repo must still apply when hermes runs
        # three directories down.
        import subprocess

        repo = os.path.realpath(tempfile.mkdtemp(prefix="memini-override-repo-"))
        subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
        nested = os.path.join(repo, "services", "api")
        os.makedirs(nested)
        self._write_overrides({repo: {"namespace": "acme/api", "setAt": ""}})
        os.environ.pop("MEMINI_NAMESPACE", None)
        self.assertEqual(memini._override_key(nested), repo)
        self.assertEqual(self._in(nested)._namespace, "acme/api")

    def test_malformed_or_absent_file_degrades_to_automatic_resolution(self):
        proj = os.path.realpath(tempfile.mkdtemp(prefix="memini-override-"))
        os.environ.pop("MEMINI_NAMESPACE", None)
        # Absent: the module-level XDG temp dir holds no overrides.json.
        self.assertIsNone(memini._read_override(proj))
        # Hand-edited into invalid JSON: degrade, never raise into hermes.
        self._write_overrides(None, raw="{ not json")
        self.assertIsNone(memini._read_override(proj))
        self.assertEqual(self._in(proj)._namespace, os.path.basename(proj))

    def test_wrong_shape_and_blank_namespace_are_not_overrides(self):
        proj = os.path.realpath(tempfile.mkdtemp(prefix="memini-override-"))
        self._write_overrides(None, raw=json.dumps({"version": 1, "overrides": []}))
        self.assertIsNone(memini._read_override(proj))
        self._write_overrides({proj: {"namespace": "   "}})
        self.assertIsNone(memini._read_override(proj))

    def test_empty_xdg_falls_back_to_home_overrides(self):
        prev = os.environ.get("XDG_CONFIG_HOME")
        self.addCleanup(lambda: os.environ.__setitem__("XDG_CONFIG_HOME", prev))
        os.environ["XDG_CONFIG_HOME"] = ""
        self.assertEqual(
            str(memini._overrides_path()),
            os.path.join(os.path.expanduser("~"), ".config", "memini", "overrides.json"),
        )


class StatusToolTest(unittest.TestCase):
    ENV = (
        "MEMINI_NAMESPACE",
        "MEMINI_BASE_URL",
        "MEMINI_URL",
        "MEMINI_API_KEY",
        "MEMINI_TOKEN",
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

    def test_status_shows_what_an_active_override_is_masking(self):
        xdg = tempfile.mkdtemp(prefix="memini-test-config-")
        os.makedirs(os.path.join(xdg, "memini"))
        with open(os.path.join(xdg, "memini", "overrides.json"), "w") as f:
            json.dump(
                {"version": 1, "overrides": {self.proj: {"namespace": "acme/api", "setAt": "2026-07-12T20:30:00Z"}}},
                f,
            )
        prev = os.environ.get("XDG_CONFIG_HOME")
        os.environ["XDG_CONFIG_HOME"] = xdg
        self.addCleanup(lambda: os.environ.__setitem__("XDG_CONFIG_HOME", prev))
        os.environ["MEMINI_NAMESPACE"] = "pinned"
        out = self._status()
        self.assertIn("acme/api", out)
        self.assertIn("without the override", out)
        self.assertIn("override-active", out)
        # An override IS the fix for a global pin; it must not also nag about one.
        self.assertNotIn("global-namespace-pin", out)

    def test_status_flags_a_namespace_resolved_before_the_override_was_set(self):
        # The namespace is resolved once, in initialize; an override set
        # mid-session only reaches the wire after a restart. Say so rather than
        # reporting a namespace this session is not actually using.
        p = memini.MeminiMemoryProvider()
        p.initialize("sess-restart")
        report = memini._describe_settings(self.proj, in_use="stale-namespace")
        codes = [w["code"] for w in report["warnings"]]
        self.assertIn("restart-required", codes)
        self.assertIn("resolved at startup", memini._render_settings(report))


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
