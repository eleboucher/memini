"""
title: Memini Memory Tools
author: memini
author_url: https://github.com/eleboucher/memini
funding_url: https://github.com/eleboucher/memini
version: 0.1.0
license: MIT
required_open_webui_version: 0.5.0
description: On-demand memini memory — gives the model recall_memory / remember_memory tools to call when it decides it needs them, plus memini_status to report which namespace is in force and why. The automatic alternative is the Memini Memory filter.
"""

# Talks to memini over REST, scoped by the X-Memini-Namespace header; the API
# key comes from MEMINI_API_KEY so the secret stays out of the Open WebUI DB.
#
# Namespace resolution: the admin Valve is the DECLARED namespace (Open WebUI is
# a server with no meaningful per-request cwd — a cwd-keyed override was never
# meaningful here). It is sent to POST /v1/handshake (api/openapi.yaml) as
# project.declared_namespace on every call; the server echoes it back verbatim
# unless an explicit pin overrides it. The handshake is fail-soft (any error, or
# a ~2.5s timeout, falls back to the valve value alone) and memoized on this
# Tools instance with a 10-minute TTL.

import os
import time
from typing import Optional
from urllib.parse import quote, urlparse

import aiohttp
from pydantic import BaseModel, Field

DEFAULT_BASE_URL = "http://localhost:8080"
DEFAULT_NAMESPACE = "openwebui"
LOOPBACK_HOSTS = {"localhost", "127.0.0.1", "::1"}
# Mirrors packages/memini-client's HANDSHAKE_TTL_MS / default timeout; this
# integration ships as a single-file Open WebUI upload so it stays a copy.
HANDSHAKE_TTL_S = 600.0
HANDSHAKE_TIMEOUT_S = 2.5
CLIENT_NAME = "openwebui-memini-tools"
CLIENT_VERSION = "0.1.0"  # keep in sync with this file's `version:` frontmatter


def sanitize_namespace(value: str) -> str:
    out = []
    for ch in str(value).strip():
        out.append(ch if (ch.isalnum() or ch in "._-") else "-")
    collapsed = "".join(out)
    while "--" in collapsed:
        collapsed = collapsed.replace("--", "-")
    return collapsed.strip("-")


def header_safe(value: str) -> str:
    """Trim and drop control characters (a CR/LF would split the header, or
    inject a bogus field into the handshake's JSON body). Not
    sanitize_namespace: "/" survives, so a hierarchical namespace valve like
    acme/api reaches the server the way the other integrations send it rather
    than being flattened into a namespace nothing else reads."""
    return "".join(ch for ch in str(value).strip() if ch >= " " and ch != "\x7f")


