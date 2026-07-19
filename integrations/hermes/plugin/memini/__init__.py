"""memini memory provider for Hermes Agent.

Native MemoryProvider: Hermes drives it directly, no MCP server.

    prefetch         recall relevant memories before each turn
    sync_turn        capture each exchange (episodic)
    on_pre_compress  re-inject recalled context before compaction
    on_memory_write  mirror MEMORY.md/USER.md edits into memini (semantic)
    tools            memory_recall / memory_briefing / memory_list /
                     memory_remember / memory_forget / memory_status

Install: copy this directory to ~/.hermes/plugins/memini and set
`memory.provider: memini` in ~/.hermes/config.yaml. Memory providers are
single-select; `plugins.enabled` does not activate them.

Namespace resolution, in order: MEMINI_NAMESPACE (raw-trimmed) > the namespace
a POST /v1/handshake resolves server-side (api/openapi.yaml) > the local git
remote/toplevel/cwd derivation chain. The handshake is fail-soft: any error or
a ~2.5s timeout falls back to local derivation, never breaks a turn. It is
memoized module-wide with a 10-minute TTL, so a long-lived Hermes process
re-handshakes at most once per ~10 minutes rather than on every call — see
_cached_handshake below.

Environment:
    MEMINI_BASE_URL                 base URL (default http://localhost:8080)
    MEMINI_NAMESPACE                project to scope memory to (default: server handshake, else git/cwd, else "hermes")
    MEMINI_HOME                     caller's personal namespace, sent as X-Memini-Home (unset = no home leg)
    MEMINI_API_KEY                  bearer token, if memini requires auth
    MEMINI_REQUIRE_HTTPS            =1 to refuse sending a token over plaintext HTTP
    MEMINI_RECALL_LIMIT             max memories recalled per turn (default 3; beneath the server's setting)
    MEMINI_INJECT_RECALL_MIN_SCORE  floor (>=) on the final ranked (composite) score for auto-recall (default 0; beneath the server's setting)
    MEMINI_INJECT_RECALL_MAX_TOK    hard token ceiling on the recall block (0 = unbounded; beneath the server's setting)
    MEMINI_INJECT_LABELS            comma/pipe bullet labels: tier, confidence, age
    MEMINI_TIMEOUT_MS               per-request timeout in ms (default 30000; beneath the server's setting).
                                    Must exceed the server's MEMINI_RERANK_TIMEOUT, or a slow reranker
                                    returns nothing instead of degrading to composite order.

Each env var above beats the server's handshake-resolved settings, which beat
the built-in default (recall_limit=3, no score floor, unbounded tokens).
recall/capture (on by default) have no local env toggle here — only the
server's settings can turn them off — so as not to invent a knob this
provider has never exposed.

The recall knobs mirror the opencode/Claude Code plugins so memory shaping is
consistent across integrations. Network errors are swallowed.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any, Callable
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode, urlparse
from urllib.request import Request, urlopen

try:
    from agent.memory_provider import MemoryProvider
except ImportError:  # allow import/testing outside a Hermes install
    from abc import ABC, abstractmethod

    class MemoryProvider(ABC):
        @property
        @abstractmethod
        def name(self) -> str: ...
        @abstractmethod
        def is_available(self) -> bool: ...
        @abstractmethod
        def initialize(self, session_id: str, **kwargs: Any) -> None: ...
        @abstractmethod
        def get_tool_schemas(self) -> list[dict]: ...
        @abstractmethod
        def handle_tool_call(self, name: str, args: dict, **kwargs: Any) -> str: ...
        def get_config_schema(self) -> list[dict]:
            return []

        def save_config(self, values: dict, hermes_home: str) -> None:
            pass

        def prefetch(self, query: str, **kwargs: Any) -> str:
            return ""

        def sync_turn(self, user: str, assistant: str, **kwargs: Any) -> None:
            pass

        def on_pre_compress(self, messages: list, **kwargs: Any) -> str:
            return ""

        def on_memory_write(
            self, action: str, target: str, content: str, **kwargs: Any
        ) -> None:
            pass


DEFAULT_BASE_URL = "http://localhost:8080"
# Seconds a single memini call may take. MEMINI_TIMEOUT_MS (milliseconds, the
# name every other integration uses) overrides it; request_timeout_ms from the
# handshake fills the gap when it is unset -- see _request_timeout().
#
# 30s, not the 5s this used to hardcode, because the ceiling must stay ABOVE the
# server's MEMINI_RERANK_TIMEOUT (10s): the server bounds a slow reranker and
# degrades to composite order, but a client that hangs up first receives nothing
# at all rather than an unranked result.
DEFAULT_TIMEOUT_MS = 30000
MIN_TIMEOUT_MS = 100
# The client identifies itself to /v1/handshake for logging/diagnostics only
# (api/openapi.yaml's HandshakeRequest.client) -- a plain literal, mirroring
# plugin.yaml's manually-maintained version rather than parsing YAML with no
# dependency to do it with.
_PLUGIN_VERSION = "0.6.9"
# How long a memoized handshake stays trustworthy, and how long a single
# handshake call may block before falling back. Mirrors
# packages/memini-client's HANDSHAKE_TTL_MS / default timeout; this plugin
# ships stdlib-only so it stays a copy, not an import.
_HANDSHAKE_TTL = 600.0  # seconds
_HANDSHAKE_TIMEOUT = 2.5  # seconds
LOOPBACK_HOSTS = {"localhost", "127.0.0.1", "::1"}
VALID_TIERS = ("working", "episodic", "semantic", "procedural")
# The LLM-facing semantic scope vocabulary, identical to the MCP server's
# (internal/api/mcp: scopeEnum). The deprecated REST aliases "exact"/"subtree"
# are deliberately NOT offered: the model makes a semantic choice, it does not
# speak the back-compat dialect.
VALID_SCOPES = ("project", "full", "everywhere")
_plaintext_bearer_warned = False


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def _base_url() -> str:
    return _env("MEMINI_BASE_URL") or DEFAULT_BASE_URL


def _secret() -> str:
    return _env("MEMINI_API_KEY")


def _home() -> str:
    """The caller's personal namespace, sent as X-Memini-Home. Env-only,
    mirroring MEMINI_NAMESPACE's precedence style — no cwd/derivation
    fallback; "" means no home leg."""
    return _env("MEMINI_HOME")


def _uses_plaintext_bearer_auth(base: str, secret: str) -> bool:
    if not secret:
        return False
    parsed = urlparse(base)
    host = (parsed.hostname or "").lower()
    return parsed.scheme == "http" and host not in LOOPBACK_HOSTS


def _check_plaintext_bearer_guard(
    base: str, secret: str, warn: Callable[[str], None] | None = None
) -> None:
    global _plaintext_bearer_warned
    if not _uses_plaintext_bearer_auth(base, secret):
        return
    msg = (
        f"memini: MEMINI_API_KEY is configured for plaintext HTTP to {base}. "
        "Bearer tokens and memory payloads can be observed on the network; "
        "use HTTPS or an SSH tunnel."
    )
    if _env("MEMINI_REQUIRE_HTTPS") == "1":
        raise RuntimeError(msg)
    if not _plaintext_bearer_warned:
        _plaintext_bearer_warned = True
        (warn or (lambda m: print(m, file=sys.stderr)))(msg)


def _sanitize_namespace(value: str) -> str:
    """Keep the X-Memini-Namespace header value clean: alnum, dot, dash,
    underscore; collapse the rest to dashes and trim. The server sanitizes too,
    but the header should be clean."""
    out = []
    for ch in str(value).strip():
        out.append(ch if (ch.isalnum() or ch in "._-") else "-")
    collapsed = "".join(out)
    while "--" in collapsed:
        collapsed = collapsed.replace("--", "-")
    return collapsed.strip("-")


def _git_out(args: list[str], cwd: str) -> str:
    """Run a git command in cwd, returning trimmed stdout or "" on any error
    (not a repo, git missing, timeout). Best-effort — never raises."""
    try:
        out = subprocess.run(
            ["git", *args],
            cwd=cwd,
            capture_output=True,
            text=True,
            timeout=0.5,
        )
        return out.stdout.strip() if out.returncode == 0 else ""
    except Exception:
        return ""


def _repo_name_from_remote(url: str) -> str:
    """Last path segment of a git remote URL, minus a trailing .git. Handles
    ssh://, https://, and scp-style host:owner/repo URLs. "" on parse failure."""
    cleaned = re.sub(r"\.git$", "", url.strip().rstrip("/"), flags=re.IGNORECASE)
    if not cleaned:
        return ""
    # scp form (host:owner/repo): the segment before the first ":" is the host.
    scp = re.match(r"^[^/:]+:[^/]", cleaned)
    path = cleaned[cleaned.index(":") + 1 :] if scp else cleaned
    segs = [s for s in path.split("/") if s]
    return segs[-1] if segs else ""


