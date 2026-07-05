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

import os
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
        text = str(mem.get("summary") or mem.get("content") or f"Memory {index + 1}").strip()
        if text:
            # Plain bullets, matching the opencode/hermes/openclaw/Claude default.
            lines.append(f"- {text[:300]}")
    return "\n".join(lines)


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
            default_factory=lambda: os.environ.get("MEMINI_BASE_URL")
            or os.environ.get("MEMINI_URL")
            or DEFAULT_BASE_URL,
            description="memini REST base URL (defaults from MEMINI_BASE_URL / MEMINI_URL env)",
        )
        namespace: str = Field(
            default=DEFAULT_NAMESPACE,
            description="Tenant the memory is scoped to (X-Memini-Namespace). Share it across agents to pool memory.",
        )
        recall: bool = Field(default=True, description="Recall memories before each turn")
        capture: bool = Field(default=True, description="Capture the completed turn after each response")
        recall_limit: int = Field(default=3, description="Max memories injected per turn")
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

    def _namespace(self, __user__: Optional[dict]) -> str:
        ns = self.valves.namespace or DEFAULT_NAMESPACE
        if self.valves.scope_by_user and __user__:
            uid = __user__.get("id") or __user__.get("email") or ""
            if uid:
                ns = f"{ns}-{uid}"
        return sanitize_namespace(ns) or DEFAULT_NAMESPACE

    async def _post_json(self, path: str, payload: dict, namespace: str) -> Optional[dict]:
        base_url = str(self.valves.base_url).rstrip("/")
        secret = os.environ.get("MEMINI_API_KEY") or os.environ.get("MEMINI_TOKEN") or ""
        if uses_plaintext_bearer(base_url, secret):
            message = (
                f"memini: MEMINI_API_KEY would cross plaintext HTTP to {base_url}; "
                "use HTTPS or an SSH tunnel."
            )
            if self.valves.require_https:
                raise RuntimeError(message)
            print(f"[memini] {message}")

        headers = {"Content-Type": "application/json", "X-Memini-Namespace": namespace}
        if secret:
            headers["Authorization"] = f"Bearer {secret}"
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
        chat_id = __chat_id__ or (__metadata__ or {}).get("chat_id") or body.get("chat_id") or ""
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
        chat_id = __chat_id__ or (__metadata__ or {}).get("chat_id") or body.get("chat_id") or ""
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
                "tier": "episodic",
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
