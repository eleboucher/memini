# Fix automatic recall routing and compatibility fallback

Status: resolved
Type: bug
Blocked by: None

## What to build

Automatic prompt recall must use the same server-authoritative project namespace as capture and explicit memory tools, and compatibility fallback must distinguish an old server from a transient failure.

## Acceptance criteria

- [x] Every automatic search attempt carries the resolved namespace, including the `exclude_ids` retry path.
- [x] Server-side exclusion is disabled only when the server specifically rejects the unsupported field, not after timeouts or unrelated 5xx responses.
- [x] Regression tests assert the namespace header and transient-failure behavior.
- [x] Typechecking passes and is capable of catching a future missing-namespace call.

## Comments

Resolved on `fix/pi-plugin-parity`.

Validation evidence:

- Automatic search now requires the resolved namespace on the initial request and compatibility retry, with `exclude_ids` capped before transport.
- Compatibility downgrade occurs only after an explicit HTTP 400 unsupported/unknown `exclude_ids` response and a successful same-namespace fallback; timeout, 429, 500, and unrelated 400 tests prove no retry or sticky downgrade.
- `npm run typecheck` passes against Pi 0.80.6, so missing namespace arguments remain compile-time failures.
- `npm test` passes, including namespace-header and transient-failure regressions.

Final integration evidence:

- Active-`exclude_ids` regressions now prove `MEMINI_FALLBACK=0` throws for timeout, 429, 500, unrelated 400, and a failed compatibility retry.
- Explicit tools and lifecycle digests retry authority once and never send a request under a locally derived namespace after handshake failure.