def _git_remote(cwd: str) -> str:
    """Best-effort `git remote get-url origin`, raw (unnormalized) -- sent as
    the handshake's project.remote_url fact, and used by
    _derive_local_namespace below as the degraded fallback. "" when not a
    repo, no origin, or git is unavailable/slow (see _git_out)."""
    return _git_out(["remote", "get-url", "origin"], cwd)


def _git_toplevel(cwd: str) -> str:
    """Best-effort `git rev-parse --show-toplevel`, raw absolute path -- sent
    as project.toplevel_path/toplevel_basename, and used by
    _derive_local_namespace below. "" outside a git repo."""
    return _git_out(["rev-parse", "--show-toplevel"], cwd)


def _derive_local_namespace(cwd: str) -> tuple[str, str]:
    """The LOCAL fallback chain, with provenance: git remote repo name > git
    toplevel basename > cwd basename (possibly ""). Used only when
    MEMINI_NAMESPACE is unset and the handshake is unavailable -- see
    _resolve_namespace. Mirrors the node integrations' local derivation so the
    same repo lands in the same namespace everywhere the handshake cannot be
    reached."""
    remote = _git_remote(cwd)
    if remote:
        name = _sanitize_namespace(_repo_name_from_remote(remote))
        if name:
            return name, "remote"
    toplevel = _git_toplevel(cwd)
    if toplevel:
        name = _sanitize_namespace(os.path.basename(toplevel))
        if name:
            return name, "toplevel"
    return _sanitize_namespace(os.path.basename(cwd.rstrip("/"))), "cwd"


def _facts(cwd: str) -> dict:
    """Assemble HandshakeRequest.project (api/openapi.yaml): the cwd basename
    (always present, raw/unsanitized -- the server does its own sanitizing),
    git remote/toplevel (best-effort, also _derive_local_namespace's raw
    material), and MEMINI_NAMESPACE as env_namespace -- sent so a server-side
    pin can still beat it (this client cannot make that call itself without
    knowing whether a pin exists)."""
    facts: dict[str, str] = {"cwd_basename": os.path.basename(cwd.rstrip("/"))}
    remote = _git_remote(cwd)
    if remote:
        facts["remote_url"] = remote
    toplevel = _git_toplevel(cwd)
    if toplevel:
        facts["toplevel_path"] = toplevel
        facts["toplevel_basename"] = os.path.basename(toplevel)
    ns = _env("MEMINI_NAMESPACE")
    if ns:
        facts["env_namespace"] = ns
    return facts


def _handshake(base: str, secret: str, facts: dict) -> dict | None:
    """POST /v1/handshake (api/openapi.yaml) via _api. Fail-soft ALWAYS: any
    network error, non-2xx, or a ~2.5s timeout degrades to None -- _api
    already logs why to stderr, and the caller (_resolve_namespace) falls back
    to _derive_local_namespace / the built-in defaults. No namespace is known
    yet, so the X-Memini-Namespace header is omitted (_api skips it for a
    falsy namespace). Not memoized here -- see _cached_handshake."""
    return _api(
        base,
        "/v1/handshake",
        {"project": facts, "client": {"name": "hermes-memini", "version": _PLUGIN_VERSION}},
        "",
        secret,
        timeout=_HANDSHAKE_TIMEOUT,
    )


# Module-level handshake memo: {"result": dict | None, "expires_at": float}.
# Module-level (not per-instance) because a Hermes process typically hosts one
# MeminiMemoryProvider for one project's lifetime, so this amounts to
# "handshake at most once per ~10 minutes of uptime" -- see _cached_handshake.
_handshake_cache: dict[str, Any] = {}


def _cached_handshake(
    base: str, secret: str, facts: dict, now: Callable[[], float] = time.monotonic
) -> dict | None:
    """Memoized _handshake with a 10-minute TTL (_HANDSHAKE_TTL). `now` is
    injectable so tests can drive expiry without a real 10-minute sleep."""
    t = now()
    cached = _handshake_cache.get("entry")
    if cached and t < cached["expires_at"]:
        return cached["result"]
    result = _handshake(base, secret, facts)
    _handshake_cache["entry"] = {"result": result, "expires_at": t + _HANDSHAKE_TTL}
    return result


def _resolve_namespace(cwd: str, hs: dict | None) -> tuple[str, str]:
    """Resolve the namespace for cwd, with provenance: (namespace, source).

    Order: MEMINI_NAMESPACE (raw-trimmed) > a successful handshake's resolved
    namespace > the local git remote/toplevel/cwd derivation chain.

    MEMINI_NAMESPACE sits above the handshake result on purpose, mirroring
    this provider's pre-handshake precedence: it is this integration's own
    explicit pin, honored as such rather than second-guessed by the server
    (the server still sees it, as project.env_namespace in _facts, so a pin
    can beat it server-side for callers that check in via handshake without
    setting the env var locally). `hs` is the (possibly None -- handshake is
    fail-soft) HandshakeResponse-shaped dict from _cached_handshake, passed in
    rather than fetched here so callers that already resolved it don't
    refetch."""
    if _env("MEMINI_NAMESPACE"):
        # Used raw-trimmed (the server validates the header): flattening "/"
        # here would split a tenant path like work/memini from the other
        # integrations.
        return _env("MEMINI_NAMESPACE"), "env"

    if hs and hs.get("namespace"):
        return str(hs["namespace"]), f"server:{hs.get('namespace_source', '')}"

    ns, source = _derive_local_namespace(cwd)
    return ns, f"local-{source}"


def _valid_url(base: str) -> bool:
    try:
        parsed = urlparse(base)
        _ = parsed.port
    except ValueError:
        return False
    return parsed.scheme in ("http", "https") and bool(parsed.hostname)


def _api(
    base: str,
    path: str,
    body: dict | None,
    namespace: str,
    secret: str,
    method: str = "POST",
    timeout: float | None = None,
) -> dict | None:
    if not _valid_url(base):
        return None
    headers = {"Content-Type": "application/json"}
    # namespace is "" for /v1/handshake (see _handshake): there is no
    # namespace yet to send, and an empty header value would be worse than
    # none.
    if namespace:
        headers["X-Memini-Namespace"] = namespace
    # Deliberate exception to this function's degrade-to-None below: a plaintext-bearer misconfiguration must raise, matching @memini/client's assertBearerTransportSafe.
    _check_plaintext_bearer_guard(base, secret)
    if secret:
        headers["Authorization"] = f"Bearer {secret}"
    home = _home()
    if home:
        headers["X-Memini-Home"] = home
    data = json.dumps(body).encode() if body is not None else None
    req = Request(f"{base}{path}", data=data, headers=headers, method=method)
    try:
        with urlopen(req, timeout=timeout if timeout is not None else _request_timeout()) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else {}
    except (URLError, TimeoutError, ValueError) as e:
        # Degrade gracefully but never silently: a swallowed capture/recall
        # failure looks like "memory isn't working" with nothing to debug.
        print(f"[memini] {method} {path} failed: {e}", file=sys.stderr)
        return None


