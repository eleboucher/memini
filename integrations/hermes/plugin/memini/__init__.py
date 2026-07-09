"""memini memory provider for Hermes Agent.

Native MemoryProvider: Hermes drives it directly, no MCP server.

    prefetch         recall relevant memories before each turn
    sync_turn        capture each exchange (episodic)
    on_pre_compress  re-inject recalled context before compaction
    on_memory_write  mirror MEMORY.md/USER.md edits into memini (semantic)
    tools            memory_recall / memory_remember

Install: copy this directory to ~/.hermes/plugins/memini and set
`memory.provider: memini` in ~/.hermes/config.yaml. Memory providers are
single-select; `plugins.enabled` does not activate them.

Environment:
    MEMINI_BASE_URL                 base URL (default http://localhost:8080; alias: MEMINI_URL)
    MEMINI_NAMESPACE                tenant to scope memory to (default: cwd basename, else "hermes")
    MEMINI_API_KEY                  bearer token, if memini requires auth (alias: MEMINI_TOKEN)
    MEMINI_REQUIRE_HTTPS            =1 to refuse sending a token over plaintext HTTP
    MEMINI_RECALL_LIMIT             max memories recalled per turn (default 3)
    MEMINI_INJECT_RECALL_MIN_SCORE  fused-score floor (>=) for auto-recall (default 0)
    MEMINI_INJECT_RECALL_MAX_TOK    hard token ceiling on the recall block (0 = unbounded)
    MEMINI_INJECT_LABELS            comma/pipe bullet labels: tier, confidence, age

The recall knobs mirror the opencode/Claude Code plugins so memory shaping is
consistent across integrations. Network errors are swallowed.
"""

from __future__ import annotations

import json
import os
import re
import sys
import threading
from pathlib import Path
from typing import Any, Callable
from urllib.error import URLError
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
TIMEOUT = 5
LOOPBACK_HOSTS = {"localhost", "127.0.0.1", "::1"}
VALID_TIERS = ("working", "episodic", "semantic", "procedural")
_plaintext_bearer_warned = False


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


# Canonical env names with back-compat aliases, so one env setup works across
# all memini integrations: MEMINI_BASE_URL (alias MEMINI_URL) for the server,
# MEMINI_API_KEY (alias MEMINI_TOKEN) for the token.
def _base_url() -> str:
    return _env("MEMINI_BASE_URL") or _env("MEMINI_URL") or DEFAULT_BASE_URL


def _secret() -> str:
    return _env("MEMINI_API_KEY") or _env("MEMINI_TOKEN")


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


def _resolve_tenant(cwd: str) -> str | None:
    """Read ~/.config/memini/config.json and return the tenant name if cwd
    is under a configured tenant root. Returns None when no config file,
    no match, or any error — so the existing namespace resolution is the fallback."""
    try:
        xdg = os.environ.get(
            "XDG_CONFIG_HOME", os.path.join(os.path.expanduser("~"), ".config")
        )
        config_path = Path(xdg) / "memini" / "config.json"
        config = json.loads(config_path.read_text(encoding="utf-8"))
        roots = config.get("tenantRoots")
        if not isinstance(roots, list):
            return None
        cwd_resolved = Path(cwd).resolve()
        for root in roots:
            root_path = root.get("path", "")
            if root_path == "~":
                root_path = os.path.expanduser("~")
            elif root_path.startswith("~/"):
                root_path = os.path.join(os.path.expanduser("~"), root_path[2:])
            root_resolved = Path(root_path).resolve()
            if cwd_resolved == root_resolved or root_resolved in cwd_resolved.parents:
                tenant = root.get("tenant", "")
                tenant = re.sub(r"[^A-Za-z0-9._-]+", "-", tenant).strip("-")
                if tenant:
                    return tenant
        return None
    except Exception:
        return None


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
) -> dict | None:
    if not _valid_url(base):
        return None
    headers = {"Content-Type": "application/json", "X-Memini-Namespace": namespace}
    _check_plaintext_bearer_guard(base, secret)
    if secret:
        headers["Authorization"] = f"Bearer {secret}"
    data = json.dumps(body).encode() if body is not None else None
    req = Request(f"{base}{path}", data=data, headers=headers, method=method)
    try:
        with urlopen(req, timeout=TIMEOUT) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else {}
    except (URLError, TimeoutError, ValueError) as e:
        # Degrade gracefully but never silently: a swallowed capture/recall
        # failure looks like "memory isn't working" with nothing to debug.
        print(f"[memini] {method} {path} failed: {e}", file=sys.stderr)
        return None


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


