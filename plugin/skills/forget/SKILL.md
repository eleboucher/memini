---
name: forget
description: Delete a specific memini memory safely. Use when the user asks to forget, remove, or permanently delete stored memory.
---

# forget (memini)

Find the target first with `memory_get` when given an id, or `memory_recall`
when given a description. Copy the returned `namespace` verbatim.

Before deletion, show the full memory content and obtain explicit user
confirmation. Never delete more than one memory per confirmation, and never
guess between multiple candidates. Only after confirmation call
`memory_forget` with the id and returned namespace.

Prefer `memory_update` when the fact is merely wrong or outdated because it
preserves history. After deletion, say that the memory is permanently removed
from memini and may still remain in the current chat transcript.
