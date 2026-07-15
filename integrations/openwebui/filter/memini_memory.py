"""
title: Memini Memory
author: memini
author_url: https://github.com/eleboucher/memini
funding_url: https://github.com/eleboucher/memini
version: 0.1.0
license: MIT
required_open_webui_version: 0.5.0
description: Automatic cross-session memory via memini — recall relevant memories before each turn (inlet) and capture the completed turn after (outlet). No tool calls required from the model.
"""

# Talks to memini over REST (POST /v1/search, POST /v1/memories), scoped by the
# X-Memini-Namespace header. The API key comes from MEMINI_API_KEY so the secret
# stays out of the Open WebUI database.
#
# Namespace resolution: the admin Valve is the DECLARED namespace (Open WebUI is
# a server with no meaningful per-request cwd, unlike a local agent — a
# cwd-keyed override was never meaningful here). It is sent to POST
# /v1/handshake (api/openapi.yaml) as project.declared_namespace on every
# session; the server echoes it back verbatim unless an explicit pin overrides
# it. The handshake is fail-soft (any error, or a ~2.5s timeout, falls back to
# the valve value alone — see _handshake/_get_handshake) and memoized on this
# Filter instance with a 10-minute TTL.

import os
import time
from typing import Optional
from urllib.parse import urlparse

import aiohttp
from pydantic import BaseModel, Field

DEFAULT_BASE_URL = "http://localhost:8080"
DEFAULT_NAMESPACE = "openwebui"
LOOPBACK_HOSTS = {"localhost", "127.0.0.1", "::1"}
# Mirrors packages/memini-client's HANDSHAKE_TTL_MS / default timeout; this
# integration ships as a single-file Open WebUI upload so it stays a copy.
HANDSHAKE_TTL_S = 600.0
HANDSHAKE_TIMEOUT_S = 2.5
CLIENT_NAME = "openwebui-memini-filter"
CLIENT_VERSION = "0.1.0"  # keep in sync with this file's `version:` frontmatter


def truncate_for_capture(text: str, max_chars: int) -> str:
    """Cut `text` to `max_chars` characters, marking the cut; <= 0 keeps it whole.

    0 must mean "uncapped", not "" (`text[:0]` would store an empty turn), and a
    cut must be marked: a captured turn is later recalled into a model's context,
    where a silently half-cut sentence reads exactly like a complete one.
    """
    if not isinstance(max_chars, int) or isinstance(max_chars, bool) or max_chars <= 0:
        # Not a usable cap (None, a bool, a string from a server that disagrees
        # with the schema) means "no cap". Failing open stores the text; the
        # alternative is destroying it this close to the write.
        return text
    if len(text) <= max_chars:
        return text
    return text[:max_chars] + "\n[...truncated]"


def sanitize_namespace(value: str) -> str:
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


def header_safe(value: str) -> str:
    """Trim and drop control characters — a CR or LF in the value would split the
    X-Memini-Namespace header (or inject a bogus field into the handshake's JSON
    body). Deliberately not sanitize_namespace: "/" survives, so a hierarchical
    namespace valve like acme/api reaches the server the way every other
    integration sends it. Flattened to acme-api it would write to a namespace
    nothing else reads."""
    return "".join(ch for ch in str(value).strip() if ch >= " " and ch != "\x7f")


def extract_last_user(messages: list) -> str:
    """Return the text of the most recent user message."""
    if not isinstance(messages, list):
        return ""
    for message in reversed(messages):
        if isinstance(message, dict) and message.get("role") == "user":
            content = message.get("content")
            if isinstance(content, str):
                return content.strip()
            # Multimodal content is a list of parts; join the text parts.
            if isinstance(content, list):
                parts = [
                    p.get("text", "")
                    for p in content
                    if isinstance(p, dict) and p.get("type") == "text"
                ]
                return "\n".join(parts).strip()
    return ""


def extract_last_turn(messages: list) -> tuple:
    """Return (user_text, assistant_text) for the latest completed turn."""
    user_text = ""
    assistant_text = ""
    if not isinstance(messages, list):
        return user_text, assistant_text
    for message in messages:
        if not isinstance(message, dict):
            continue
        content = message.get("content")
        if not isinstance(content, str) or not content.strip():
            continue
        role = message.get("role")
        if role == "user":
            user_text = content.strip()
        elif role == "assistant":
            assistant_text = content.strip()
    return user_text, assistant_text