def _int_env(name: str, default: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        n = int(raw)
    except ValueError:
        return default
    return n if n >= 0 else default


def _float_env(name: str, default: float) -> float:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        n = float(raw)
    except ValueError:
        return default
    return n if n >= 0 else default


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
        # Hermes' initialize kwargs carry no project path (agent_workspace is a
        # label, not a dir), so the working directory is the only signal for the
        # default namespace; set MEMINI_NAMESPACE to scope explicitly.
        tenant = _resolve_tenant(os.getcwd())
        project = os.path.basename(os.getcwd().rstrip("/"))
        if _env("MEMINI_NAMESPACE"):
            ns = _env("MEMINI_NAMESPACE")
        elif tenant:
            ns = f"{tenant}/{project}"
        else:
            ns = project
        self._namespace = _sanitize_namespace(ns) or "hermes"
        # Recall-shaping knobs, read once. Defaults match the other integrations
        # (limit 3, no floor, unbounded, plain bullets).
        limit = _int_env("MEMINI_RECALL_LIMIT", 3)
        self._recall_limit = limit if limit > 0 else 3
        self._recall_min_score = _float_env("MEMINI_INJECT_RECALL_MIN_SCORE", 0.0)
        self._recall_max_tokens = _int_env("MEMINI_INJECT_RECALL_MAX_TOK", 0)
        self._labels = _labels_env()
        if _env("MEMINI_REQUIRE_HTTPS") == "1":
            _check_plaintext_bearer_guard(self._base, self._secret)

    def _call(self, path: str, body: dict | None, method: str = "POST") -> dict | None:
        return _api(self._base, path, body, self._namespace, self._secret, method)

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
        floor = self._recall_min_score
        lines: list[str] = []
        for r in (result.get("results") or [])[: self._recall_limit]:
            # Belt-and-braces client floor: drop sub-threshold hits the server
            # may still return under score-normalization edge cases.
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

    def _recall_body(self, query: str) -> dict:
        # Exclude this session's own captured turns: they're still in the live
        # transcript, so recalling them just echoes the conversation back a turn
        # behind. Captures from other (past) sessions are still recalled.
        body: dict = {"query": query, "limit": self._recall_limit}
        if self._recall_min_score > 0:
            body["min_score"] = self._recall_min_score
        if self._session_id:
            body["exclude_metadata"] = {"session_id": self._session_id}
        return body

    def prefetch(self, query: str, **kwargs: Any) -> str:
        if not query.strip():
            return ""
        return self._recall_block(
            self._call("/v1/search", self._recall_body(query)),
            "Relevant memories (from memini):",
        )

    def on_pre_compress(self, messages: list, **kwargs: Any) -> str:
        """Re-inject recalled context before history compaction."""
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
        return self._recall_block(
            self._call("/v1/search", self._recall_body(query)),
            "[memini context before compaction]",
        )

    def sync_turn(self, user: str, assistant: str, **kwargs: Any) -> None:
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
                "content": f"{user[:1000]}\n\n{assistant[:3000]}",
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
        if action in ("add", "replace") and content.strip():
            self._call_bg("/v1/memories", {"content": content.strip()[:4000]})

    def get_tool_schemas(self) -> list[dict]:
        return [
            {
                "name": "memory_recall",
                "description": "Search long-term memory (memini) for relevant past facts and context. Call "
                "before starting work that may have history: editing an unfamiliar file, "
                "debugging a recurring issue, or when asked what's known about something. A "
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
                    },
                    "required": ["query"],
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
                "convention. Keep memories atomic — one self-contained fact per call.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "content": {
                            "type": "string",
                            "description": "The fact to remember",
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
                            "description": "Optional keywords for later search/filtering.",
                        },
                        "category": {
                            "type": "string",
                            "description": "Optional topic bucket stored as metadata.category "
                            "(e.g. bug_fixes, architecture_decisions, coding_conventions) "
                            "so the memory can be browsed by subject later.",
                        },
                    },
                    "required": ["content"],
                },
            },
            {
                "name": "memory_forget",
                "description": "Permanently delete a memory from long-term memory (memini) by its id — use when "
                "a recalled memory is wrong, outdated, or poisoned. Get the id from memory_recall "
                "or memory_list. To correct a fact, forget the stale one and remember the "
                "corrected version — this provider talks to memini over REST, which has no "
                "partial-update endpoint.",
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
        if name == "memory_recall":
            body = {"query": args["query"], "limit": args.get("limit", 3)}
            if args.get("tags"):
                body["tags"] = args["tags"]
            if args.get("metadata"):
                body["metadata"] = args["metadata"]
            result = self._call("/v1/search", body)
            items = []
            for r in (result or {}).get("results", []):
                mem = r.get("memory") or {}
                items.append(
                    {
                        "id": mem.get("id", ""),
                        "content": mem.get("content", ""),
                        "summary": mem.get("summary", ""),
                        "tier": mem.get("tier", ""),
                        "score": r.get("score", 0),
                    }
                )
            # /v1/search already carries degraded/note on `result`; pass them
            # through rather than dropping them silently.
            out = {"results": items}
            if (result or {}).get("degraded"):
                out["degraded"] = result["degraded"]
                if result.get("note"):
                    out["note"] = result["note"]
            return json.dumps(out)

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
            tier = args.get("tier")
            if tier in VALID_TIERS:
                body["tier"] = tier
            if args.get("tags"):
                body["tags"] = args["tags"]
            if args.get("category"):
                body["metadata"] = {"category": args["category"]}
            result = self._call("/v1/memories", body)
            return json.dumps(
                {"id": (result or {}).get("id"), "success": result is not None}
            )

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