def _api_result(
    base: str,
    path: str,
    body: dict | None,
    namespace: str,
    secret: str,
    method: str = "POST",
) -> tuple[dict | None, str]:
    """_api without the degrade-to-None: returns (data, error_text).

    The explicit write tool uses it, because a rejected write is information the
    model can act on — a `visibility` naming an unknown ancestor errors listing
    the valid chain, which is how the model learns the topology. Swallowing that
    into a bare "success": false leaves it nothing to correct against. Never
    raises: a tool call must degrade into an answer, not an exception in Hermes.
    """
    if not _valid_url(base):
        return None, f"invalid memini base URL: {base!r}"
    try:
        # MEMINI_REQUIRE_HTTPS=1 makes this raise; catch it here rather than
        # letting a config error surface as a Hermes traceback.
        # Deliberate: the ONE failure here allowed to be loud instead of degrading to (None, message), matching @memini/client's assertBearerTransportSafe.
        _check_plaintext_bearer_guard(base, secret)
        headers = {"Content-Type": "application/json"}
        if namespace:
            headers["X-Memini-Namespace"] = namespace
        if secret:
            headers["Authorization"] = f"Bearer {secret}"
        home = _home()
        if home:
            headers["X-Memini-Home"] = home
        data = json.dumps(body).encode() if body is not None else None
        req = Request(f"{base}{path}", data=data, headers=headers, method=method)
        with urlopen(req, timeout=_request_timeout()) as resp:
            raw = resp.read()
            return (json.loads(raw) if raw else {}), ""
    except HTTPError as e:
        # The 4xx body is the message worth having (it enumerates the valid
        # visibility chain); HTTPError is readable exactly once.
        try:
            detail = e.read().decode("utf-8", "replace").strip()
        except Exception:  # noqa: BLE001 - a body we cannot read is not fatal
            detail = ""
        message = detail or f"HTTP {e.code}"
        print(f"[memini] {method} {path} failed: {message}", file=sys.stderr)
        return None, message
    except (URLError, TimeoutError, ValueError, RuntimeError) as e:
        print(f"[memini] {method} {path} failed: {e}", file=sys.stderr)
        return None, str(e)


def _provenance(mem: dict, from_: Any) -> dict:
    """Render a hit's read-set origin: which namespace it lives in and, for a hit
    off an ancestor/home/link leg, which leg it came from. Both are omitted when
    empty, so a project-only recall carries no "from" noise at all and the model
    reads an absent "from" as "this project's own memory"."""
    out: dict = {}
    if mem.get("namespace"):
        out["namespace"] = mem["namespace"]
    if from_:
        out["from"] = from_
    return out


def _briefing_path(args: dict) -> str:
    """Build the GET /v1/namespaces/briefing query string for memory_briefing.
    The endpoint is header-scoped (X-Memini-Namespace), so there is no namespace
    in the path — the model never names one. An unrecognized scope is dropped
    rather than forwarded: the server 400s on one, and a bad guess must not turn
    orientation into an error."""
    scope = str(args.get("scope") or "").strip()
    if scope not in VALID_SCOPES:
        return "/v1/namespaces/briefing"
    return f"/v1/namespaces/briefing?{urlencode({'scope': scope})}"


def _list_path(args: dict) -> str:
    """Build the GET /v1/memories query string from a memory_list tool call:
    repeatable tier/tag params plus meta=key=value pairs. urlencode escapes the
    '=' inside each meta value, which the server decodes and splits on."""
    params: list[tuple[str, str]] = []
    for t in args.get("tiers") or []:
        params.append(("tier", str(t)))
    for tag in args.get("tags") or []:
        params.append(("tag", str(tag)))
    for key, val in (args.get("metadata") or {}).items():
        params.append(("meta", f"{key}={val}"))
    limit = args.get("limit")
    if isinstance(limit, int) and limit > 0:
        params.append(("limit", str(limit)))
    qs = urlencode(params)
    return f"/v1/memories?{qs}" if qs else "/v1/memories"


# --- Recall shaping -------------------------------------------------------
#
# These mirror the opencode / Claude Code plugins (_shared.mjs) so the recall
# block honors the same env knobs across integrations: a configurable limit, a
# score floor, a token ceiling, and label toggles.


def _truncate_for_capture(text: str, max_chars: int) -> str:
    """Cut `text` to `max_chars` characters, marking the cut; <= 0 keeps it whole.

    Python slices by code point, so unlike the JS clients there is no risk of
    splitting a character here — but the other two traps are shared: 0 must mean
    "uncapped" rather than an empty string, and a cut must be marked. A captured
    turn is recalled into a model's context later, where a silently half-cut
    sentence is indistinguishable from a complete one.
    """
    if not isinstance(max_chars, int) or isinstance(max_chars, bool) or max_chars <= 0:
        # Not a usable cap (None, a bool, a float, a string from a server that
        # disagrees with the schema) means "no cap". Failing open stores the
        # text; the alternative is destroying it this close to the write.
        return text
    if len(text) <= max_chars:
        return text
    return text[:max_chars] + "\n[...truncated]"


def _build_turn_capture(user: str, assistant: str, user_max: int, assistant_max: int) -> str:
    """Assemble a captured turn's body, each side under its own server-resolved bound."""
    return f"{_truncate_for_capture(user, user_max)}\n\n{_truncate_for_capture(assistant, assistant_max)}"


def _int_env(name: str, default: int | None) -> int | None:
    """Parse a non-negative int env var, or return `default` when unset,
    malformed, or negative. `default=None` (used when a server-resolved
    handshake setting should fill the gap instead) means "unset" is
    distinguishable from "explicitly 0"."""
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        n = int(raw)
    except ValueError:
        return default
    return n if n >= 0 else default


def _float_env(name: str, default: float | None) -> float | None:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        n = float(raw)
    except ValueError:
        return default
    return n if n >= 0 else default


# The per-request ceiling actually in force, in milliseconds. Module-level
# because the urlopen call sites are plain functions, not methods on the
# provider: resolved once from the env (so a call made before any handshake is
# still bounded), then refreshed with the server's request_timeout_ms when a
# handshake lands.
_REQUEST_TIMEOUT_MS: int | None = None


def _resolve_request_timeout(settings: dict) -> int:
    """MEMINI_TIMEOUT_MS beats the handshake's request_timeout_ms beats the
    built-in default. A below-floor value is raised to the floor rather than
    swapped for the (much larger) default, so a deliberately tight timeout
    stays tight."""
    env_ms = _int_env("MEMINI_TIMEOUT_MS", None)
    if env_ms is not None:
        return max(MIN_TIMEOUT_MS, env_ms)
    server_ms = settings.get("request_timeout_ms")
    if isinstance(server_ms, int) and server_ms > 0:
        return max(MIN_TIMEOUT_MS, server_ms)
    return DEFAULT_TIMEOUT_MS


def _request_timeout() -> float:
    """The per-request ceiling in SECONDS -- urlopen takes seconds, every knob
    in this repo is expressed in milliseconds."""
    global _REQUEST_TIMEOUT_MS
    if _REQUEST_TIMEOUT_MS is None:
        _REQUEST_TIMEOUT_MS = _resolve_request_timeout({})
    return _REQUEST_TIMEOUT_MS / 1000.0


def _labels_env(name: str = "MEMINI_INJECT_LABELS") -> set[str]:
    """Parse MEMINI_INJECT_LABELS into a set of enabled labels (tier,
    confidence, age). Empty/unset returns an empty set — formatting then emits
    plain bullets, matching the prior output exactly."""
    raw = os.environ.get(name, "")
    return {s.strip().lower() for s in re.split(r"[|,]", raw) if s.strip()}


def _format_age(created_at: Any) -> str:
    if not created_at:
        return ""
    try:
        from datetime import datetime, timezone

        ts = datetime.fromisoformat(str(created_at).replace("Z", "+00:00"))
        days = (datetime.now(timezone.utc) - ts).days
    except (ValueError, TypeError):
        return ""
    if days < 0:
        return ""
    return "today" if days == 0 else f"{days}d"


