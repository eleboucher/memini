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

import json
import os
import subprocess
from typing import Optional
from urllib.parse import quote, urlparse

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


def header_safe(value: str) -> str:
    """Trim and drop control characters (a CR/LF would split the header). Not
    sanitize_namespace: "/" survives, so a hierarchical override like acme/api
    reaches the server the way the other integrations send it rather than being
    flattened into a namespace nothing else reads."""
    return "".join(ch for ch in str(value).strip() if ch >= " " and ch != "\x7f")


def redact_secret(value: str) -> str:
    """A recognizable-but-useless fingerprint: enough to tell two tokens apart,
    not enough to use. Short values are elided entirely rather than
    half-revealed. Mirrors the redaction in packages/memini-client."""
    if not value:
        return ""
    return "***" if len(value) <= 12 else f"{value[:3]}…{value[-4:]}"


# --- Namespace override ---------------------------------------------------
#
# $XDG_CONFIG_HOME/memini/overrides.json holds the per-project namespace a user
# set deliberately — the same file the client plugins write and `memini doctor`
# reads. Every harness must honor it, or they disagree about which namespace is
# in force. Reimplemented here (a JSON file plus a `git rev-parse`) because a
# Tools file is a single upload into Open WebUI and can import nothing of memini's.


def overrides_path() -> str:
    xdg = os.environ.get("XDG_CONFIG_HOME", "")
    base = xdg if xdg.strip() else os.path.join(os.path.expanduser("~"), ".config")
    return os.path.join(base, "memini", "overrides.json")


def override_key(cwd: str) -> str:
    """The git toplevel when there is one, else the resolved directory."""
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
    """The override in effect for cwd, or None. The file is read before the key
    is computed (the key costs a `git rev-parse`), and any error degrades to "no
    override" — a broken overrides file must never raise into Open WebUI."""
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
            description="Project the memory is scoped to (X-Memini-Namespace). A per-project override in $XDG_CONFIG_HOME/memini/overrides.json wins over this.",
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

    def _resolve_namespace(self) -> tuple:
        """(namespace, source). Order: per-project override > the namespace valve.

        The override wins for the same reason it wins over MEMINI_NAMESPACE in
        every other integration: it is the one namespace input a user sets
        deliberately, and one that some harnesses honored and others ignored
        would be worse than none at all."""
        override = read_override(os.getcwd())
        if override:
            return override["namespace"], "override"
        valve = sanitize_namespace(self.valves.namespace)
        return valve or DEFAULT_NAMESPACE, "valve"

    def _headers(self, secret: str, extra: Optional[dict] = None) -> dict:
        """Build request headers — the single choke point every REST call goes
        through, so X-Memini-Home (and any future header) only needs wiring
        once."""
        headers = {
            "X-Memini-Namespace": self._resolve_namespace()[0],
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
        secret = os.environ.get("MEMINI_API_KEY") or os.environ.get("MEMINI_TOKEN") or ""
        if uses_plaintext_bearer(base_url, secret):
            message = (
                f"memini: MEMINI_API_KEY would cross plaintext HTTP to {base_url}; "
                "use HTTPS or an SSH tunnel."
            )
            if self.valves.require_https:
                raise RuntimeError(message)
            print(f"[memini] {message}")
        headers = self._headers(secret, {"Content-Type": "application/json"})
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
        secret = os.environ.get("MEMINI_API_KEY") or os.environ.get("MEMINI_TOKEN") or ""
        if uses_plaintext_bearer(base_url, secret):
            message = (
                f"memini: MEMINI_API_KEY would cross plaintext HTTP to {base_url}; "
                "use HTTPS or an SSH tunnel."
            )
            if self.valves.require_https:
                raise RuntimeError(message)
            print(f"[memini] {message}")
        headers = self._headers(secret)
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
        to and recalled from, where that namespace came from (a per-project
        override, or the configured valve), and any misconfiguration worth
        flagging. Call this when the user asks what memini is doing, which
        namespace is in use, or why something saved earlier cannot be recalled —
        a namespace mismatch is the usual cause. Read-only; secrets are redacted.

        :return: The effective settings, with the provenance of each.
        """
        namespace, source = self._resolve_namespace()
        override = read_override(os.getcwd())
        base_url = str(self.valves.base_url).rstrip("/")
        secret = os.environ.get("MEMINI_API_KEY") or os.environ.get("MEMINI_TOKEN") or ""
        home = str(self.valves.home or "").strip()
        valve = sanitize_namespace(self.valves.namespace) or DEFAULT_NAMESPACE

        lines = [
            "memini — effective settings (Open WebUI tools)",
            f"project: {override_key(os.getcwd())}",
            "",
            "NAMESPACE",
            f"  {'effective':<24} {namespace:<30} <- {source}",
        ]
        # The provenance is the point: a value alone would not show that an
        # override is masking the configured valve.
        if override:
            setat = f" (set {override['setAt']})" if override["setAt"] else ""
            lines.append(f"  {'without the override':<24} {valve:<30} <- valve{setat}")
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
            "PATHS",
            f"  {'overrides':<24} {overrides_path()}"
            + ("" if os.path.exists(overrides_path()) else " (absent)"),
            "",
        ]

        warnings = []
        if override:
            warnings.append(
                f"  [i] override-active: the namespace valve says \"{valve}\", but a per-project "
                f"override pins this project to \"{override['namespace']}\". Remove its entry from "
                f"{overrides_path()} to go back to the valve."
            )
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
