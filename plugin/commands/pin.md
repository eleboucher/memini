---
description: Pin a memini memory so it surfaces in every session briefing
argument-hint: "<memory id, or a description of the memory>"
---

Pin a memory so it is injected into **every** future session briefing, is exempt
from confidence decay, and is never dropped by the injection token budget.

Target: $ARGUMENTS

Steps:

1. **Find it.** If the argument is a memory id, call `memory_get` with it. If it is
   a description, call `memory_recall` first and — unless exactly one result is an
   obvious match — show the candidates and ask which one. Copy the `namespace` field
   off the result verbatim; never construct one.

2. **Read its current tags.** This step is not optional. `memory_update` **replaces**
   the tag list rather than merging into it, so passing `tags: ["pinned"]` would
   silently delete every other tag the memory has.

3. **Call `memory_update`** with the id, its namespace, and `tags` set to the
   existing tags **plus** `"pinned"`. Preserve the original order and do not add
   `"pinned"` twice if it is already there — say it was already pinned and stop.

To **unpin**, do the same but pass the existing tags _minus_ `"pinned"`.

Then tell the user what is now pinned and warn them if the pinned set is getting
large: the briefing's pinned budget defaults to 5 (`MEMINI_INJECT_BRIEFING_PINNED`),
so pinning more than that means some pins stop surfacing — and which ones drop is
not something the user chose. Pins are for durable identity and preferences, not
for anything merely important.
