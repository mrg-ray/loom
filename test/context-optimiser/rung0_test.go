// Copyright 2026 Teradata
//
// rung 0 — relief inside the current turn. A single-shot session (CLI) is one
// turn forever: the old ladder's boundaries (t−K … t−1) match nothing, so
// before rung 0 such a session had no working pressure valve. On this design
// the valve is the in-loop fold: at the start mark the next call carries the
// fold instruction, the model answers with its own summary, and the harness
// collapses the context around it. Pressure relief (evict, then compressor
// fold) is the failsafe, entered only after the model's reply was unusable.
// These routes prove that end-to-end: the fold applies at the mark, the
// summary renders as conversation (never in the system slot), the pending
// pair always survives, settled turns collapse behind the summary, and the
// failsafe engages when — and only when — the model's reply is unusable.
//
//	LOOM_CONTEXT_OPTIMISER=1 go test -tags fts5 -run TestRung0 ./test/context-optimiser/ -v

package contextoptimiser

import (
	"fmt"
	"strings"
	"testing"
)

// rung0Budget is the growth budget G (MaxTokens is G — an absolute, not a
// fraction of a provider window). The scenarios below are sized to cross it.
const rung0Budget = 8000

// summaryHeader opens the rendered L2 summary — a user-role reminder at the
// head of the conversation region, never a system block.
const summaryHeader = "Session summary — the work so far"

func summaryRows(s stage) (user, system int) {
	for _, m := range s.Messages {
		if !strings.Contains(m.Content, summaryHeader) {
			continue
		}
		switch m.Role {
		case "user":
			user++
		case "system":
			system++
		}
	}
	return
}

func containsAnywhere(stages []stage, role, needle string) bool {
	for _, s := range stages {
		for _, m := range s.Messages {
			if (role == "" || m.Role == role) && strings.Contains(m.Content, needle) {
				return true
			}
		}
	}
	return false
}

func assertMarkerBudget(t *testing.T, stages []stage) {
	t.Helper()
	for si, s := range stages {
		markers := 0
		for _, m := range s.Messages {
			if m.CacheBreakpoint {
				markers++
			}
		}
		if markers > 4 {
			t.Errorf("stage %d: %d cache markers — over Anthropic's budget of 4", si, markers)
		}
	}
}

// assertFoldApplied — the model answered the fold instruction, and the
// collapsed context the harness dispatched next carries the summary as
// conversation: one user-role reminder, nothing in the system slot.
func assertFoldApplied(t *testing.T, r *rig, stages []stage) {
	t.Helper()
	if r.llm.foldCount() == 0 {
		t.Fatal("the fold instruction was never sent — the in-loop fold did not arm at the mark")
	}
	final := stages[len(stages)-1]
	user, system := summaryRows(final)
	if user != 1 {
		t.Errorf("final context carries %d user-role summary rows, want exactly 1", user)
	}
	if system != 0 {
		t.Errorf("summary rendered in the system slot %d time(s) — it is conversation, not system", system)
	}
	if containsAnywhere(stages, "", "covers msg:") {
		t.Error("a 'covers msg' line appeared — that is the compressor fold's citation; the in-loop fold writes none")
	}
}

// newestToolRowWhole — the pending pair survives every fold: the newest tool
// row in the final context renders its payload, never a stub or a casualty.
func newestToolRowWhole(final stage) bool {
	var last string
	for _, m := range final.Messages {
		if m.Role == "tool" {
			last = m.Content
		}
	}
	return last != "" && strings.Contains(last, "scan-row")
}

// TestRung0_SingleTurnEvictShedsToTarget — one turn, consumed mid-size results
// push past the mark mid-drive. The in-loop fold fires first and sheds enough
// on its own: no eviction stub is ever dispatched, the run completes, and the
// cache marker budget holds in every dispatched context.
func TestRung0_SingleTurnEvictShedsToTarget(t *testing.T) {
	requireGate(t)
	r := newRig(t, routeOutDir(t, "rung0-evict"), nil, rung0Budget, 2000, 16384)
	sid := "rung0-evict"

	turns := make([]scriptedTurn, 0, 7)
	for i := 0; i < 6; i++ {
		turns = append(turns, callTools(emit(fmt.Sprintf("r%d", i), 6000, "string")))
	}
	turns = append(turns, sayText("done"))
	if err := drive(t, r, sid, "one long task", turns...); err != nil {
		t.Fatal(err)
	}

	stages := r.readStages(t)
	if len(stages) < 5 {
		t.Fatalf("want ≥5 provider calls, got %d", len(stages))
	}
	assertFoldApplied(t, r, stages)
	if containsAnywhere(stages, "tool", "evicted from context") {
		t.Error("an evicted stub was dispatched — the in-loop fold should have relieved pressure before the failsafe ran")
	}
	assertMarkerBudget(t, stages)
}

