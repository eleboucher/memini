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

import json
import os
import subprocess
from typing import Optional
from urllib.parse import urlparse

import aiohttp
from pydantic import BaseModel, Field

DEFAULT_BASE_URL = "http://localhost:8080"
DEFAULT_NAMESPACE = "openwebui"
LOOPBACK_HOSTS = {"localhost", "127.0.0.1", "::1"}


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
    X-Memini-Namespace header. Deliberately not sanitize_namespace: "/" survives,
    so a hierarchical override like acme/api reaches the server the way every
    other integration sends it. Flattened to acme-api it would write to a
    namespace nothing else reads, which is the exact failure the override exists
    to prevent."""
    return "".join(ch for ch in str(value).strip() if ch >= " " and ch != "\x7f")


# --- Namespace override ---------------------------------------------------
#
# $XDG_CONFIG_HOME/memini/overrides.json holds the per-project namespace a user
# set deliberately. It is a shared contract — the client plugins write it,
# `memini doctor` reads it — so every harness must honor it or they disagree
# about which namespace is in force. Reimplemented here (it is a JSON file plus
# a `git rev-parse`) because a Filter is a single file uploaded into Open WebUI:
# it can import nothing of memini's.


def overrides_path() -> str:
    """$XDG_CONFIG_HOME/memini/overrides.json, else ~/.config/memini/overrides.json."""
    xdg = os.environ.get("XDG_CONFIG_HOME", "")
    base = xdg if xdg.strip() else os.path.join(os.path.expanduser("~"), ".config")
    return os.path.join(base, "memini", "overrides.json")


def override_key(cwd: str) -> str:
    """The key an override is stored under: the git toplevel when there is one,
    else the resolved directory."""
    try:
        out = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            cwd=cwd,
            capture_output=True,
            text=True,
            timeout=0.5,
        )
        toplevel = out.stdout.strip() if out.returncode == 0 else ""
    except Exception:  # noqa: BLE001 — not a repo, no git, timeout: all the same
        toplevel = ""
    return os.path.abspath(toplevel or cwd)


def read_override(cwd: str) -> Optional[dict]:
    """The override in effect for cwd, as {"namespace", "setAt"}, or None.

    The file is read before the key is computed: the key costs a `git rev-parse`,
    and nobody should pay for one to discover they have no overrides at all. Any
    error — missing file, hand-edited JSON, wrong shape — yields None, because a
    broken overrides file must degrade to the configured namespace, never raise
    into Open WebUI."""
    try:
        with open(overrides_path(), encoding="utf-8") as handle:
            data = json.load(handle)
    except Exception:  # noqa: BLE001 — degrade gracefully by design
        return None
    overrides = data.get("overrides") if isinstance(data, dict) else None
    if not isinstance(overrides, dict) or not overrides:
        return None
    entry = overrides.get(override_key(cwd))
    if not isinstance(entry, dict):
        return None
    namespace = header_safe(str(entry.get("namespace") or ""))
    if not namespace:
        return None
    return {"namespace": namespace, "setAt": str(entry.get("setAt") or "")}


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
            default_factory=lambda: (
                os.environ.get("MEMINI_BASE_URL")
                or os.environ.get("MEMINI_URL")
                or DEFAULT_BASE_URL
            ),
            description="memini REST base URL (defaults from MEMINI_BASE_URL / MEMINI_URL env)",
        )
        namespace: str = Field(
            default=DEFAULT_NAMESPACE,
            description="Project the memory is scoped to (X-Memini-Namespace). Share it across agents to pool memory. A per-project override in $XDG_CONFIG_HOME/memini/overrides.json wins over this.",
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

    def _resolve_namespace(self) -> tuple:
        """(namespace, source). Order: per-project override > the namespace valve.

        The override wins over the valve for the same reason it wins over
        MEMINI_NAMESPACE everywhere else: it is the one namespace input a user
        sets deliberately, and an override some harnesses honored and others
        ignored would be worse than none. Open WebUI is a server, so "the
        project" is the directory it was launched in — which only means something
        for a local install, exactly where someone would set an override."""
        override = read_override(os.getcwd())
        if override:
            return override["namespace"], "override"
        valve = sanitize_namespace(self.valves.namespace or DEFAULT_NAMESPACE)
        return valve or DEFAULT_NAMESPACE, "valve"

    def _namespace(self, __user__: Optional[dict]) -> str:
        ns, _ = self._resolve_namespace()
        if self.valves.scope_by_user and __user__:
            uid = __user__.get("id") or __user__.get("email") or ""
            if uid:
                # The per-user suffix still applies on top of an override: it
                # isolates *who*, not *what*. Dropping it because an override is
                # set would silently collapse every user of a shared server into
                # one namespace.
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
        secret = (
            os.environ.get("MEMINI_API_KEY") or os.environ.get("MEMINI_TOKEN") or ""
        )
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
            self._namespace(__user__),
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
        metadata = {"source": "openwebui", "chat_id": chat_id, "format": "turn"}
        if last_assistant_failed(body.get("messages")):
            metadata["failed"] = True
        stored = await self._post_json(
            "/v1/memories",
            {
                "content": f"{user_text[:1000]}\n\n{assistant_text[:3000]}",
                "tags": ["openwebui"],
                "metadata": metadata,
            },
            self._namespace(__user__),
        )
        if stored is not None:
            # Keep the dedup set bounded.
            if len(self._captured) > 512:
                self._captured.clear()
            self._captured.add(key)
        return body
