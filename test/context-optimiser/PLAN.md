# context-optimiser — suite plan for the ContextCompilation design

The suite for `loom-context-and-tool-result-hld.md`. The harness in `_legacy/`
(rig, scripted LLM, gates, dump/stage plumbing — 1,086 lines with no test
functions) is reused; the legacy *assertions* are not — they test deleted
mechanisms (K-window, settling, sidecar, maxL2Tokens, fold-entry lists,
proactive marks). This plan is the complete list of what replaces them. It is
written before the implementation exists; each test lands with the mutation
step that makes it compilable (mutation doc §B7 order).

## Instrument, unchanged

The README's contract stands: this answers "is the context right at this point
in a conversation?" — stages out, rules in `expected/`, stop at the first
divergence. `expected/hld.md` is rewritten to the new design's rules; the
legacy expectations are history.

## Harness extensions (rig)

1. **Refusal injection** — the single most important new capability. The
   scripted LLM gains `refuse(n)`: the next n Chat calls return the typed
   context-too-long error. Relief is then testable deterministically: no
   token stuffing, no marks, exact replay.
2. **Scripted compressor** returning a fixed summary text (exists as
   `countingCompressor`; gains a canned-output mode).
3. `emit(id, size)` unchanged — payload generator.
4. Gate set unchanged; `restoreMatchesLive` becomes the headline gate.

## The tests

### T1 — Compile render cases (HLD §5.2 steps 1–8)
One route, six assertions on the compiled context:
- conversation rows always full; never stubbed at any size.
- tool row `evicted=true` → evicted stub, byte-exact:
  `[%s result, ~%d tokens, evicted from context — re-run the call above if this data is needed again.%s\n preview: %s]`
- tool row > threshold, `turn = T` → offload stub, byte-exact:
  `[%s result, ~%d tokens, held in memory this turn — query_tool_result(message_id=%d, offset=0, limit=100) to read it; sql="SELECT ... FROM results" for tabular data.%s\n preview: %s]`
- tool row > threshold, `turn < T` (synthetic legacy row) → evicted stub.
- at-threshold row renders full (strictly-over rule — a page never stubs itself).
- preview = first 160 bytes, whitespace collapsed, rune-cut; JSON payload
  yields a content preview, never `[`.

### T2 — Or