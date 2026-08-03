# The standard — loom-context-hld

What the context must be, taken from the HLD. Read `out/<route>/stages.md` against this.

The governing principle, and the one to fall back on when a case isn't listed:
**shed the reproducible before compressing the irreplaceable.**

---

## Structural — every stage

1. **The system slot is exactly two messages**: ROM, then the L2 block — L2 as its *own*
   system message, never concatenated into ROM. ROM must stay byte-stable so the provider's
   prefix cache holds; the L2 message is the only system-slot element allowed to change.
2. **The L2 block renders on every call once any entry exists, and never disappears.** Even
   when every entry is demoted, its short line still renders. A model cannot ask for what it
   does not know is missing.
3. **The context is a render of the log.** Nothing is removed from the log, ever. L1 is the
   `!folded` rows; each renders full or as a stub.
4. **Addresses are absolute and self-describing.** Anything recallable displays its own
   address where the model learns it exists — `msg:<id>` in stubs, `fold:<n>` in short
   lines, `msg:<a>-<b>` in long summaries. The model copies, never constructs.
5. **Tool-pair boundaries hold**: a tool result is never separated from its call, and a
   parallel batch's results stay together.

## Arrival — nothing happens

6. **A tool result enters the message whole, always.** No size check, no store write, no
   content replacement. The row holds the payload; the row is the store.
7. Therefore: for any tool result, the durable message content and the payload the tool
   returned are identical, at any size.

## Rendering

8. **A message renders as a stub when its content is ≥ the arrival threshold in bytes, or
   when it is flagged `evicted`.** Size is a condition — recomputable, same answer every
   render. `evicted` is an event — recorded, never cleared.
9. **The stub reports tokens, its address, and a preview of the payload** — the model must
   be able to judge relevance without fetching. A stub that describes the storage operation
   instead of the data is a defect; the preview is of the content.
10. **A stub costs stub-sized tokens.** Pressure is computed over the rendered view, so an
    evicted payload contributes its stub, not its payload. With a raw basis eviction frees
    nothing and the walk cannot terminate.

## The decision point

11. **Nothing mutates context state except `releasePressure`, before dispatch.** No
    compaction at admission, none at reload. Adding a message must never change what any
    earlier message renders as.
12. **Order is fixed: evict, then fold.** Fold only runs when eviction is exhausted and usage
    is still above the fold mark.
13. **The walk goes oldest → newest, stops at the evict mark or the working set**, and each
    flag is written synchronously at set time — a crash between decision and dispatch must
    reload the same context that was about to be sent.

## The working set — never evicted, never folded

14. **The boundary is the user message that entered through `Chat()`**, not the newest
    user-role message. A skill-body sidecar is user-role and **must not move the boundary**.
15. Everything from the boundary onward is protected, including recalled content, which is
    inside by position rather than by rule.

## Fold

16. **Selection is token-sized to reach the evict mark** — oldest `!folded` first, never into
    the working set, no pin, nothing spliced out.
17. **The compressor reads the batch as rendered** — stubs, not payloads. It summarizes
    conversation; it must never be handed a wall of tool output.
18. **One pass produces one entry with two texts**: a long summary (state of the work —
    what was done, what came of it, what is settled, what was ruled out, with message ids
    for anything worth exact retrieval) and a short line (the phase in one line, prefixed
    with the fold number). Both persist at fold time; the entry never changes afterwards.
19. **The batch is flagged `folded`. Nothing is removed.**
20. **Terminal:** eviction exhausted, fold at the working-set edge, still over the fold mark
    → no dispatch. `Chat()` returns the recoverable error carrying usage, the marks, and
    that both reliefs are exhausted. Never a silent trim, never a request the provider will
    reject.

## L2

21. **Rendering is recomputed every call, stored nowhere**: walk entries newest → oldest
    taking long summaries until `maxL2Tokens` would be passed; that entry and every older
    one contribute their short line instead. Emit oldest-first.
22. **Demotion is not an operation.** No flag, no row touched, nothing fires at the
    threshold — the next render simply picks the short line.
23. **A demoted long never re-enters the block.** It is reachable only by `recall(fold:N)`,
    which returns it as a tool result.

## Recall

24. **One call, `recall(address)`.** `fold:N` → that entry's long summary. `msg:<id>` → that
    payload. `msg:<a>-<b>` → those messages.
25. **Every return is an ordinary tool result at the tail** — in the working set now,
    evictable later. Nothing is copied into a privileged slot; no promoted slot exists.
26. **The stub stays a stub.** Recall never clears `evicted`; there is no unflag mechanism.
27. **Addresses resolve only within the session.** Another session's ids never resolve.

## Kernel

28. **The advertised tool schemas are counted in the budget** — as accounting only, never
    rendered into the message list. The count is recomputed when this session's skill loads
    or disclosures change the projection.

## Reload

29. **Reload is read + render, nothing else.** No replay, no compaction, no compressor call.
30. **Live and restored are identical by construction** — the same read of the same rows.
    Any divergence means something other than the log is holding state.
31. Pre-migration sessions (no flags, no entries) load fully live and take their first
    pressure event normally.

---

## What must no longer exist

Arrival content-replacement (`handleLargeResult`'s rewrite, the `formatToolResult`
equivalent) · the promoted-context slot and `conversation_memory` · `evictL1Prefix` and the
pin · the `AddMessage` compression block · the `ReplayMessages` compaction loop ·
`evictL2ToSwap` and the L2 wipe · `CompactMemory`'s wholesale semantics · offset-based
message paging · `session_memory` as a read surface.

A route that still observes any of these is testing a system that has not been built yet.
