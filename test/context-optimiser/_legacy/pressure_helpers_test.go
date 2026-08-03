// Copyright 2026 Teradata
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Shared pressure-probe helpers used by the HLD acceptance and full-lifecycle
// suites: a counting compressor, per-turn probes, and pressure-driving rigs.
package contextoptimiser

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/types"
)

// ---------------------------------------------------------------------------
// Probe — one reading of a session's memory state, taken after each turn.
// ---------------------------------------------------------------------------

type probe struct {
	turn       int
	msgs       int
	l1Tokens   int
	budgetPct  float64
	l2Len      int
	swapEvicts int
	compress   int // compressor invocations so far
}

func (p probe) String() string {
	return fmt.Sprintf("turn %2d: msgs=%3d l1tok=%6d budget=%5.1f%% l2=%5d swapEvict=%d compressCalls=%d",
		p.turn, p.msgs, p.l1Tokens, p.budgetPct, p.l2Len, p.swapEvicts, p.compress)
}

// countingCompressor stands in for the LLM compressor. It records every call, so
// a test can prove how often compaction actually ran — including on reload — and
// returns a summary of a controllable size.
type countingCompressor struct {
	mu       sync.Mutex
	calls    int
	summary  string
	disabled bool
}

func (c *countingCompressor) CompressMessages(_ context.Context, msgs []types.Message) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.summary != "" {
		return c.summary, nil
	}
	return fmt.Sprintf("[compressed %d messages]", len(msgs)), nil
}

func (c *countingCompressor) IsEnabled() bool { return !c.disabled }

func (c *countingCompressor) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// segMem reaches the session's tiered memory through exported API.
func segMemOf(t *testing.T, a *agent.Agent, sessionID string) *agent.SegmentedMemory {
	t.Helper()
	s, ok := a.GetSession(sessionID)
	if !ok {
		t.Fatalf("session %s not found", sessionID)
	}
	sm, ok := s.SegmentedMem.(*agent.SegmentedMemory)
	if !ok {
		t.Fatalf("session %s carries no SegmentedMemory", sessionID)
	}
	return sm
}