def last_assistant_failed(messages) -> bool:
    """Whether the latest assistant message errored, so the capture can flag it
    (the distiller mines failed→fixed turns into recovery)."""
    if not isinstance(messages, list):
        return False
    for message in reversed(messages):
        if isinstance(message, dict) and message.get("role") == "assistant":
            return bool(message.get("error"))
    return False


def format_results(results: list, limit: int) -> str:
    """Render memini search hits as a compact bullet list."""
    if not isinstance(results, list) or not results:
        return ""
    lines = []
    for index, result in enumerate(results[: max(limit, 0)]):
        mem = (result or {}).get("memory") or {}
        text = str(
            mem.get("summary") or mem.get("content") or f"Memory {index + 1}"
        ).strip()
        if text:
            # Plain bullets, matching the opencode/hermes/openclaw/Claude default.
            lines.append(f"- {text[:300]}")
    return "\n".join(lines)


def degraded_note(result: Optional[dict]) -> str:
    """Render the /v1/search degraded marker as a one-line note, or "" when the
    search was healthy. `degraded`/`note` are already on the response, set when
    the query embed was unavailable and search fell back to keyword-only
    matching — surface them so a keyword-only result isn't read as exhaustive."""
    if not isinstance(result, dict) or not result.get("degraded"):
        return ""
    note = result.get("note") or (
        "semantic search unavailable — these results are keyword-only and may be incomplete"
    )
    return f"[memini: {note}]"


def uses_plaintext_bearer(base_url: str, secret: str) -> bool:
    """True when a bearer token would cross plaintext HTTP to a non-loopback host."""
    if not secret:
        return False
    try:
        parsed = urlparse(base_url)
    except ValueError:
        return False
    host = (parsed.hostname or "").lower()
    return parsed.scheme == "http" and host not in LOOPBACK_HOSTS


