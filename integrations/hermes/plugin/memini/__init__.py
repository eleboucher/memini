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
    MEMINI_URL            base URL (default http://localhost:8080)
    MEMINI_NAMESPACE      tenant to scope memory to (default: cwd basename, else "hermes")
    MEMINI_API_KEY        bearer token, if memini requires auth
    MEMINI_REQUIRE_HTTPS  =1 to refuse sending a token over plaintext HTTP

Network errors are swallowed.
"""

from __future__ import annotations

import json
import os
import sys
import threading
from pathlib import Path
from typing import Any, Callable
from urllib.error import URLError
from urllib.parse import urlencode, urlparse
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
        def get_config_schema(self) -> list[dict]: return []
        def save_config(self, values: dict, hermes_home: str) -> None: pass
        def prefetch(self, query: str, **kwargs: Any) -> str: return ""
        def sync_turn(self, user: str, assistant: str, **kwargs: Any) -> None: pass
        def on_pre_compress(self, messages: list, **kwargs: Any) -> str: return ""
        def on_memory_write(self, action: str, target: str, content: str, **kwargs: Any) -> None: pass


DEFAULT_BASE_URL = "http://localhost:8080"
TIMEOUT = 5
LOOPBACK_HOSTS = {"localhost", "127.0.0.1", "::1"}
VALID_TIERS = ("working", "episodic", "semantic", "procedural")
_plaintext_bearer_warned = False


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def _uses_plaintext_bearer_auth(base: str, secret: str) -> bool:
    if not secret:
        return False
    parsed = urlparse(base)
    host = (parsed.hostname or "").lower()
    return parsed.scheme == "http" and host not in LOOPBACK_HOSTS


def _check_plaintext_bearer_guard(base: str, secret: str, warn: Callable[[str], None] | None = None) -> None:
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


def _valid_url(base: str) -> bool:
    try:
        parsed = urlparse(base)
        _ = parsed.port
    except ValueError:
        return False
    return parsed.scheme in ("http", "https") and bool(parsed.hostname)


def _api(base: str, path: str, body: dict | None, namespace: str, secret: str,
         method: str = "POST") -> dict | None:
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
    except (URLError, TimeoutError, ValueError):
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


class MeminiMemoryProvider(MemoryProvider):
    """Cross-session memory backed by a memini service."""

    @property
    def name(self) -> str:
        return "memini"

    def is_available(self) -> bool:
        return _valid_url(_env("MEMINI_URL", DEFAULT_BASE_URL))

    def initialize(self, session_id: str, **kwargs: Any) -> None:
        self._base = _env("MEMINI_URL", DEFAULT_BASE_URL).rstrip("/")
        self._secret = _env("MEMINI_API_KEY")
        self._session_id = session_id
        # Hermes' initialize kwargs carry no project path (agent_workspace is a
        # label, not a dir), so the working directory is the only signal for the
        # default namespace; set MEMINI_NAMESPACE to scope explicitly.
        self._namespace = _env("MEMINI_NAMESPACE") or os.path.basename(os.getcwd().rstrip("/")) or "hermes"
        if _env("MEMINI_REQUIRE_HTTPS") == "1":
            _check_plaintext_bearer_guard(self._base, self._secret)

    def _call(self, path: str, body: dict | None, method: str = "POST") -> dict | None:
        return _api(self._base, path, body, self._namespace, self._secret, method)

    def _call_bg(self, path: str, body: dict) -> None:
        threading.Thread(target=self._call, args=(path, body), daemon=True).start()

    def get_config_schema(self) -> list[dict]:
        return [
            {"key": "url", "description": "memini server URL",
             "default": DEFAULT_BASE_URL, "env_var": "MEMINI_URL"},
            {"key": "namespace", "description": "memory namespace (tenant)",
             "required": False, "env_var": "MEMINI_NAMESPACE"},
            {"key": "secret", "description": "memini bearer token (optional)",
             "secret": True, "required": False, "env_var": "MEMINI_API_KEY"},
        ]

    def save_config(self, values: dict, hermes_home: str) -> None:
        # Never persist secret-typed config (the bearer token) to disk. The
        # token is read from the MEMINI_API_KEY env var at runtime (see
        # initialize), not from this file, so writing it would leave a
        # plaintext credential on disk for no functional benefit.
        secret_keys = {c["key"] for c in self.get_config_schema() if c.get("secret")}
        safe = {k: v for k, v in values.items() if k not in secret_keys}
        (Path(hermes_home) / "memini.json").write_text(json.dumps(safe, indent=2))

    def _format(self, result: dict | None, limit: int) -> str:
        if not result:
            return ""
        lines = []
        for r in (result.get("results") or [])[:limit]:
            mem = r.get("memory") or {}
            text = (mem.get("summary") or mem.get("content") or "").strip()
            if text:
                lines.append(f"- {text[:300]}")
        return "\n".join(lines)

    def _recall_body(self, query: str) -> dict:
        # Exclude this session's own captured turns: they're still in the live
        # transcript, so recalling them just echoes the conversation back a turn
        # behind. Captures from other (past) sessions are still recalled.
        body = {"query": query, "limit": 5}
        if self._session_id:
            body["exclude_metadata"] = {"session_id": self._session_id}
        return body

    def prefetch(self, query: str, **kwargs: Any) -> str:
        if not query.strip():
            return ""
        block = self._format(self._call("/v1/search", self._recall_body(query)), 5)
        return f"Relevant memories (from memini):\n{block}" if block else ""

    def on_pre_compress(self, messages: list, **kwargs: Any) -> str:
        """Re-inject recalled context before history compaction."""
        query = ""
        for m in reversed(messages):
            role = m.get("role") if isinstance(m, dict) else getattr(m, "role", None)
            if role != "user":
                continue
            content = m.get("content") if isinstance(m, dict) else getattr(m, "content", None)
            if isinstance(content, str) and content.strip():
                query = content.strip()
                break
        block = self._format(self._call("/v1/search", self._recall_body(query)), 5) if query else ""
        return f"[memini context before compaction]\n{block}" if block else ""

    def sync_turn(self, user: str, assistant: str, **kwargs: Any) -> None:
        user, assistant = (user or "").strip(), (assistant or "").strip()
        if not user and not assistant:
            return
        self._call_bg("/v1/memories", {
            "content": f"User: {user[:1000]}\nAssistant: {assistant[:3000]}",
            "tier": "episodic",
            "metadata": {"source": "hermes", "session_id": kwargs.get("session_id", self._session_id)},
        })

    def on_memory_write(self, action: str, target: str, content: str, **kwargs: Any) -> None:
        if action in ("add", "update") and content.strip():
            self._call_bg("/v1/memories", {"content": content.strip()[:4000], "tier": "semantic"})

    def get_tool_schemas(self) -> list[dict]:
        return [
            {
                "name": "memory_recall",
                "description": "Search long-term memory (memini) for relevant past facts and context.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {"type": "string", "description": "What to search for"},
                        "limit": {"type": "integer", "description": "Max results", "default": 5},
                        "tags": {
                            "type": "array",
                            "items": {"type": "string"},
                            "description": "Restrict to memories carrying every listed tag (AND).",
                        },
                        "metadata": {
                            "type": "object",
                            "additionalProperties": {"type": "string"},
                            "description": "Restrict to memories whose metadata contains each "
                                           "key=value pair, e.g. {\"category\": \"bug_fixes\"}.",
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
                        "limit": {"type": "integer", "description": "Max results (0 = all)", "default": 20},
                    },
                },
            },
            {
                "name": "memory_remember",
                "description": "Store a durable fact, decision, or preference in long-term memory (memini).",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "content": {"type": "string", "description": "The fact to remember"},
                        "tier": {
                            "type": "string",
                            "enum": list(VALID_TIERS),
                            "description": "semantic=durable knowledge, procedural=how-to, "
                                           "episodic=what happened, working=transient",
                            "default": "semantic",
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
        ]

    def handle_tool_call(self, name: str, args: dict, **kwargs: Any) -> str:
        if name == "memory_recall":
            body = {"query": args["query"], "limit": args.get("limit", 5)}
            if args.get("tags"):
                body["tags"] = args["tags"]
            if args.get("metadata"):
                body["metadata"] = args["metadata"]
            result = self._call("/v1/search", body)
            items = []
            for r in (result or {}).get("results", []):
                mem = r.get("memory") or {}
                items.append({
                    "content": mem.get("content", ""),
                    "summary": mem.get("summary", ""),
                    "tier": mem.get("tier", ""),
                    "score": r.get("score", 0),
                })
            return json.dumps({"results": items})

        if name == "memory_list":
            result = self._call(_list_path(args), None, method="GET")
            items = []
            for mem in (result or {}).get("memories", []):
                items.append({
                    "id": mem.get("id", ""),
                    "content": mem.get("content", ""),
                    "summary": mem.get("summary", ""),
                    "tier": mem.get("tier", ""),
                    "tags": mem.get("tags", []),
                    "metadata": mem.get("metadata", {}),
                })
            return json.dumps({"memories": items})

        if name == "memory_remember":
            tier = args.get("tier", "semantic")
            if tier not in VALID_TIERS:
                tier = "semantic"
            body: dict = {"content": args["content"], "tier": tier}
            if args.get("tags"):
                body["tags"] = args["tags"]
            if args.get("category"):
                body["metadata"] = {"category": args["category"]}
            result = self._call("/v1/memories", body)
            return json.dumps({"id": (result or {}).get("id"), "success": result is not None})

        return json.dumps({"error": f"Unknown tool: {name}"})


def register(ctx: Any) -> None:
    ctx.register_memory_provider(MeminiMemoryProvider())