func statInt(stats map[string]interface{}, key string) int {
	switch v := stats[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func statFloat(stats map[string]interface{}, key string) float64 {
	switch v := stats[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// readProbe takes one reading.
func readProbe(t *testing.T, a *agent.Agent, sessionID string, turn int, comp *countingCompressor) probe {
	t.Helper()
	sm := segMemOf(t, a, sessionID)
	stats := sm.GetMemoryStats()
	evicts, _ := sm.GetSwapStats()
	p := probe{
		turn:       turn,
		msgs:       statInt(stats, "l1_message_count"),
		l1Tokens:   statInt(stats, "l1_token_count"),
		budgetPct:  statFloat(stats, "budget_usage_pct"),
		l2Len:      len(sm.GetL2Summary()),
		swapEvicts: evicts,
	}
	if comp != nil {
		p.compress = comp.count()
	}
	return p
}

// pressureRig drives N turns of mid-sized tool results and probes after each,
// which is the shape most of these defects need.
func pressureRig(t *testing.T, name string, turns, payload int, comp *countingCompressor,
	maxCtx, reserved, offload int) (*rig, string, []probe) {
	t.Helper()

	script := make([]scriptedTurn, 0, turns*2)
	for i := 0; i < turns; i++ {
		script = append(script,
			callTool(fmt.Sprintf("call-%d", i), emitToolName,
				map[string]interface{}{"bytes": float64(payload), "shape": "string"}),
			sayText(fmt.Sprintf("segment %d done", i)))
	}

	r := newRig(t, routeOutDir(t, name), script, maxCtx, reserved, offload)
	if comp != nil {
		r.setCompressor(comp)
	}
	sid := "defect-" + name
	ctx := context.Background()

	var probes []probe
	for i := 0; i < turns; i++ {
		if _, err := r.agent.Chat(ctx, sid, fmt.Sprintf("scan segment %d", i)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		probes = append(probes, readProbe(t, r.agent, sid, i+1, comp))
	}
	return r, sid, probes
}

// parallelPressureRig drives turns that fire several tool calls at once. Each
// assistant message then owns N tool results, forming an atomic group that
// adjustCompressionBoundary must keep together — the shape that makes the
// boundary walk back, and can walk it to zero.
func parallelPressureRig(t *testing.T, name string, turns, fanout, payload int, comp *countingCompressor,
	maxCtx, reserved, offload int) (*rig, string, []probe) {
	t.Helper()

	script := make([]scriptedTurn, 0, turns*2)
	for i := 0; i < turns; i++ {
		calls := make([]types.ToolCall, 0, fanout)
		for j := 0; j < fanout; j++ {
			calls = append(calls, emit(fmt.Sprintf("call-%d-%d", i, j), payload, "string"))
		}
		script = append(script, callTools(calls...), sayText(fmt.Sprintf("batch %d done", i)))
	}

	r := newRig(t, routeOutDir(t, name), script, maxCtx, reserved, offload)
	if comp != nil {
		r.setCompressor(comp)
	}
	sid := "defect-" + name
	ctx := context.Background()

	var probes []probe
	for i := 0; i < turns; i++ {
		if _, err := r.agent.Chat(ctx, sid, fmt.Sprintf("scan batch %d", i)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		probes = append(probes, readProbe(t, r.agent, sid, i+1, comp))
	}
	return r, sid, probes
}

func dumpProbes(t *testing.T, probes []probe) string {
	var b strings.Builder
	for _, p := range probes {
		b.WriteString("\n  " + p.String())
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// D1 — compaction can reclaim nothing, silently.
// ---------------------------------------------------------------------------

// Correct behaviour: while the budget sits above the warning zone and there is
// more than the recency floor in L1, each turn must reclaim something — L1 shrinks,
// or the summary grows. A turn that leaves both unchanged while over budget means
// compaction ran and reclaimed nothing, with no log and no retry.

// ---------------------------------------------------------------------------
// D2 — the emergency trim destroys conversation.
// ---------------------------------------------------------------------------

// Correct behaviour: no single turn may collapse the conversation. Pressure is
// relieved incrementally; a step that drops most of L1 and clears the summary is
// data loss, not pressure handling.

// ---------------------------------------------------------------------------
// D3 — the summary is wiped, not compressed.
// ---------------------------------------------------------------------------

// Correct behaviour: when the summary reaches its cap it must be compressed or
// remain reachable in-session. Writing it to a table the model cannot read and
// setting it to empty loses everything folded so far.

// ---------------------------------------------------------------------------
// D4 — the tool block is counted nowhere.
// ---------------------------------------------------------------------------

// Correct behaviour: the advertised tool schemas are dispatched on every call, so
// their weight belongs in the budget. Loading a skill that registers a tool must
// move the reported usage, because it moves what the provider receives.

// ---------------------------------------------------------------------------
// D5 — the skill body hijacks the last-user pin.
// ---------------------------------------------------------------------------

// Correct behaviour: the message protected from compaction as "the active
// objective" must be the user's own request. After a skill load the newest
// user-role message is the skill body, so the request loses its shield.

// ---------------------------------------------------------------------------
// D6 — restore produces a different session than live.
// ---------------------------------------------------------------------------

// Correct behaviour: a replay of a session reproduces it. Live compacts one batch
// per admission; replay bulk-loads then compacts in a loop — same history, two
// algorithms, so they land in different places.

// ---------------------------------------------------------------------------
// D7 — every reload re-compacts from scratch.
// ---------------------------------------------------------------------------

// Correct behaviour: reload restores the session's working state. It must not
// re-derive it, because re-deriving means re-running the compressor — real
// provider calls — on every single session load.

// ---------------------------------------------------------------------------
// D8 — recalled messages cannot be pruned.
// ---------------------------------------------------------------------------

// Correct behaviour: anything occupying the window is subject to pressure. Content
// recalled into the promoted slot is not, so it can only grow — the model must
// call clear, and nothing makes it.

// ---------------------------------------------------------------------------
// D9 — tool results are never relocated under pressure.
// ---------------------------------------------------------------------------

// Correct behaviour: under pressure, reproducible bulk leaves the context before
// irreplaceable conversation is summarized. Relocation is decided only at arrival,
// on one result's own size, so a session of mid-sized results has nothing
// evictable and compacts conversation instead.