def _approx_tokens(text: str) -> int:
    """~0.75 tokens/word, floor of 1 for any non-empty line."""
    if not text:
        return 0
    words = len(str(text).split())
    return max(1, (words * 4 + 2) // 3)  # ceil(words * 4 / 3)


def _fit_by_tokens(items: list[str], max_tokens: int) -> tuple[list[str], int]:
    """Trim a list of bullet lines to fit under `max_tokens`, keeping the head
    (most-relevant first). max_tokens <= 0 means unbounded. Returns (kept,
    dropped)."""
    if not items:
        return [], 0
    if max_tokens <= 0:
        return list(items), 0
    out: list[str] = []
    used = dropped = 0
    for s in items:
        t = _approx_tokens(s)
        if used + t > max_tokens:
            dropped += 1
            continue
        out.append(s)
        used += t
    return out, dropped


# --- Injection-enforcement core (hermes copies) ---------------------------
#
# Ported from @memini/client's enforce core (packages/memini-client/src/
# enforce/identity.ts + seen.ts); semantics are pinned by the shared golden
# vectors (packages/memini-client/vectors/enforcement.json), replayed here by
# test_enforcement_vectors.py. hermes ships stdlib-only, so these stay copies,
# not imports -- the vector runner is what keeps them the same functions.

_CONTENT_HASH_RE = re.compile(r"^[0-9a-f]{16}$")


def _is_content_hash(s: Any) -> bool:
    """True for a well-formed server-minted content hash: 16 lowercase hex
    chars (the server's sha256(content||summary)[:16] -- the same recipe as
    the local fallback in _injected_identity, so the two are
    interchangeable)."""
    return isinstance(s, str) and bool(_CONTENT_HASH_RE.match(s))


def _injected_identity(m: Any) -> str:
    """Content-identity hash for the injected-memory state. Prefers the
    server-minted content_hash when present and well-formed -- read off the
    object itself or its nested `memory` (the core's
    `m?.content_hash ?? m?.memory?.content_hash`), because the server hashes
    the FULL content even when it serves a concise form. Falls back to hashing
    the text a recall surface would render (content, falling back to summary)
    for old servers, so an in-place update past any render cap still changes
    identity and re-injects."""
    m = m if isinstance(m, dict) else {}
    ch = m.get("content_hash")
    if ch is None:
        mem = m.get("memory")
        ch = mem.get("content_hash") if isinstance(mem, dict) else None
    if _is_content_hash(ch):
        return ch
    text = m.get("content") or m.get("summary") or ""
    return hashlib.sha256(str(text).encode("utf-8")).hexdigest()[:16]


def _injected_suppressed(
    entry: Any,
    identity: str | None,
    *,
    now: float,
    counter: int,
    cooldown_ms: int,
    cooldown_prompts: int,
) -> bool:
    """The shared windowed-cooldown predicate, core-exact (enforce/seen.ts):

        h == ""                        -> True   sentinel/tool-read: forever
                                                 (hermes never records one --
                                                 kept for vector parity)
        identity and h != identity     -> False  content changed: re-inject
        ms == 0 and prompts == 0       -> True   legacy forever-dedupe (#134)
        else suppressed within EITHER window; re-admit once BOTH lapse.
        counter == 0 leaves the prompt dimension inert (a host that never
        advances a counter degrades to time-only, not forever); negative
        deltas (clock skew / counter regression) clamp to suppressed.

    identity=None is the id-only check (the exclude_ids view): the content-
    change bypass is skipped, so a real-hash entry is judged on the windows
    alone. `now`/`at` are epoch MILLISECONDS, matching the core and the golden
    vectors."""
    if not isinstance(entry, dict):
        return False
    h = entry.get("h")
    if h == "":
        return True
    if identity and h != identity:
        return False
    if cooldown_ms == 0 and cooldown_prompts == 0:
        return True
    prompt_dim = (
        cooldown_prompts > 0
        and counter > 0
        and counter - entry.get("n", 0) < cooldown_prompts
    )
    time_dim = cooldown_ms > 0 and now - entry.get("at", 0) < cooldown_ms
    return prompt_dim or time_dim


# --- Effective settings ---------------------------------------------------
#
# "What is this provider actually doing right now?" A list of values would not
# answer that; the provenance is the feature. The case worth catching is a
# MEMINI_NAMESPACE exported globally (a shell rc, or a fish universal variable),
# set once and forgotten, quietly collapsing every repo on the machine into one
# namespace: the value looks fine, only where it came from gives it away. So the
# namespace is resolved both as-is and without the env pin (the local git/cwd
# derivation), and both are reported.

# Names whose values must never be printed in full. Matches the whole name, not
# a suffix, so MEMINI_API_KEYS_FILE (a path) would not be caught while
# MEMINI_API_KEY is. Mirrors internal/redact and the TS core: redaction is
# always on, never opt-in — a settings dump is the likeliest place for a token
# to be pasted into an issue.
_SENSITIVE = re.compile(
    r"(^|_)(KEY|TOKEN|SECRET|PASSWORD|PASS|BEARER|DSN|CREDENTIALS?)$", re.IGNORECASE
)

# Every env var this provider reads, with the value it falls back to when unset.
# Explicit rather than scraped from os.environ: "MEMINI_HOME is unset" is itself
# a finding, so a knob nobody set must still show up, with its default.
CLIENT_KNOBS: tuple[tuple[str, str], ...] = (
    ("MEMINI_BASE_URL", DEFAULT_BASE_URL),
    ("MEMINI_API_KEY", ""),
    ("MEMINI_REQUIRE_HTTPS", "0"),
    ("MEMINI_NAMESPACE", "(auto: handshake/git/cwd)"),
    ("MEMINI_HOME", ""),
    ("MEMINI_RECALL_LIMIT", "3"),
    ("MEMINI_INJECT_RECALL_MIN_SCORE", "0"),
    ("MEMINI_INJECT_RECALL_MAX_TOK", "uncapped"),
    ("MEMINI_INJECT_LABELS", ""),
    ("MEMINI_TIMEOUT_MS", str(DEFAULT_TIMEOUT_MS)),
)


def _is_sensitive(name: str) -> bool:
    return bool(_SENSITIVE.search(name))


def _redact(value: str) -> str:
    """A recognizable-but-useless fingerprint: enough to tell two tokens apart
    and confirm the one you set is the one in use, not enough to use. Short
    values are elided entirely — a 6-char secret showing 3 leading and 4
    trailing characters would be no secret at all."""
    if not value:
        return ""
    return "***" if len(value) <= 12 else f"{value[:3]}…{value[-4:]}"


def _describe_settings(cwd: str, in_use: str = "") -> dict:
    """Effective settings with provenance: the namespace resolution (env, a
    live -- possibly memoized -- handshake, or local git/cwd derivation), each
    knob and where it came from (secrets redacted), and the warnings."""
    base = _base_url()
    secret = _secret()
    hs = _cached_handshake(base, secret, _facts(cwd))
    effective, source = _resolve_namespace(cwd, hs)
    derived_ns, derived_source = _derive_local_namespace(cwd)

    settings = []
    for name, default in CLIENT_KNOBS:
        raw = _env(name)
        if raw:
            value = _redact(raw) if _is_sensitive(name) else raw
        else:
            value = default or "(unset)"
        settings.append({"name": name, "value": value, "source": "env" if raw else "default"})

    warnings: list[dict] = []

    # The finding this whole report exists for.
    pin = _env("MEMINI_NAMESPACE")
    if pin and derived_ns and derived_ns != pin:
        warnings.append(
            {
                "level": "warn",
                "code": "global-namespace-pin",
                "message": (
                    f'MEMINI_NAMESPACE is set to "{pin}", which pins EVERY project on this machine '
                    f'to one namespace. This project would otherwise resolve to "{derived_ns}" (via '
                    "git/cwd, absent the pin). If it is exported from a shell rc (or a fish universal "
                    "variable), every repo you work in is sharing one memory pool."
                ),
                "fix": "Unset MEMINI_NAMESPACE and let each project resolve on its own.",
            }
        )

    # A handshake result (or anything else) only reaches the wire on the next
    # session: the namespace is resolved once, in initialize.
    if in_use and in_use != effective:
        warnings.append(
            {
                "level": "warn",
                "code": "restart-required",
                "message": (
                    f'this session is writing to and recalling from "{in_use}", but the settings '
                    f'now resolve to "{effective}" — the namespace was resolved when the session '
                    "started."
                ),
                "fix": "Restart hermes to pick it up.",
            }
        )

    if _uses_plaintext_bearer_auth(base, secret):
        warnings.append(
            {
                "level": "warn",
                "code": "plaintext-bearer",
                "message": (
                    f"a bearer token is configured for plaintext HTTP to {base}; the token and "
                    "your memory payloads can be observed on the network."
                ),
                "fix": "Use HTTPS, or tunnel over SSH. Set MEMINI_REQUIRE_HTTPS=1 to make this an "
                "error.",
            }
        )

    if not _home():
        warnings.append(
            {
                "level": "note",
                "code": "home-unset",
                "message": "MEMINI_HOME is unset: no personal leg merges into recall.",
                "fix": "Export MEMINI_HOME=personal/<you>.",
            }
        )

    return {
        "cwd": cwd,
        "project": cwd,
        "namespace": {
            "effective": effective,
            "source": source,
            "in_use": in_use or effective,
            "derived": {"namespace": derived_ns, "source": f"local-{derived_source}"},
            "home": _home(),
        },
        "settings": settings,
        "warnings": warnings,
    }


def _render_settings(report: dict) -> str:
    """Render the report as text. Plain text, not the JSON the other tools
    return: this one exists to be read by a human, and a JSON blob would only be
    re-rendered by the model — badly, and with the redaction re-litigated."""
    ns = report["namespace"]
    lines = [
        "memini — effective settings (hermes)",
        f"project: {report['project']}",
        "",
        "NAMESPACE",
        f"  {'effective':<26} {ns['effective']:<30} <- {ns['source']}",
    ]
    if ns["derived"]["namespace"] != ns["effective"]:
        d = ns["derived"]
        lines.append(f"  {'git/cwd would give':<26} {d['namespace']:<30} <- {d['source']}")
    if ns["in_use"] != ns["effective"]:
        lines.append(f"  {'this session is using':<26} {ns['in_use']:<30} (resolved at startup)")
    lines.append(f"  {'home (personal)':<26} {ns['home'] or '(unset)'}")
    lines.append("")

    lines.append("SETTINGS")
    for s in report["settings"]:
        origin = "<- env" if s["source"] == "env" else "(default)"
        name = s["name"].removeprefix("MEMINI_").lower()
        lines.append(f"  {name:<26} {s['value']:<30} {origin}")
    lines.append("")

    if report["warnings"]:
        lines.append("WARNINGS")
        for w in report["warnings"]:
            lines.append(f"  [{'!' if w['level'] == 'warn' else 'i'}] {w['code']}: {w['message']}")
            if w.get("fix"):
                lines.append(f"      fix: {w['fix']}")
    else:
        lines.append("No problems detected.")
    return "\n".join(lines)


class MeminiMemoryProvider(MemoryProvider):
    """Cross-session memory backed by a memini service."""

    @property
    def name(self) -> str:
        return "memini"

    def is_available(self) -> bool:
        return _valid_url(_base_url())

    def initialize(self, session_id: str, **kwargs: Any) -> None:
        self._base = _base_url().rstrip("/")
        self._secret = _secret()
        self._session_id = session_id
        # Insertion-ordered set of memory ids already injected into this
        # conversation's context — by prefetch or pulled explicitly through the
        # recall/briefing tools. Later prefetches exclude them (server-side via
        # exclude_ids, client-side as belt-and-braces) so the recall limit is
        # spent on memories the context does not already carry. Instance-scoped:
        # the provider lives exactly as long as the conversation it serves.
        # Values are the enforce core's {"h": content-identity hash, "at":
        # epoch_ms, "n": prefetch_counter} entries stamped at each
        # (re-)injection: the windowed cooldown predicate (_suppressed) uses
        # them to decide whether an id is still fresh enough to exclude, has
        # lapsed and may be re-served, or was UPDATED in place (h mismatch)
        # and must re-inject immediately.
        self._injected_ids: dict = {}
        # Per-user-turn counter: bumped once at the top of every prefetch()
        # (the per-turn unit here), it drives the cooldown's prompt dimension.
        # Reset with _injected_ids on pre-compress.
        self._prefetch_n = 0
        # Latched on the first successful retry-without-exclude_ids: an older
        # server 400s the whole request on the unknown field, and "no dedupe"
        # beats "no recall at all".
        self._exclude_ids_unsupported = False
        # Same latch for the composite floor: an older server 400s min_rank_score
        # too, so once a retry-without-it lands, apply the floor client-side for
        # the rest of the conversation ("no server floor" beats "no recall").
        self._min_rank_score_unsupported = False
        # Hermes' initialize kwargs carry no project path (agent_workspace is a
        # label, not a dir), so the working directory is the only signal for the
        # default namespace; set MEMINI_NAMESPACE to scope explicitly. Order:
        # MEMINI_NAMESPACE > a handshake resolved by the server > local git/cwd
        # derivation. The handshake call (memoized, 10-min TTL, fail-soft) also
        # runs the plaintext-bearer guard via _api, so a MEMINI_REQUIRE_HTTPS=1
        # misconfiguration still surfaces at startup without a separate check.
        self._cwd = os.getcwd()
        hs = _cached_handshake(self._base, self._secret, _facts(self._cwd))
        self._handshake = hs
        ns, source = _resolve_namespace(self._cwd, hs)
        self._namespace = ns or "hermes"
        self._namespace_source = source if ns else "local-default"

        # Recall-shaping knobs: MEMINI_* env beats the handshake's resolved
        # ClientSettings beats the built-in default (limit 3, no floor,
        # unbounded, both recall/capture on). There is no local "option" tier
        # here, unlike opencode/openwebui -- hermes has no config object.
        settings = (hs or {}).get("settings") or {}

        # The request ceiling is global (the urlopen helpers are module-level),
        # so refresh it here rather than storing it on the provider.
        global _REQUEST_TIMEOUT_MS
        _REQUEST_TIMEOUT_MS = _resolve_request_timeout(settings)

        env_limit = _int_env("MEMINI_RECALL_LIMIT", None)
        if env_limit is not None:
            self._recall_limit = env_limit if env_limit > 0 else 3
        else:
            server_limit = settings.get("recall_limit")
            self._recall_limit = (
                server_limit if isinstance(server_limit, int) and server_limit > 0 else 3
            )

        env_min_score = _float_env("MEMINI_INJECT_RECALL_MIN_SCORE", None)
        if env_min_score is not None:
            self._recall_min_score = env_min_score
        else:
            server_min_score = settings.get("inject_recall_min_score")
            self._recall_min_score = (
                server_min_score if isinstance(server_min_score, (int, float)) else 0.0
            )

        env_max_tok = _int_env("MEMINI_INJECT_RECALL_MAX_TOK", None)
        if env_max_tok is not None:
            self._recall_max_tokens = env_max_tok
        else:
            server_max_tok = settings.get("inject_recall_max_tok")
            self._recall_max_tokens = server_max_tok if isinstance(server_max_tok, int) else 0

        # Windowed injection cooldown (env > handshake > default, kind int,
        # min 0). ms is the time window; prompts the per-turn window. Either
        # dimension is disabled by setting it to 0; BOTH zero == the legacy
        # "suppress forever" behavior. See _suppressed for the predicate.
        env_cooldown_ms = _int_env("MEMINI_INJECT_COOLDOWN_MS", None)
        if env_cooldown_ms is not None:
            self._inject_cooldown_ms = env_cooldown_ms
        else:
            server_cooldown_ms = settings.get("inject_cooldown_ms")
            self._inject_cooldown_ms = (
                server_cooldown_ms
                if isinstance(server_cooldown_ms, int) and server_cooldown_ms >= 0
                else 1800000
            )

        env_cooldown_prompts = _int_env("MEMINI_INJECT_COOLDOWN_PROMPTS", None)
        if env_cooldown_prompts is not None:
            self._inject_cooldown_prompts = env_cooldown_prompts
        else:
            server_cooldown_prompts = settings.get("inject_cooldown_prompts")
            self._inject_cooldown_prompts = (
                server_cooldown_prompts
                if isinstance(server_cooldown_prompts, int) and server_cooldown_prompts >= 0
                else 3
            )

        # Capture bounds: how much of a turn is worth keeping is the server's
        # call (its store and recall budget), so the handshake carries it; the
        # env vars stay as a per-caller escape hatch, matching the knobs above.
        for attr, env_name, wire_key, default in (
            ("_capture_user_max_chars", "MEMINI_CAPTURE_USER_MAX_CHARS", "capture_user_max_chars", 1000),
            (
                "_capture_assistant_max_chars",
                "MEMINI_CAPTURE_ASSISTANT_MAX_CHARS",
                "capture_assistant_max_chars",
                3000,
            ),
        ):
            env_val = _int_env(env_name, None)
            if env_val is not None:
                setattr(self, attr, env_val)
            else:
                server_val = settings.get(wire_key)
                setattr(self, attr, server_val if isinstance(server_val, int) and server_val >= 0 else default)

        # recall/capture: no local env toggle (see the module docstring) --
        # only the server's handshake settings can turn these off, else on.
        self._recall_enabled = (
            settings["recall"] if isinstance(settings.get("recall"), bool) else True
        )
        self._capture_enabled = (
            settings["capture"] if isinstance(settings.get("capture"), bool) else True
        )

        self._labels = _labels_env()

    def _call(self, path: str, body: dict | None, method: str = "POST") -> dict | None:
        return _api(self._base, path, body, self._namespace, self._secret, method)

    def _call_result(
        self, path: str, body: dict | None, method: str = "POST"
    ) -> tuple[dict | None, str]:
        """_call without the degrade-to-None — see _api_result. Used by the
        explicit write tool so a rejected write reaches the model with the
        server's own error text."""
        return _api_result(self._base, path, body, self._namespace, self._secret, method)

    def _call_bg(self, path: str, body: dict) -> None:
        threading.Thread(target=self._call, args=(path, body), daemon=True).start()

    def get_config_schema(self) -> list[dict]:
        return [
            {
                "key": "url",
                "description": "memini server URL",
                "default": DEFAULT_BASE_URL,
                "env_var": "MEMINI_BASE_URL",
            },
            {
                "key": "namespace",
                "description": "memory namespace (tenant)",
                "required": False,
                "env_var": "MEMINI_NAMESPACE",
            },
            {
                "key": "secret",
                "description": "memini bearer token (optional)",
                "secret": True,
                "required": False,
                "env_var": "MEMINI_API_KEY",
            },
        ]

    def save_config(self, values: dict, hermes_home: str) -> None:
        # Never persist secret-typed config (the bearer token) to disk. The
        # token is read from the MEMINI_API_KEY env var at runtime (see
        # initialize), not from this file, so writing it would leave a
        # plaintext credential on disk for no functional benefit.
        secret_keys = {c["key"] for c in self.get_config_schema() if c.get("secret")}
        safe = {k: v for k, v in values.items() if k not in secret_keys}
        (Path(hermes_home) / "memini.json").write_text(json.dumps(safe, indent=2))

    def _format_lines(self, result: dict | None) -> list[str]:
        """Render hits to bullet lines. Plain `- text` when no labels are
        enabled (the default); `- [tier · conf=X · age] text` otherwise."""
        if not result:
            return []
        labels = self._labels
        # Client composite floor is a fallback ONLY: it applies when the knob was
        # clamped to client-only (>= 1) or an older server latched min_rank_score
        # off — not when the server enforced the floor (its result set is then
        # authoritative and is NOT re-filtered here).
        server_enforced = 0 < self._recall_min_score < 1 and not self._min_rank_score_unsupported
        floor = self._recall_min_score if self._recall_min_score > 0 and not server_enforced else 0
        lines: list[str] = []
        for r in (result.get("results") or [])[: self._recall_limit]:
            if floor > 0 and (r.get("score") or 0) < floor:
                continue
            mem = r.get("memory") or {}
            text = (mem.get("summary") or mem.get("content") or "").strip()[:300]
            if not text:
                continue
            if not labels:
                lines.append(f"- {text}")
                continue
            tags: list[str] = []
            if "tier" in labels and mem.get("tier"):
                tags.append(str(mem["tier"]))
            if "confidence" in labels and isinstance(
                mem.get("confidence"), (int, float)
            ):
                tags.append(f"conf={mem['confidence']:.2f}")
            if "age" in labels:
                age = _format_age(mem.get("created_at"))
                if age:
                    tags.append(age)
            lines.append(f"- [{' · '.join(tags)}] {text}" if tags else f"- {text}")
        return lines

    def _recall_block(self, result: dict | None, header: str) -> str:
        """Format hits, fit under the token ceiling, and add a footer when the
        tail was dropped. Returns "" when there is nothing to show."""
        lines = self._format_lines(result)
        kept, dropped = _fit_by_tokens(lines, self._recall_max_tokens)
        if not kept:
            return ""
        block = "\n".join(kept)
        if dropped > 0:
            block += f"\n[... {dropped} item(s) truncated by token budget]"
        # /v1/search sets degraded="keyword_only" (plus a note) when the query
        # embed was unavailable and it fell back to keyword-only matching;
        # both are already on `result`, so surfacing them is a one-line addition.
        if result and result.get("degraded"):
            note = (
                result.get("note")
                or "semantic search unavailable — results are keyword-only and may be incomplete"
            )
            block += f"\n[memini: {note}]"
        return f"{header}\n{block}"

    def _recall_body(self, query: str, exclude_injected: bool = True) -> dict:
        # Exclude this session's own captured turns: they're still in the live
        # transcript, so recalling them just echoes the conversation back a turn
        # behind. Captures from other (past) sessions are still recalled.
        body: dict = {"query": query, "limit": self._recall_limit}
        # inject_recall_min_score floors the FINAL composite score server-side
        # via min_rank_score (not the fused-scale min_score), matching the Claude
        # Code plugin. The server rejects >= 1 as out of range, so a mis-set knob
        # clamps to a client-only floor; and once an older server 400s the field
        # it latches off and the floor is applied client-side (_format_lines).
        if 0 < self._recall_min_score < 1 and not self._min_rank_score_unsupported:
            body["min_rank_score"] = self._recall_min_score
        if self._session_id:
            body["exclude_metadata"] = {"session_id": self._session_id}
        # And exclude what this conversation already carries — memories a prior
        # prefetch injected or the model pulled via the recall/briefing tools —
        # so the limit is spent on hits the context does not yet hold. Only
        # IN-COOLDOWN ids are sent: a lapsed id is intentionally re-servable.
        # Capped at the server's exclude_ids limit, most recent kept.
        if exclude_injected and self._injected_ids and not self._exclude_ids_unsupported:
            now = time.time() * 1000.0
            ids = [mid for mid, e in self._injected_ids.items() if self._suppressed(e, now)]
            if ids:
                body["exclude_ids"] = ids[-512:]
        return body

    def _record_injected(self, mems: Any) -> None:
        """Mark memories as present in this conversation's context, stamping
        each id with its content identity (_injected_identity), the wall-clock
        ms, and the prompt counter at injection so the windowed cooldown
        (_suppressed) can later decide re-admission -- and so an in-place
        update (h mismatch) can bypass it. `mems` is an iterable of wire
        memory dicts (each carrying at least an id). A re-record refreshes the
        stamp (and, via pop/re-insert, the LRU spot)."""
        now = time.time() * 1000.0
        for mem in mems:
            if not isinstance(mem, dict):
                continue
            mid = mem.get("id")
            if isinstance(mid, str) and mid:
                self._injected_ids.pop(mid, None)  # refresh LRU position
                self._injected_ids[mid] = {
                    "h": _injected_identity(mem),
                    "at": now,
                    "n": self._prefetch_n,
                }
        while len(self._injected_ids) > 512:
            self._injected_ids.pop(next(iter(self._injected_ids)))

    def _suppressed(self, entry: dict, now: float, identity: str | None = None) -> bool:
        """_injected_suppressed (the core-exact module predicate) bound to this
        provider's live knobs and prompt counter. `now` is epoch MILLISECONDS.

        identity=None is the id-only view used for exclude_ids (the wire
        cannot know what content the server would serve); the per-hit drop
        filter (_drop_injected) passes each hit's real identity so an
        in-place UPDATE bypasses the window and re-injects. hermes never
        records a sentinel (h="") entry -- tool results here carry full
        content, so identity is always knowable and nothing is immortal."""
        return _injected_suppressed(
            entry,
            identity,
            now=now,
            counter=self._prefetch_n,
            cooldown_ms=self._inject_cooldown_ms,
            cooldown_prompts=self._inject_cooldown_prompts,
        )

    def _drop_injected(self, result: dict | None) -> dict | None:
        """Belt-and-braces for the server-side exclude_ids (absent on the
        old-server fallback path): drop hits that are still IN COOLDOWN,
        judged per hit against its content identity. A lapsed id is NOT
        dropped — it passes through so prefetch re-serves and re-records it
        (Gap-5) — and neither is a hit whose content CHANGED since injection
        (h mismatch): the fresh content re-injects immediately."""
        if not result or not self._injected_ids:
            return result
        hits = result.get("results") or []
        now = time.time() * 1000.0
        kept = []
        for r in hits:
            mem = (r or {}).get("memory") or {}
            entry = self._injected_ids.get(mem.get("id"))
            if entry and self._suppressed(entry, now, _injected_identity(mem)):
                continue
            kept.append(r)
        if len(kept) == len(hits):
            return result
        out = dict(result)
        out["results"] = kept
        return out

    def prefetch(self, query: str, **kwargs: Any) -> str:
        # Advance the per-user-turn counter before any gate (shape or recall):
        # each prefetch() is one turn, and the cooldown's prompt dimension
        # measures turns-since-injection even on turns that inject nothing —
        # mirrors the plugin's UserPromptSubmit counter.
        self._prefetch_n += 1
        # recall is on by default; only a server-resolved handshake setting
        # can turn it off (see initialize) -- there is no local env toggle.
        if not self._recall_enabled or not query.strip():
            return ""
        body = self._recall_body(query)
        result = self._call("/v1/search", body)
        # Newer-than-server optional fields (min_rank_score, exclude_ids): an
        # older server 400s the whole request on the unknown field, and _call
        # degrades that to None — indistinguishable from a dead server here, so
        # retry ONCE with BOTH fields stripped (matching _shared.mjs's combined
        # strip). If THAT lands, a newer field was the problem: latch each field
        # that this request actually carried ("no floor / no dedupe" beats "no
        # recall at all"); a dead server fails both calls and latches nothing.
        # Gate strictly on what THIS body carried: the windowed cooldown means a
        # non-empty _injected_ids no longer implies exclude_ids was sent (lapsed
        # ids omit it), and a clamped/latched floor omits min_rank_score, so a
        # byte-identical retry would latch spuriously and forever.
        sent_exclude_ids = "exclude_ids" in body
        sent_rank_floor = "min_rank_score" in body
        if result is None and (sent_exclude_ids or sent_rank_floor):
            retry_body = {k: v for k, v in body.items() if k not in ("min_rank_score", "exclude_ids")}
            result = self._call("/v1/search", retry_body)
            if result is not None:
                if sent_exclude_ids:
                    self._exclude_ids_unsupported = True
                if sent_rank_floor:
                    self._min_rank_score_unsupported = True
        result = self._drop_injected(result)
        block = self._recall_block(result, "Relevant memories (from memini):")
        if block:
            self._record_injected(r.get("memory") or {} for r in (result or {}).get("results") or [])
        return block

    def on_pre_compress(self, messages: list, **kwargs: Any) -> str:
        """Re-inject recalled context before history compaction."""
        if not self._recall_enabled:
            return ""
        query = ""
        for m in reversed(messages):
            role = m.get("role") if isinstance(m, dict) else getattr(m, "role", None)
            if role != "user":
                continue
            content = (
                m.get("content") if isinstance(m, dict) else getattr(m, "content", None)
            )
            if isinstance(content, str) and content.strip():
                query = content.strip()
                break
        if not query:
            return ""
        # Compression rebuilds the context: everything previously injected is
        # about to be summarized away, so this re-injection must NOT exclude
        # what the conversation already saw — surfacing it again is the whole
        # point — and the dedupe state (ids AND the prompt counter they were
        # stamped against) resets with the context it described.
        self._injected_ids.clear()
        self._prefetch_n = 0
        result = self._call("/v1/search", self._recall_body(query, exclude_injected=False))
        block = self._recall_block(result, "[memini context before compaction]")
        if block:
            self._record_injected(r.get("memory") or {} for r in (result or {}).get("results") or [])
        return block

    def sync_turn(self, user: str, assistant: str, **kwargs: Any) -> None:
        # capture is on by default; only a server-resolved handshake setting
        # can turn it off -- there is no local env toggle.
        if not self._capture_enabled:
            return
        user, assistant = (user or "").strip(), (assistant or "").strip()
        if not user and not assistant:
            return
        # Adopt a host-passed per-turn session id: recall's exclude_metadata
        # keys on self._session_id, so capture and exclusion must resolve the
        # same value — a mismatch means this session's own turns are never
        # excluded and echo back on the next recall.
        sid = kwargs.get("session_id") or self._session_id
        self._session_id = sid
        if not sid:
            # A capture without a session id can never be excluded; storing it
            # would echo this session's turns back forever.
            return
        self._call_bg(
            "/v1/memories",
            {
                "content": _build_turn_capture(
                    user, assistant, self._capture_user_max_chars, self._capture_assistant_max_chars
                ),
                "tags": ["hermes"],
                "metadata": {"source": "hermes", "session_id": sid, "format": "turn"},
            },
        )

    def on_memory_write(
        self, action: str, target: str, content: str, **kwargs: Any
    ) -> None:
        # Hermes emits action ∈ {add, replace, remove}; mirror those that add
        # content. Tier is omitted so the server classifies it (a decision or
        # preference lands durable, chatter stays episodic) rather than forcing
        # everything to semantic.
        if not self._capture_enabled:
            return
        if action in ("add", "replace") and content.strip():
            # Sent whole. This used to cut at 4000 characters, which had no
            # counterpart anywhere: the server stores content as unbounded text,
            # and the only real ceiling is its 4MB request body. Silently losing
            # the tail of a memory the caller asked to store is worse than a
            # large write.
            self._call_bg("/v1/memories", {"content": content.strip()})

    def get_tool_schemas(self) -> list[dict]:
        return [
            {
                "name": "memory_recall",
                "description": "Search prior context in long-term memory (memini) via hybrid (semantic + "
                "keyword) retrieval, ranked by relevance, recency, and corroboration. Call "
                "BEFORE starting work that may have history: editing an unfamiliar file, "
                "debugging a recurring issue, making a non-obvious decision, or when asked "
                "what's known about something. scope picks how wide to read: 'project' (just "
                "this project), 'full' (default: project plus inherited ancestor/personal/link "
                "context), or 'everywhere' (full plus nested sub-projects). Each result's "
                "namespace/from fields are provenance, not a choice — an absent 'from' means "
                "this project's own memory, otherwise it names the ancestor or personal "
                "namespace the memory came from; read them to learn where knowledge lives, "
                "never construct a namespace path. Empty results mean nothing is known — "
                "proceed from first principles, never invent a remembered fact. A "
                'degraded:"keyword_only" field in the result means semantic search was '
                "unavailable and results came from keyword matching alone — treat as "
                "incomplete, not exhaustive.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {
                            "type": "string",
                            "description": "What to search for",
                        },
                        "limit": {
                            "type": "integer",
                            "description": "Max results",
                            "default": 3,
                        },
                        "tags": {
                            "type": "array",
                            "items": {"type": "string"},
                            "description": "Restrict to memories carrying every listed tag (AND).",
                        },
                        "metadata": {
                            "type": "object",
                            "additionalProperties": {"type": "string"},
                            "description": "Restrict to memories whose metadata contains each "
                            'key=value pair, e.g. {"category": "bug_fixes"}.',
                        },
                        "scope": {
                            "type": "string",
                            "enum": list(VALID_SCOPES),
                            "default": "full",
                            "description": "How wide to read: 'project' = just this project's own "
                            "memories; 'full' (default) = project plus inherited context "
                            "(ancestors, your personal namespace, links); 'everywhere' = full "
                            "plus nested sub-projects.",
                        },
                    },
                    "required": ["query"],
                },
            },
            {
                "name": "memory_briefing",
                "description": "Layered session-start briefing for this project from long-term memory "
                "(memini) — pinned context, durable facts, how-to procedures, and recent "
                "activity — in one query-less call. Call it when a session opens to orient "
                "yourself; prefer it over broad recall queries at session start. The "
                "scope_header line ('Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← "
                "personal(2)') spells out the ancestor chain you inherit from — read it "
                "instead of guessing namespace paths, and name one of those ancestors as "
                "memory_remember's visibility to share a fact up that chain. "
                "scope='everywhere' also briefs nested sub-projects.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "scope": {
                            "type": "string",
                            "enum": list(VALID_SCOPES),
                            "default": "full",
                            "description": "How wide to brief: 'project' = just this project's own "
                            "memories; 'full' (default) = project plus inherited context "
                            "(ancestors, your personal namespace, links); 'everywhere' = full "
                            "plus nested sub-projects.",
                        },
                    },
                },
            },
            {
                "name": "memory_list",
                "description": "Browse long-term memory (memini) without a query — filter by tier, "
                "tags, or metadata category (e.g. all procedural memories, or "
                "everything categorized bug_fixes). Newest first.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "tiers": {
                            "type": "array",
                            "items": {"type": "string", "enum": list(VALID_TIERS)},
                            "description": "Restrict to these tiers; empty means all.",
                        },
                        "tags": {
                            "type": "array",
                            "items": {"type": "string"},
                            "description": "Restrict to memories carrying every listed tag (AND).",
                        },
                        "metadata": {
                            "type": "object",
                            "additionalProperties": {"type": "string"},
                            "description": "Restrict to memories whose metadata contains each key=value pair.",
                        },
                        "limit": {
                            "type": "integer",
                            "description": "Max results (0 = all)",
                            "default": 20,
                        },
                    },
                },
            },
            {
                "name": "memory_remember",
                "description": "Store a durable fact, decision, or preference in long-term memory (memini). "
                "Call proactively when the user says 'remember this', after an architectural "
                "decision (capture the why), or after discovering a non-obvious bug or "
                "convention. Keep memories atomic — one self-contained fact per call. Don't "
                "store what's already in project docs or trivially recoverable from code. To "
                "correct an existing memory, pass its id — the write updates it in place. "
                "visibility decides who should know: 'project' (default) keeps it here; "
                "'personal' follows the user everywhere; or name an ancestor from the "
                "memory_briefing Scope line to share it up that chain. reinforced=true in the "
                "result means the fact was ALREADY KNOWN: no new memory was created, the "
                "existing one was strengthened, and `id` names that pre-existing memory rather "
                "than anything you just wrote — do not report it to the user as a new save.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "content": {
                            "type": "string",
                            "description": "The fact to remember — atomic and self-contained.",
                        },
                        "id": {
                            "type": "string",
                            "description": "Existing memory id (from memory_recall / memory_list) "
                            "to correct in place instead of writing a new memory.",
                        },
                        "tier": {
                            "type": "string",
                            "enum": list(VALID_TIERS),
                            "description": "semantic=durable knowledge, procedural=how-to, "
                            "episodic=what happened, working=transient "
                            "(omit to let the server classify from the content)",
                        },
                        "tags": {
                            "type": "array",
                            "items": {"type": "string"},
                            "description": "Topic keywords for later search/filtering; tag a "
                            "critical always-relevant fact 'pinned'.",
                        },
                        "category": {
                            "type": "string",
                            "description": "Optional topic bucket stored as metadata.category "
                            "(e.g. bug_fixes, architecture_decisions, coding_conventions) "
                            "so the memory can be browsed by subject later.",
                        },
                        "visibility": {
                            "type": "string",
                            "description": "Who should remember this: 'project' (default, this project "
                            "only), 'personal' (about the user, follows them everywhere), or an "
                            "ancestor namespace name read off the memory_briefing Scope line "
                            "(e.g. the team or org level) to share it up that chain. On a durable "
                            "write an unrecognized name errors listing the valid options. "
                            "Episodic/working writes always stay in the project regardless.",
                        },
                    },
                    "required": ["content"],
                },
            },
            {
                "name": "memory_status",
                "description": "Show the memini settings in force for this session: which namespace memories "
                "are written to and recalled from, where that namespace came from (MEMINI_NAMESPACE, "
                "a server-resolved handshake, or the working directory), what it would be without "
                "the env pin, and any misconfiguration worth flagging. Read-only, and secrets are "
                "redacted. Call it when the user asks what memini is doing, which namespace is in "
                "use, or why something saved earlier cannot be recalled — a namespace mismatch is "
                "the usual cause.",
                "parameters": {"type": "object", "properties": {}},
            },
            {
                "name": "memory_forget",
                "description": "Permanently delete a memory from long-term memory (memini) by its id — use when "
                "a recalled memory is wrong, outdated, or poisoned. Get the id from memory_recall "
                "or memory_list. To correct a fact instead, call memory_remember with the "
                "existing id (it updates in place, preserving history); forget only memories "
                "that should not exist at all.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "id": {
                            "type": "string",
                            "description": "The id of the memory to forget (from memory_recall / memory_list).",
                        },
                    },
                    "required": ["id"],
                },
            },
        ]

    def handle_tool_call(self, name: str, args: dict, **kwargs: Any) -> str:
        if name == "memory_status":
            # Re-resolved live rather than read off self._namespace, so a
            # handshake result that changed mid-session shows up — and the gap
            # between what this session is using and what now resolves is
            # reported as a warning instead of being papered over.
            return _render_settings(
                _describe_settings(
                    getattr(self, "_cwd", None) or os.getcwd(),
                    in_use=getattr(self, "_namespace", ""),
                )
            )

        if name == "memory_recall":
            body = {"query": args["query"], "limit": args.get("limit", 3)}
            if args.get("tags"):
                body["tags"] = args["tags"]
            if args.get("metadata"):
                body["metadata"] = args["metadata"]
            # An unrecognized scope is dropped rather than forwarded: /v1/search
            # 400s on one, and a hallucinated value must not turn a recall into
            # an error.
            if args.get("scope") in VALID_SCOPES:
                body["scope"] = args["scope"]
            result = self._call("/v1/search", body)
            items = []
            raw_mems = []
            for r in (result or {}).get("results", []):
                mem = r.get("memory") or {}
                raw_mems.append(mem)
                item = {
                    "id": mem.get("id", ""),
                    "content": mem.get("content", ""),
                    "summary": mem.get("summary", ""),
                    "tier": mem.get("tier", ""),
                    "score": r.get("score", 0),
                }
                item.update(_provenance(mem, r.get("from")))
                items.append(item)
            # What the model just pulled explicitly is now in the transcript:
            # record it (with real content identity — tool results carry full
            # content, never a truncation sentinel) so prefetch stops
            # re-injecting it. (Tool results are never filtered — an explicit
            # query shows everything it found.)
            self._record_injected(raw_mems)
            # /v1/search already carries degraded/note on `result`; pass them
            # through rather than dropping them silently.
            out = {"results": items}
            if (result or {}).get("degraded"):
                out["degraded"] = result["degraded"]
                if result.get("note"):
                    out["note"] = result["note"]
            return json.dumps(out)

        if name == "memory_briefing":
            result = self._call(_briefing_path(args), None, method="GET")
            if result is None:
                return json.dumps({"briefing": None, "error": "memini unavailable"})

            def section(items: list | None) -> list:
                out = []
                raw_mems = []
                for b in items or []:
                    mem = b.get("memory") or {}
                    raw_mems.append(mem)
                    item = {
                        "id": mem.get("id", ""),
                        "content": mem.get("content", ""),
                        "tier": mem.get("tier", ""),
                    }
                    item.update(_provenance(mem, b.get("from")))
                    out.append(item)
                # The briefing's memories are now in the transcript: record
                # them (with real content identity) so prefetch stops
                # re-injecting what the model already oriented on.
                self._record_injected(raw_mems)
                return out

            return json.dumps(
                {
                    "namespace": result.get("namespace", ""),
                    "scope_header": result.get("scope_header", ""),
                    "pinned": section(result.get("pinned")),
                    "facts": section(result.get("facts")),
                    "procedures": section(result.get("procedures")),
                    "recent": section(result.get("recent")),
                }
            )

        if name == "memory_list":
            result = self._call(_list_path(args), None, method="GET")
            items = []
            for mem in (result or {}).get("memories", []):
                items.append(
                    {
                        "id": mem.get("id", ""),
                        "content": mem.get("content", ""),
                        "summary": mem.get("summary", ""),
                        "tier": mem.get("tier", ""),
                        "tags": mem.get("tags", []),
                        "metadata": mem.get("metadata", {}),
                    }
                )
            return json.dumps({"memories": items})

        if name == "memory_remember":
            # No client-side tier default: send tier only when the caller gave a
            # valid one, else omit it so the server classifies the content.
            body: dict = {"content": args["content"]}
            if args.get("id"):  # POST /v1/memories upserts by id
                body["id"] = args["id"]
            tier = args.get("tier")
            if tier in VALID_TIERS:
                body["tier"] = tier
            if args.get("tags"):
                body["tags"] = args["tags"]
            if args.get("category"):
                body["metadata"] = {"category": args["category"]}
            # visibility is NOT validated client-side beyond trimming: 'project'
            # and 'personal' are fixed, but any other value names an ancestor of
            # THIS namespace, which only the server can resolve — and its error
            # enumerates the valid chain, which is how the model learns the
            # topology. Swallowing an unknown name here would silently write to
            # the wrong place instead.
            visibility = str(args.get("visibility") or "").strip()
            if visibility:
                body["visibility"] = visibility
            result, error = self._call_result("/v1/memories", body)
            if error:
                return json.dumps({"id": None, "success": False, "error": error})
            out = {"id": (result or {}).get("id"), "success": True}
            # reinforced: the fact was already known, nothing new was written, and
            # id names the pre-existing memory. Dropping the flag here would let
            # the model report a no-op as a fresh save.
            if (result or {}).get("reinforced"):
                out["reinforced"] = True
            return json.dumps(out)

        if name == "memory_forget":
            mem_id = args.get("id")
            if not mem_id:
                return json.dumps({"forgotten": False, "error": "id is required"})
            result = self._call(
                f"/v1/memories/{quote(str(mem_id), safe='')}", None, method="DELETE"
            )
            return json.dumps({"forgotten": result is not None})

        return json.dumps({"error": f"Unknown tool: {name}"})


def register(ctx: Any) -> None:
    ctx.register_memory_provider(MeminiMemoryProvider())