class Filter:
    class Valves(BaseModel):
        base_url: str = Field(
            default_factory=lambda: os.environ.get("MEMINI_BASE_URL") or DEFAULT_BASE_URL,
            description="memini REST base URL (defaults from MEMINI_BASE_URL env)",
        )
        namespace: str = Field(
            default=DEFAULT_NAMESPACE,
            description=(
                "Project the memory is scoped to (X-Memini-Namespace). Share it across "
                "agents to pool memory. Sent to the server as the declared namespace via "
                "POST /v1/handshake on each session — the server echoes it back verbatim "
                "unless an explicit pin overrides it."
            ),
        )
        home: str = Field(
            default_factory=lambda: os.environ.get("MEMINI_HOME", ""),
            description="Caller's personal namespace, sent as X-Memini-Home (unset = no home leg)",
        )
        recall: bool = Field(
            default=True, description="Recall memories before each turn"
        )
        capture: bool = Field(
            default=True, description="Capture the completed turn after each response"
        )
        recall_limit: int = Field(
            default=3, description="Max memories injected per turn"
        )
        timeout_ms: int = Field(default=5000, description="Per-request timeout (ms)")
        fallback_on_error: bool = Field(
            default=True,
            description="Degrade silently on memini errors instead of surfacing them",
        )
        require_https: bool = Field(
            default=False,
            description="Refuse to send the API key over plaintext HTTP to a non-loopback host",
        )
        scope_by_user: bool = Field(
            default=False,
            description="Isolate memory per Open WebUI user by suffixing the namespace with the user id",
        )
        priority: int = Field(default=0, description="Filter execution priority")

    def __init__(self):
        self.valves = self.Valves()
        # Bounds repeated outlet calls for the same response from writing dupes.
        self._captured = set()
        # Memoized POST /v1/handshake result: {"result": dict | None, "expires_at": float}.
        # Per-instance (Open WebUI holds one Filter instance for a good while), TTL
        # 10 minutes — see _get_handshake.
        self._handshake_cache = None

    def _facts(self) -> dict:
        """Assemble HandshakeRequest.project (api/openapi.yaml). Open WebUI is a
        server with no meaningful per-request cwd, so cwd_basename (required by
        the wire contract) is low-signal filler; the real signal is
        declared_namespace, carrying the admin's namespace valve."""
        cwd = os.getcwd()
        facts = {"cwd_basename": os.path.basename(cwd.rstrip("/")) or DEFAULT_NAMESPACE}
        declared = header_safe(str(self.valves.namespace or ""))
        if declared:
            facts["declared_namespace"] = declared
        return facts

    async def _handshake(self) -> Optional[dict]:
        """POST /v1/handshake (api/openapi.yaml). Fail-soft ALWAYS: any network
        error, non-2xx, or a ~2.5s timeout degrades to None, and
        _resolve_namespace falls back to the valve value alone. Not memoized
        here — see _get_handshake."""
        base_url = str(self.valves.base_url).rstrip("/")
        secret = os.environ.get("MEMINI_API_KEY") or ""
        # Deliberate exception to "Fail-soft ALWAYS" above: this raise escapes the try/except below, matching @memini/client's assertBearerTransportSafe.
        if uses_plaintext_bearer(base_url, secret):
            message = (
                f"memini: MEMINI_API_KEY would cross plaintext HTTP to {base_url}; "
                "use HTTPS or an SSH tunnel."
            )
            if self.valves.require_https:
                raise RuntimeError(message)
            print(f"[memini] {message}")

        headers = {"Content-Type": "application/json"}
        if secret:
            headers["Authorization"] = f"Bearer {secret}"
        home = str(self.valves.home or "").strip()
        if home:
            headers["X-Memini-Home"] = home
        body = {"project": self._facts(), "client": {"name": CLIENT_NAME, "version": CLIENT_VERSION}}
        timeout = aiohttp.ClientTimeout(total=HANDSHAKE_TIMEOUT_S)
        try:
            async with aiohttp.ClientSession(timeout=timeout) as session:
                async with session.post(
                    f"{base_url}/v1/handshake", json=body, headers=headers
                ) as res:
                    if res.status >= 400:
                        print(f"[memini] handshake failed: {res.status}")
                        return None
                    return await res.json()
        except Exception as error:  # noqa: BLE001 — fail-soft ALWAYS, unconditionally
            print(f"[memini] handshake failed: {error}")
            return None

    async def _get_handshake(self) -> Optional[dict]:
        """_handshake, memoized on this Filter instance with a 10-minute TTL."""
        now = time.monotonic()
        cached = self._handshake_cache
        if cached and now < cached["expires_at"]:
            return cached["result"]
        result = await self._handshake()
        self._handshake_cache = {"result": result, "expires_at": now + HANDSHAKE_TTL_S}
        return result

    async def _resolve_namespace(self) -> tuple:
        """(namespace, source). The admin Valve is the DECLARED namespace (Open
        WebUI is a server, so "the project" only ever meant the launch
        directory — never as meaningful as a real project override, which is
        why that mechanism is gone). It is sent to POST /v1/handshake as
        project.declared_namespace; the server echoes it back verbatim unless
        an explicit pin overrides it. Fail-soft: any handshake error/timeout
        falls back to the valve value alone."""
        valve = sanitize_namespace(self.valves.namespace or DEFAULT_NAMESPACE) or DEFAULT_NAMESPACE
        hs = await self._get_handshake()
        if hs and hs.get("namespace"):
            return str(hs["namespace"]), f"server:{hs.get('namespace_source', '')}"
        return valve, "valve"

    async def _capture_bounds(self) -> tuple[int, int]:
        """The turn-capture bounds, from the server's handshake settings.

        How much of a turn is worth keeping depends on the server's store and
        recall budget, so it is the server's call, not a valve's — these are the
        only two settings this filter takes from the handshake (its other knobs
        are Valves by design). Fail-soft like everything else here: no handshake,
        no settings, or a non-integer value falls back to the built-in default.
        """
        hs = await self._get_handshake()
        settings = (hs or {}).get("settings") or {}

        def bound(key: str, default: int) -> int:
            v = settings.get(key)
            return v if isinstance(v, int) and not isinstance(v, bool) and v >= 0 else default

        return bound("capture_user_max_chars", 1000), bound("capture_assistant_max_chars", 3000)

    async def _namespace(self, __user__: Optional[dict]) -> str:
        ns, _ = await self._resolve_namespace()
        if self.valves.scope_by_user and __user__:
            uid = __user__.get("id") or __user__.get("email") or ""
            if uid:
                # The per-user suffix applies AFTER namespace resolution: it
                # isolates *who*, not *what*, and must not be skipped just
                # because the server resolved a namespace — a shared server
                # must still keep each account's memory apart.
                ns = f"{ns}-{sanitize_namespace(uid)}"
        return ns or DEFAULT_NAMESPACE

    def _headers(self, namespace: str, secret: str) -> dict:
        """Build request headers — the single choke point every REST call goes
        through, so X-Memini-Home (and any future header) only needs wiring
        once."""
        headers = {"Content-Type": "application/json", "X-Memini-Namespace": namespace}
        if secret:
            headers["Authorization"] = f"Bearer {secret}"
        home = str(self.valves.home or "").strip()
        if home:
            headers["X-Memini-Home"] = home
        return headers

    async def _post_json(
        self, path: str, payload: dict, namespace: str
    ) -> Optional[dict]:
        base_url = str(self.valves.base_url).rstrip("/")
        secret = os.environ.get("MEMINI_API_KEY") or ""
        # Deliberate exception to the "degrade gracefully by design" except below: this raise escapes it, matching @memini/client's assertBearerTransportSafe.
        if uses_plaintext_bearer(base_url, secret):
            message = (
                f"memini: MEMINI_API_KEY would cross plaintext HTTP to {base_url}; "
                "use HTTPS or an SSH tunnel."
            )
            if self.valves.require_https:
                raise RuntimeError(message)
            print(f"[memini] {message}")

        headers = self._headers(namespace, secret)
        timeout = aiohttp.ClientTimeout(total=self.valves.timeout_ms / 1000)
        try:
            async with aiohttp.ClientSession(timeout=timeout) as session:
                async with session.post(
                    f"{base_url}{path}", json=payload, headers=headers
                ) as res:
                    if res.status >= 400:
                        if self.valves.fallback_on_error:
                            return None
                        body = await res.text()
                        raise RuntimeError(f"memini {path} failed: {res.status} {body}")
                    return await res.json()
        except Exception as error:  # noqa: BLE001 — degrade gracefully by design
            if not self.valves.fallback_on_error:
                raise
            print(f"[memini] {error}")
            return None

    async def inlet(
        self,
        body: dict,
        __user__: Optional[dict] = None,
        __metadata__: Optional[dict] = None,
        __chat_id__: Optional[str] = None,
    ) -> dict:
        if not self.valves.recall:
            return body
        messages = body.get("messages")
        query = extract_last_user(messages)
        if not query or not isinstance(messages, list):
            return body
        payload = {"query": query, "limit": self.valves.recall_limit}
        # Exclude this chat's own captured turns: they're still in the live
        # transcript, so recalling them just echoes the conversation back a turn
        # behind. Captures from other (past) chats are still recalled. On inlet
        # the chat id arrives via injected __chat_id__/__metadata__, not body.
        chat_id = (
            __chat_id__
            or (__metadata__ or {}).get("chat_id")
            or body.get("chat_id")
            or ""
        )
        if chat_id:
            payload["exclude_metadata"] = {"chat_id": chat_id}
        result = await self._post_json(
            "/v1/search",
            payload,
            await self._namespace(__user__),
        )
        block = format_results((result or {}).get("results"), self.valves.recall_limit)
        if not block:
            return body
        note = degraded_note(result)
        if note:
            block += "\n" + note
        context = {
            "role": "system",
            "content": (
                "Relevant long-term memory from memini (background context — prefer "
                "the user's current instructions):\n" + block
            ),
        }
        # Insert before the latest user message so it reads as preceding context.
        insert_at = len(messages)
        for i in range(len(messages) - 1, -1, -1):
            if isinstance(messages[i], dict) and messages[i].get("role") == "user":
                insert_at = i
                break
        messages.insert(insert_at, context)
        return body

    async def outlet(
        self,
        body: dict,
        __user__: Optional[dict] = None,
        __metadata__: Optional[dict] = None,
        __chat_id__: Optional[str] = None,
    ) -> dict:
        if not self.valves.capture:
            return body
        user_text, assistant_text = extract_last_turn(body.get("messages"))
        if not user_text or not assistant_text:
            return body
        # Same resolution as inlet; the outlet payload also carries body["chat_id"].
        # (Open WebUI never injects __chat_id__ on outlet — the metadata/body legs
        # are the ones that resolve here.)
        chat_id = (
            __chat_id__
            or (__metadata__ or {}).get("chat_id")
            or body.get("chat_id")
            or ""
        )
        if not chat_id:
            # A capture without a chat id can never be excluded by inlet recall,
            # so it would echo this chat's own turns back forever. Skip it.
            return body
        key = f"{chat_id}:{hash(assistant_text)}"
        if key in self._captured:
            return body
        user_max, assistant_max = await self._capture_bounds()
        metadata = {"source": "openwebui", "chat_id": chat_id, "format": "turn"}
        if last_assistant_failed(body.get("messages")):
            metadata["failed"] = True
        stored = await self._post_json(
            "/v1/memories",
            {
                "content": f"{truncate_for_capture(user_text, user_max)}\n\n{truncate_for_capture(assistant_text, assistant_max)}",
                "tags": ["openwebui"],
                "metadata": metadata,
            },
            await self._namespace(__user__),
        )
        if stored is not None:
            # Keep the dedup set bounded.
            if len(self._captured) > 512:
                self._captured.clear()
            self._captured.add(key)
        return body
