# context-optimiser

An instrument for answering one question: **is the context right at this point in
a conversation?** Not "did the code run" — whether what reached the model is what
should have reached it.

It is not a CI gate. Nothing depends on it, it runs only when asked, and its
output is a set of stages to read.

## Run it

```sh
LOOM_CONTEXT_OPTIMISER=1 GOWORK=off go test -tags fts5 ./test/context-optimiser/
```

No API key. The provider is scripted, so a route replays identically every time.

Artifacts land in `out/<route>/`:

| file | what it is |
|---|---|
| `RUN.md` | whether the run completed or halted, and at which turn |
| `stages.md` | every provider call, in order — the exact context dispatched |
| `context-dumps/` | the raw M9 records the stages were rendered from |

## How to read a run

1. Open `RUN.md`. If it says HALTED, a structural gate failed — that is the
   finding, and the stages after it were never produced.
2. If it completed, read `stages.md` against `expected/<route>.md`, stage by
   stage, and **stop at the first divergence**. Everything downstream of a wrong
   context is a consequence of it, not new information.

## Why the expectations are rules, not contexts

`expected/*.md` states what the context must be, as rules with the reason attached —
not a written-out copy of a correct context. A full expected context is mostly
duplicated text, goes stale the moment a route changes by one turn, and still only
judges the one message sequence it happened to encode. Rules are compact, portable
across routes, and carry the reasoning that makes a divergence arguable.

They state what the MODEL needs — a stable head, instructions it can act on, the
user's words intact, data at the size it is useful.

They are never generated from a run: generated expectations encode today's behaviour
as correct, faults included, and the suite goes green on a broken system forever.

## What the run checks by itself

Some facts need no judgement, so the route settles them inline and halts on the
first failure — a broken run then costs seconds and produces nothing to read:

- the route did not run past its script (past it, nothing is deterministic)
- every tool result has a `tool_use_id` and a call that precedes it
- no two consecutive user-role messages
- a turn actually changed the context (a stage identical to the one before it is
  the shape a silent no-op takes)
- a loaded skill's body appears exactly once — not zero times, not twice
- **restore matches live, after every turn**

That last one is the strongest check in the suite and needs no expectation file at
all: it compares two things the system itself produced — the live session, and the
same session replayed from durable storage. They must agree.

Everything else — whether bulk was relocated instead of conversation being
summarized, whether a folded decision kept its referent, whether anything stale is
still present — is left to the reading. Those are judgements, and a `diff` cannot
make them.

## Routes

- **lifecycle** — one session through every event a turn can produce: clean start,
  skill list, skill load, pattern load, ordinary work, a decision, a large result,
  recall, a parallel batch with a second load, sustained pressure, high-risk
  refusal.

Restart is not a separate route: it is checked after **every** turn of every route,
which is stricter than checking it once.

## Adding a route

A route is a list of turns in a `route_*_test.go` file: the user message, the
scripted provider responses it consumes, and optionally one structural check. Add
a matching `expected/<name>.md` stating what the context must be, and a test that calls
`runRoute`.

A scripted turn can also *derive* its tool call from the context it was handed —
that is how the recall turn finds the handle the run actually produced, without
the route hard-coding a value it cannot know.
