package contextoptimiser

import (
	"context"
	"fmt"
	"testing"
)

// §10/§12: at genuinely high usage, eviction clears ALL evictable payload
// outside the working set — the reproducible is shed before the irreplaceable.
func TestEviction_ClearsAtHighUsage(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{summary: "STATE"}
	r := newRig(t, routeOutDir(t, "probe3"), nil, 8000, 1000, 6000)
	r.setCompressor(comp)
	r.setMaxL2Tokens(300)
	sid := "probe3"
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		if i%2 == 0 {
			r.llm.turns = append(r.llm.turns, callTool(fmt.Sprintf("c%d", i), emitToolName, sized(3000)), sayText("r"))
		} else {
			r.llm.turns = append(r.llm.turns, sayText("ack"))
		}
		r.agent.Chat(ctx, sid, heavyConversation(i))
		sm := segMemOf(t, r.agent, sid)
		st := sm.GetMemoryStats()
		usage := statFloat(st, "budget_usage_pct")
		ws := statInt(st, "working_set_start")
		if usage > 85 {
			// find evictable payloads OUTSIDE the working set that survived
			msgs := durableMessages(t, r, sid)
			survivors := 0
			for j, m := range msgs {
				if j < ws && m.Role == "tool" && len(m.Content) > 1000 && !m.Folded && !m.Evicted {
					survivors++
				}
			}
			if usage > 88 && survivors > 0 {
				t.Errorf("turn %d usage=%.1f%%: %d evictable payload(s) survive outside the working set — eviction did not shed available bulk", i, usage, survivors)
			}
		}
	}
}
