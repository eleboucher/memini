---
name: pin
description: Pin or unpin a memini memory while preserving its existing tags. Use when the user wants a memory in every briefing.
---

# pin (memini)

Locate the memory with `memory_get` or `memory_recall` and copy its id and
namespace verbatim. Read its current tags: `memory_update` replaces rather than
merges the tag list.

To pin, call `memory_update` with the existing tags plus `pinned`, preserving
order and avoiding duplicates. To unpin, pass the existing tags minus `pinned`.
If several candidates match, ask the user which one. Report the updated memory.

Pins are exempt from retro-tiering demotion, but not confidence decay or the
briefing token/item budget. Warn when the pinned set is becoming large.