def redact_secret(value: str) -> str:
    """A recognizable-but-useless fingerprint: enough to tell two tokens apart,
    not enough to use. Short values are elided entirely rather than
    half-revealed. Mirrors the redaction in packages/memini-client."""
    if not value:
        return ""
    return "***" if len(value) <= 12 else f"{value[:3]}…{value[-4:]}"


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
            default_factory=lambda: os.environ.get("MEMINI_BASE_URL") or DEFAULT_BASE_URL,
            description="memini REST base URL (defaults from MEMINI_BASE_URL env)",
        )
        namespace: str = Field(
            default=DEFAULT_NAMESPACE,
            description=(
                "Project the memory is scoped to (X-Memini-Namespace). Sent to the server as "
                "the declared namespace via POST /v1/handshake on each call — the server echoes "
                "it back verbatim unless an explicit pin overrides it."
            ),
        )
        home: str = Field(
            default_factory=lambda: os.environ.get("MEMINI_HOME", ""),
            description="Caller's personal namespace, sent as X-Memini-Home (unset = no home leg)",
        )
        recall_limit: int = Field(default=3, description="Max memories returned by recall_memory")
        timeout_ms: int = Field(default=5000, description="Per-request timeout (ms)")
        require_https: bool = Field(
            default=False,
            description="Refuse to send the API key over plaintext HTTP to a non-loopback host",
        )

    def __init__(self):
        self.valves = self.Valves()
        # Memoized POST /v1/handshake result: {"result": dict | None, "expires_at": float}.
        # Per-instance, TTL 10 minutes — see _get_handshake.
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
        """_handshake, memoized on this Tools instance with a 10-minute TTL."""
        now = time.monotonic()
        cached = self._handshake_cache
        if cached and now < cached["expires_at"]:
            return cached["result"]
        result = await self._handshake()
        self._handshake_cache = {"result": result, "expires_at": now + HANDSHAKE_TTL_S}
        return result

    async def _resolve_namespace(self) -> tuple:
        """(namespace, source). The admin Valve is the DECLARED namespace, sent
        to POST /v1/handshake as project.declared_namespace; the server echoes
        it back verbatim unless an explicit pin overrides it. Fail-soft: any
        handshake error/timeout falls back to the valve value alone."""
        valve = sanitize_namespace(self.valves.namespace) or DEFAULT_NAMESPACE
        hs = await self._get_handshake()
        if hs and hs.get("namespace"):
            return str(hs["namespace"]), f"server:{hs.get('namespace_source', '')}"
        return valve, "valve"

    async def _headers(self, secret: str, extra: Optional[dict] = None) -> dict:
        """Build request headers — the single choke point every REST call goes
        through, so X-Memini-Home (and any future header) only needs wiring
        once."""
        namespace, _ = await self._resolve_namespace()
        headers = {
            "X-Memini-Namespace": namespace,
            **(extra or {}),
        }
        if secret:
            headers["Authorization"] = f"Bearer {secret}"
        home = str(self.valves.home or "").strip()
        if home:
            headers["X-Memini-Home"] = home
        return headers

    async def _post_json(self, path: str, payload: dict) -> Optional[dict]:
        base_url = str(self.valves.base_url).rstrip("/")
        secret = os.environ.get("MEMINI_API_KEY") or ""
        # Deliberate exception: this raise is meant to reach the calling tool method, matching @memini/client's assertBearerTransportSafe.
        if uses_plaintext_bearer(base_url, secret):
            message = (
                f"memini: MEMINI_API_KEY would cross plaintext HTTP to {base_url}; "
                "use HTTPS or an SSH tunnel."
            )
            if self.valves.require_https:
                raise RuntimeError(message)
            print(f"[memini] {message}")
        headers = await self._headers(secret, {"Content-Type": "application/json"})
        timeout = aiohttp.ClientTimeout(total=self.valves.timeout_ms / 1000)
        async with aiohttp.ClientSession(timeout=timeout) as session:
            async with session.post(
                f"{base_url}{path}", json=payload, headers=headers
            ) as res:
                if res.status >= 400:
                    body = await res.text()
                    raise RuntimeError(f"memini {path} failed: {res.status} {body}")
                return await res.json()

    async def _delete(self, path: str) -> bool:
        base_url = str(self.valves.base_url).rstrip("/")
        secret = os.environ.get("MEMINI_API_KEY") or ""
        # Deliberate exception: this raise is meant to reach the calling tool method, matching @memini/client's assertBearerTransportSafe.
        if uses_plaintext_bearer(base_url, secret):
            message = (
                f"memini: MEMINI_API_KEY would cross plaintext HTTP to {base_url}; "
                "use HTTPS or an SSH tunnel."
            )
            if self.valves.require_https:
                raise RuntimeError(message)
            print(f"[memini] {message}")
        headers = await self._headers(secret)
        timeout = aiohttp.ClientTimeout(total=self.valves.timeout_ms / 1000)
        async with aiohttp.ClientSession(timeout=timeout) as session:
            async with session.delete(f"{base_url}{path}", headers=headers) as res:
                if res.status >= 400:
                    body = await res.text()
                    raise RuntimeError(f"memini DELETE {path} failed: {res.status} {body}")
                return True

    async def memini_status(self, __event_emitter__=None) -> str:
        """
        Show the memini settings in force: which namespace memories are written
        to and recalled from, where that namespace came from (the configured
        valve, or a server-resolved handshake), and any misconfiguration worth
        flagging. Call this when the user asks what memini is doing, which
        namespace is in use, or why something saved earlier cannot be recalled —
        a namespace mismatch is the usual cause. Read-only; secrets are redacted.

        :return: The effective settings, with the provenance of each.
        """
        namespace, source = await self._resolve_namespace()
        base_url = str(self.valves.base_url).rstrip("/")
        secret = os.environ.get("MEMINI_API_KEY") or ""
        home = str(self.valves.home or "").strip()
        valve = sanitize_namespace(self.valves.namespace) or DEFAULT_NAMESPACE

        lines = [
            "memini — effective settings (Open WebUI tools)",
            f"project: {os.getcwd()}",
            "",
            "NAMESPACE",
            f"  {'effective':<24} {namespace:<30} <- {source}",
        ]
        # The provenance is the point: a value alone would not show that the
        # server resolved something other than the configured valve.
        if source != "valve" and namespace != valve:
            lines.append(f"  {'the namespace valve says':<24} {valve:<30} <- valve")
        lines += [
            f"  {'home (personal)':<24} {home or '(unset)'}",
            "",
            "CONNECTION",
            f"  {'base_url':<24} {base_url}",
            f"  {'api_key':<24} {redact_secret(secret) or '(unset)'}",
            f"  {'require_https':<24} {'1' if self.valves.require_https else '0'}",
            f"  {'recall_limit':<24} {self.valves.recall_limit}",
            f"  {'timeout_ms':<24} {self.valves.timeout_ms}",
            "",
        ]

        warnings = []
        if uses_plaintext_bearer(base_url, secret):
            warnings.append(
                f"  [!] plaintext-bearer: the API key is configured for plaintext HTTP to "
                f"{base_url}; the token and your memory payloads can be observed on the network. "
                "Use HTTPS or an SSH tunnel."
            )
        if not home:
            warnings.append(
                "  [i] home-unset: no personal namespace is configured, so no personal leg merges "
                "into recall."
            )
        lines.append("WARNINGS" if warnings else "No problems detected.")
        lines += warnings

        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": f"memini namespace: {namespace}", "done": True}}
            )
        return "\n".join(lines)

    async def recall_memory(self, query: str, __event_emitter__=None) -> str:
        """
        Search long-term memory for facts relevant to a query. Call this when the
        user refers to past conversations, prior decisions, or anything you might
        have been told before. Call before starting work that may have history —
        editing an unfamiliar file, debugging a recurring issue, or answering
        "what do we know about X".

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
            mem_id = str(mem.get("id") or "").strip()
            # Include the id so the model can forget_memory(id) a wrong/outdated hit.
            suffix = f" (id: {mem_id})" if mem_id else ""
            lines.append(f"- ({tier}) {text[:300]}{suffix}")
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": f"Recalled {len(lines)} memories", "done": True}}
            )
        header = "Relevant memories:"
        # /v1/search sets degraded="keyword_only" (plus a note) when the query
        # embed was unavailable and it fell back to keyword-only matching; the
        # field is already on `result`, so surfacing it is a one-line addition.
        if (result or {}).get("degraded"):
            note = (result or {}).get("note") or (
                "semantic search unavailable — these results are keyword-only and may be incomplete"
            )
            header += f" (degraded: {note})"
        return header + "\n" + "\n".join(lines)

    async def remember_memory(self, content: str, id: str = "", __event_emitter__=None) -> str:
        """
        Store a fact in long-term memory so it can be recalled in future
        conversations. Call this when the user shares a durable preference,
        decision, or fact worth remembering. To correct an existing memory,
        pass its id (as shown by recall_memory) — the write updates it in place.

        :param content: The fact to remember, written as a self-contained statement.
        :param id: Optional id of an existing memory to correct in place.
        :return: Confirmation that the memory was stored.
        """
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": "Storing memory…", "done": False}}
            )
        # No forced tier: this tool is for durable preferences/decisions/facts,
        # so let the server classify the content (a decision or preference lands
        # durable, chatter stays episodic) instead of pinning everything to one tier.
        body = {
            "content": content,
            "tags": ["openwebui"],
            "metadata": {"source": "openwebui"},
        }
        if id:  # POST /v1/memories upserts by id
            body["id"] = id
        await self._post_json("/v1/memories", body)
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": "Memory stored", "done": True}}
            )
        return "Stored in memory."

    async def forget_memory(self, id: str, __event_emitter__=None) -> str:
        """
        Permanently delete a memory from long-term memory by its id. Call this when
        a recalled memory is wrong, outdated, or poisoned. Get the id from
        recall_memory (each hit is annotated with its id). To correct a fact
        instead, call remember_memory with the existing id (it updates in place);
        forget only memories that should not exist at all.

        :param id: The id of the memory to forget, as shown by recall_memory.
        :return: Confirmation that the memory was forgotten.
        """
        if not id:
            return "No memory id provided."
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": "Forgetting memory…", "done": False}}
            )
        await self._delete(f"/v1/memories/{quote(str(id), safe='')}")
        if __event_emitter__:
            await __event_emitter__(
                {"type": "status", "data": {"description": "Memory forgotten", "done": True}}
            )
        return "Forgotten."
