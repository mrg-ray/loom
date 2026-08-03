package contextoptimiser

import (
	"context"
	"fmt"
	"testing"
)

// §Eviction-4: recalled content is in the working set THIS turn, then becomes an
// ordinary old tool result — evictable like any other. Verify it is actually
// evicted under later pressure rather than surviving forever.
func TestEviction_RecalledBecomesEvictable(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{summary: "STATE"}
	r := newRig(t, routeOutDir(t, "probe4"), nil, 8000, 1000, 6000)
	r.setCompressor(comp)
	r.setMaxL2Tokens(300)
	sid := "probe4"
	ctx := context.Background()

	// oversize arrival → recall it → then sustained pressure
	r.llm.turns = append(r.llm.turns,
		callTool("big", emitToolName, sized(20000)), sayText("stored"),
		scriptedTurn{derive: recallByMsgAddress}, sayText("recalled"))
	r.agent.Chat(ctx, sid, "pull scan")
	r.agent.Chat(ctx, sid, "what was in it?")

	recalledLenAfter := func() (full int, stub int) {
		for _, m := range renderedContext(t, r, sid) {
			if m.Role == "tool" && countScanRows(m.Content) > 50 {
				full++
			}
		}
		return
	}
	f0, _ := recalledLenAfter()
	t.Logf("right after recall: %d full scan-row tool messages in render", f0)

	// sustained pressure — the recalled payload should get evicted
	for i := 0; i < 20; i++ {
		r.llm.turns = append(r.llm.turns, sayText("ack"))
		r.agent.Chat(ctx, sid, heavyConversation(i))
	}
	fN, _ := recalledLenAfter()
	sm := segMemOf(t, r.agent, sid)
	usage := statFloat(sm.GetMemoryStats(), "budget_usage_pct")
	t.Logf("after 20 pressure turns (usage=%.1f%%): %d full scan-row tool messages remain", usage, fN)
	if fN > 0 && usage > 88 {
		t.Errorf("a full scan-row payload survives at %.1f%% usage — recalled/oversize content not evicted under pressure", usage)
	}
	fmt.Println()
}

func countScanRows(s string) int {
	n, idx := 0, 0
	for {
		j := indexFrom(s, "scan-row", idx)
		if j < 0 {
			return n
		}
		n++
		idx = j + 8
	}
}

func indexFrom(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	i := 0
	for i = from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
