# The standard — tool-result-storage-design

What tool-result storage must be, taken from
`~/tera-code/designs/tool-result-storage-design.md`. The acceptance suite is
`storage_acceptance_test.go` (`-run TestStorage`); each clause names its test.
RED means the clause is not built yet — the suite is the build's driving light,
not its trophy.

The governing principle:
**tool results are ephemeral working data of the turn; conversation is the only
durable content. Payloads are re-made, never archived.**

---

## Storage — the DB holds receipts, never payloads

1. **A tool result's durable row is its settled stub from birth** — tool name,
   token figure, run-again wording, preview head. Payload bytes never reach the
   database, small or large. → `Arrival_DBHoldsSettledStubNeverPayload`
2. **The row is written once.** Pressure reshapes the render, never the row —
   there is no `evicted` column, no shed-UPDATE, no crash window.
   → `Arrival_RowNeverRewrittenUnderPressure` (green invariant)
3. **Nothing tool-result-shaped is written to local disk.** No loom.db, no
   overflow cache, no index file. → `Index_NothingOnDisk`

## The window — retention is a render policy, K = 1

4. **In its own turn and the next (distance ≤ 1)**: a small result renders
   whole; a large one renders as the offloaded stub and its payload is loadable.
   → `Window_SmallSettlesAtDistanceTwo`, `Retrieval_LargeLoadableAtDistanceOne`
   (green — the promise that must survive the fix)
5. **At distance 2 the result settles**: the render becomes the stored stub,
   the payload is gone from memory. → `Window_SmallSettlesAtDistanceTwo`
6. **The dial changes renders, never storage.** DB rows are byte-identical at
   every K. → `Dial_StorageIdenticalAcrossK` (skips until the dial exists,
   then binds it)

## Stubs — two forms, one stored

7. **Settled form**: names the originating tool, carries a token figure sized
   to the ORIGINAL payload, says run-again, keeps the preview head, offers no
   load path. → `Stub_SettledAnatomyAndTokenFigure`
8. **No nesting**: the stored stub is emitted verbatim, never re-derived — a
   stub of a stub, or a token figure that collapsed to stub-size, is a renderer
   defect. → `Stub_SettledAnatomyAndTokenFigure`
9. **Offloaded form**: advertises `recall("msg:<id>")`, and for tabular data
   also the query path. → `Arrival_DBHoldsSettledStubNeverPayload`,
   `Stub_OffloadedTabularAdvertisesQueryPath`

## Retrieval — the window is the boundary

10. **In-window load works**: recall pages an offloaded payload back as a tool
    result. → `Retrieval_LargeLoadableAtDistanceOne`
11. **Settled miss is honest**: recall of a settled result returns no payload
    and names the tool to re-run. → `Retrieval_RecallMissNamesToolAfterSettle`
12. **Query addresses are message ids** — the id the stub shows, no generated
    ref-id namespace. → `Retrieval_QueryByMsgAddress`
13. **The query index outlives the window in-session**: SELECTs answer after
    the context shed the payload. → `Retrieval_QueryIndexOutlivesWindow`
14. **The index dies with the session**: after reload it answers gone/re-run,
    never data. → `Retrieval_QueryGoneAfterReload`
15. **Ranges are honest**: `recall("msg:a-b")` returns conversation whole and
    tool rows as their stub line. → `Retrieval_RangeReturnsStubLineForToolRows`
16. **No resurrected surfaces**: `get_tool_result` and `conversation_memory`
    stay dead; `recall` and `query_tool_result` are advertised.
    → `Toolset_NoResurrectedSurfaces`

## Pressure — within the turn, and almost never across turns

17. **A long agentic turn relieves itself**: current-turn tool results are
    evictable oldest-first even inside the working set (memory-only); the
    boundary user message is untouchable; no terminal error while tool bulk
    remains sheddable. → `Pressure_LongTurnRelievesWithoutTerminal`
18. **Cross-turn pressure goes quiet**: bulk self-settles at the K-boundary,
    so no eviction wording and no folds appear on a route whose conversation
    is small. → `Pressure_RareAcrossTurns`
19. **Cache shape**: turn over turn, the render changes only at the K-boundary
    settling region; everything ahead of it is byte-stable.
    → `Cache_PrefixStableExceptKBoundary`

## Reload — read + render, born correct

20. **A restored session carries conversation whole and tool rows as settled
    stubs** — zero payload bytes anywhere in the rebuilt render.
    → `Reload_SettledStubsNoPayloadBytes`
21. **A mid-window restart forfeits the window gracefully**: the in-window
    result comes back SETTLED (re-run form), never an offloaded stub whose
    load cannot resolve. → `Reload_MidWindowDegradesToSettled`

---

## Cloud-side checklist (tera-backend — tests to land with the fix)

Not runnable from this suite; each item is a unit/integration test the cloud
fix must carry:

C1. `session_messages.seq` — identity column, unique, **backfilled ordered by
    (session_id, created_at)**; loads ORDER BY seq. A pre-migration session's
    addresses must agree with conversation order.
C2. Schema carries `folded` only — a test asserts **no `evicted` column**
    exists, so nobody re-adds pressure-time DB writes.
C3. Adapter implements `SaveMessageReturningID` (INSERT … RETURNING seq) and
    `MarkMessagesFolded`; `cloudMessageToLoom` maps seq → Message.ID + folded.
C4. Single writer per path: session_service writes user/assistant rows, loom
    memory writes tool/memory rows; both yield seq; no duplicate rows after a
    full SendMessage round-trip.
C5. Threshold mapping: `-1` → inline everything, `0` → loom default (64 KiB),
    `>0` → literal. Asserted at the ChatExecutor boundary.
C6. `SharedMemoryStore` constructed once per ChatExecutor — goroutine count
    stays flat across N SendMessage calls.
C7. MCP bridge parses result text that is valid `{columns, rows}` JSON into
    the structured map; a Teradata-shaped string result reaches the query
    index end-to-end.
C8. `get_tool_result` no longer force-registered; `conversation_memory` gone.
C9. Session affinity: with in-process session reuse, the K-window holds across
    two SendMessage calls (previous turn's payload loadable); after a process
    restart, the same call settles gracefully (C-side mirror of clauses 4/21).

---

## Current ledger (2026-08-01, loom-context @ 40201b11)

RED (14): clauses 1, 3, 5, 7, 8, 9(tabular), 11, 12, 13, 15, 16(query missing),
17, 18, 20, 21 — the designed behaviour, not yet built.
GREEN (4): clause 2 (invariant), clause 10 (in-window promise — must stay
green), clause 14 (vacuous until the index exists), clause 19 (weak until
settling exists — strengthens itself as the fix lands).
SKIP (1): clause 6 — binds automatically when the retention dial appears.
