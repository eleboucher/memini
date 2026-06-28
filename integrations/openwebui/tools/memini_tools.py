"""
title: Memini Memory Tools
author: memini
author_url: https://github.com/eleboucher/memini
funding_url: https://github.com/eleboucher/memini
version: 0.1.0
license: MIT
required_open_webui_version: 0.5.0
description: On-demand memini memory — gives the model recall_memory / remember_memory tools to call when it decides it needs them. The automatic alternative is the Memini Memory filter.
"""

# Talks to memini over REST, scoped by the X-Memini-Namespace header; the API
# key comes from MEMINI_API_KEY so the secret stays out of the Open WebUI DB.

import os
from typing import Optional
from urllib.parse import urlparse

import aiohttp
from pydantic import BaseModel, Field

DEFAULT_BASE_URL = "http://localhost:8080"
DEFAULT_NAMESPACE = "openwebui"
LOOPBACK_HOSTS = {"localhost", "127.0.0.1", "::1"}


def sanitize_namespace(value: str) -> str:
    out = []
    for ch in str(value).strip():
        out.append(ch if (ch.isalnum() or ch in "._-") else "-")
    collapsed = "".join(out)
    while "--" in collapsed:
        collapsed = collapsed.replace("--", "-")
    return collapsed.strip("-")


def uses_plaintext_bearer(base_url: str, secret: str) -> bool:
    if not secret:
        return False
    try:
        parsed = urlparse(base_url)
    except ValueError:
        return False
    host = (parsed.hostname or "").lower()
    return parsed.scheme == "http" and host not in LOOPBACK_HOSTS


class Tools:
    class Valves(BaseModel):
        base_url: str = Field(
            default_factory=lambda: os.environ.get("MEMINI_BASE_URL")
            or os.environ.get("MEMINI_URL")
            or DEFAULT_BASE_URL,
            description="memini REST base URL (defaults from MEMINI_BASE_URL / MEMINI_URL env)",
        )
        namespace: str = Field(
            default=DEFAULT_NAMESPACE,
            description="Tenant the memory is scoped to (X-Memini-Namespace)",
        )
        recall_limit: int = Field(default=3, description="Max memories returned by recall_memory")
        timeout_ms: int = Field(default=5000, description="Per-request timeout (ms)")
        require_https: bool = Field(
            default=False,
            description="Refuse to send the API key over plaintext HTTP to a non-loopback host",
        )

    def __init__(self):
        self.valves = self.Valves()

    async def _post_json(self, path: str, payload: dict) -> Optional[dict]:
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
        headers = {
            "Content-Type": "application/json",
            "X-Memini-Namespace": sanitize_namespace(self.valves.namespace) or DEFAULT_NAMESPACE,
        }
        if secret:
            headers["Authorization"] = f"Bearer {secret}"
        timeout = aiohttp.ClientTimeout(total=self.valves.timeout_ms / 1000)
        async with aiohttp.ClientSession(timeout=timeout) as session:
            async with session.post(
                f"{base_url}{path}", json=payload, headers=headers
            ) as res:
                if res.status >= 400:
                    body = await res.text()
                    raise RuntimeError(f"memini {path} failed: {res.status} {body}")
                return await res.json()

    async def recall_memory(self, query: str, __event_emitter__=None) -> str:
        """
        Search long-term memory for facts relevant to a query. Call this when the
        user refers to past conversations, prior decisions, or anything you might
        have been told before.

        :param query: What to look up in memory.
        :return: The matching memories, or a note that none were found.
        """
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": "Recalling memories…", "done": False}}
            )
        result = await self._post_json(
            "/v1/search", {"query": query, "limit": self.valves.recall_limit}
        )
        results = (result or {}).get("results") or []
        if not results:
            if __event_emitter__:
                await __event_emitter__(
                    {"type": "status", "data": {"description": "No memories found", "done": True}}
                )
            return "No relevant memories found."
        lines = []
        for index, item in enumerate(results[: self.valves.recall_limit]):
            mem = (item or {}).get("memory") or {}
            text = str(mem.get("summary") or mem.get("content") or f"Memory {index + 1}").strip()
            tier = str(mem.get("tier") or "memory").strip()
            lines.append(f"- ({tier}) {text[:300]}")
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": f"Recalled {len(lines)} memories", "done": True}}
            )
        return "Relevant memories:\n" + "\n".join(lines)

    async def remember_memory(self, content: str, __event_emitter__=None) -> str:
        """
        Store a fact in long-term memory so it can be recalled in future
        conversations. Call this when the user shares a durable preference,
        decision, or fact worth remembering.

        :param content: The fact to remember, written as a self-contained statement.
        :return: Confirmation that the memory was stored.
        """
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": "Storing memory…", "done": False}}
            )
        await self._post_json(
            "/v1/memories",
            {
                "content": content,
                "tier": "episodic",
                "tags": ["openwebui"],
                "metadata": {"source": "openwebui"},
            },
        )
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": "Memory stored", "done": True}}
            )
        return "Stored in memory."