// TestRung0_FoldLastResort — one turn whose mass is assistant reasoning text
// with only floor-protected tiny results: nothing is evictable, so the fold is
// the only valve. The model's own summary is that fold, and the pending pair
// survives it whole.
func TestRung0_FoldLastResort(t *testing.T) {
	requireGate(t)
	r := newRig(t, routeOutDir(t, "rung0-fold"), nil, rung0Budget, 2000, 16384)
	sid := "rung0-fold"

	reasoning := strings.Repeat("thinking through the step in detail. ", 55) // ~2k chars, unevictable
	turns := make([]scriptedTurn, 0, 24)
	for i := 0; i < 23; i++ {
		st := callTools(emit(fmt.Sprintf("t%d", i), 300, "string"))
		st.text = reasoning
		turns = append(turns, st)
	}
	turns = append(turns, sayText("done"))
	if err := drive(t, r, sid, "many small steps", turns...); err != nil {
		t.Fatal(err)
	}

	stages := r.readStages(t)
	assertFoldApplied(t, r, stages)
	if !newestToolRowWhole(stages[len(stages)-1]) {
		t.Error("the newest result did not survive the folds whole — pending protection failed")
	}
}

// TestRung0_MultiTurnOldRungsSuffice — settled turns carry the mass, the
// current turn is small. The fold is a full collapse of the settled turns:
// their call/result pairs leave the context and the summary stands in for
// them, their operator rows and assistant text stay — and the current turn
// is never touched: every one of its consumed results renders whole (the
// equivalence lock, kept by the new mechanism).
func TestRung0_MultiTurnOldRungsSuffice(t *testing.T) {
	requireGate(t)
	r := newRig(t, routeOutDir(t, "rung0-equiv"), nil, rung0Budget, 2000, 16384)
	sid := "rung0-equiv"

	if err := drive(t, r, sid, "big turn one",
		callTools(emit("old1", 40000, "string")), sayText("ok")); err != nil {
		t.Fatal(err)
	}
	if err := drive(t, r, sid, "big turn two",
		callTools(emit("old2", 40000, "string")), sayText("ok")); err != nil {
		t.Fatal(err)
	}
	if err := drive(t, r, sid, "current turn",
		callTools(emit("cur", 3000, "string")),
		callTools(emit("cur2", 3000, "string")),
		sayText("done")); err != nil {
		t.Fatal(err)
	}

	stages := r.readStages(t)
	assertFoldApplied(t, r, stages)
	final := stages[len(stages)-1]
	var curWhole, stubs int
	for _, m := range final.Messages {
		if m.Role != "tool" {
			continue
		}
		switch {
		case strings.Contains(m.Content, "evicted from context"), strings.Contains(m.Content, "held in memory"):
			stubs++
		case strings.Contains(m.Content, "scan-row"):
			curWhole++
		}
	}
	if stubs != 0 {
		t.Errorf("%d stub rows remain in the final context — settled turns' pairs collapse behind the summary, they do not linger as stubs", stubs)
	}
	if curWhole != 2 {
		t.Errorf("current turn renders %d whole results, want 2 — the collapse must never touch the current turn", curWhole)
	}
	assertMarkerBudget(t, stages)
}

// TestRung0_FailsafeWhenReplyUnusable — the model answers the fold instruction
// with something that is not a summary. After the retries the pressure path
// takes over: evictable results are shed as stubs, and the run still completes.
func TestRung0_FailsafeWhenReplyUnusable(t *testing.T) {
	requireGate(t)
	r := newRig(t, routeOutDir(t, "rung0-failsafe"), nil, rung0Budget, 2000, 16384)
	r.setCompressor(&countingCompressor{})
	r.llm.foldGarbage = true
	sid := "rung0-failsafe"

	turns := make([]scriptedTurn, 0, 7)
	for i := 0; i < 6; i++ {
		turns = append(turns, callTools(emit(fmt.Sprintf("f%d", i), 6000, "string")))
	}
	turns = append(turns, sayText("done"))
	if err := drive(t, r, sid, "one long task", turns...); err != nil {
		t.Fatal(err)
	}

	stages := r.readStages(t)
	if got := r.llm.foldCount(); got < 2 {
		t.Fatalf("fold instruction answered %d time(s) — the harness re-asks once before falling back", got)
	}
	if !containsAnywhere(stages, "tool", "evicted from context") {
		t.Error("no evicted stub ever dispatched — the pressure failsafe did not engage after the unusable replies")
	}
	_, system := summaryRows(stages[len(stages)-1])
	if system != 0 {
		t.Errorf("a summary rendered in the system slot %d time(s) under the failsafe — it is conversation on every path", system)
	}
	assertMarkerBudget(t, stages)
}
