# Fix automatic recall routing and compatibility fallback

Status: ready-for-agent
Type: bug
Blocked by: None

## What to build

Automatic prompt recall must use the same server-authoritative project namespace as capture and explicit memory tools, and compatibility fallback must distinguish an old server from a transient failure.

## Acceptance criteria

- [ ] Every automatic search attempt carries the resolved namespace, including the `exclude_ids` retry path.
- [ ] Server-side exclusion is disabled only when the server specifically rejects the unsupported field, not after timeouts or unrelated 5xx responses.
- [ ] Regression tests assert the namespace header and transient-failure behavior.
- [ ] Typechecking passes and is capable of catching a future missing-namespace call.

## Comments
