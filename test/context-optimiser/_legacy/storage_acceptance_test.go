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

// Acceptance tests for tool-result-storage-design.md — the revision that makes
// tool results ephemeral turn data: DB holds the settled stub from birth, the
// payload lives in memory for K turns (default 1), pressure never writes the
// database, and the query index is per-session :memory: SQLite.
//
// These assert the DESIGNED behaviour, so most fail against today's code and
// turn green as the fix lands (in the other session — this suite is the
// instrument, not the implementation):
//
//	LOOM_CONTEXT_OPTIMISER=1 go test -tags fts5 -run TestStorage ./test/context-optimiser/ -v
//
// Sections cite the design doc (§) at ~/tera-code/designs/tool-result-storage-design.md.
// Everything is observed through exported surfaces only: the rendered context,
// the durable log, the retrieval tools, and a restore twin.
package contextoptimiser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/types"
)

const (
	trsThreshold = 4096  // arrival threshold for these routes
	trsSmall     = 3000  // below the threshold — renders whole in-window
	trsLarge     = 8000  // above the threshold — offloaded stub from birth
	trsBudget    = 60000 // roomy budget: no pressure unless a test wants it
	trsTight     = 12000 // tight budget: pressure reachable in one route
	trsReserved  = 2000
)

var trsTokenFigure = regexp.MustCompile(`~(\d+)\s*tokens`)

// trsBulk reports whether content carries a raw emit payload rather than a stub
// or a preview: a preview line (≤120 chars) holds at most ~13 "scan-row" runs.
func trsBulk(content string) bool { return strings.Count(content, "scan-row") > 50 }

// trsToolRows returns the tool-role messages of a context or log, in order.
func trsToolRows(msgs []types.Message) []types.Message {
	var out []types.Message
	for _, m := range msgs {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

// trsSettled reports whether content is a settled stub per §3: names its size,
// tells the model to run the tool again, offers no load path, carries no bulk.
func trsSettled(content string) bool {
	return strings.Contains(content, "tokens") &&
		strings.Contains(content, "again") &&
		!strings.Contains(content, `recall("msg:`) &&
		!trsBulk(content)
}

// trsStubAddr finds the address a model would copy out of a STUB — never out
// of the ROM, whose documentation carries example addresses like msg:41 that
// resolve to nothing. Non-system rows only, newest match wins.
func trsStubAddr(ms []types.Message) string {
	addr := ""
	for _, m := range ms {
		if m.Role == "system" {
			continue
		}
		if a := stubAddress.FindString(m.Content); a != "" {
			addr = a
		}
	}
	return addr
}

// trsLogRows dumps the rendered tool rows on failure, so a red test shows the
// fix session exactly what the model saw.
func trsLogRows(t *testing.T, rows []types.Message) {
	t.Helper()
	for i, m := range rows {
		t.Logf("tool row %d: %s", i, trim(m.Content))
	}
}

func trsSegMem(t *testing.T, r *rig, sid string) *agent.SegmentedMemory {
	t.Helper()
	s := r.liveSession(t, sid)
	sm, ok := s.SegmentedMem.(*agent.SegmentedMemory)
	if !ok {
		t.Fatalf("session %s carries no *agent.SegmentedMemory", sid)
	}
	return sm
}

// trsChat drives one user turn and fails the test on error.
func trsChat(t *testing.T, r *rig, sid, text string) {
	t.Helper()
	if _, err := r.agent.Chat(context.Background(), sid, text); err != nil {
		t.Fatalf("chat %q: %v", text, err)
	}
}

// ---------------------------------------------------------------------------
// §1/§2 arrival + §9.2 — the DB holds the settled stub from birth, never the
// payload, and pressure never rewrites it.
// ---------------------------------------------------------------------------

// §1 ARRIVAL: for a small and a large result alike, the durable row is the
// settled stub; the payload exists only in memory (whole render for small,
// offloaded stub for large).
func TestStorage_Arrival_DBHoldsSettledStubNeverPayload(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("t-small", trsSmall, "string"), emit("t-large", trsLarge, "string")),
		sayText("both scans noted"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-arrival"
	trsChat(t, r, sid, "run a small and a large scan")

	durable := trsToolRows(durableMessages(t, r, sid))
	if len(durable) != 2 {
		t.Fatalf("expected 2 durable tool rows, got %d", len(durable))
	}
	for i, row := range durable {
		if trsBulk(row.Content) {
			t.Errorf("§1: durable tool row %d holds the payload; the DB must hold the settled stub from birth", i)
		}
		if !strings.Contains(row.Content, emitToolName) || !strings.Contains(row.Content, "tokens") {
			t.Errorf("§3: durable tool row %d is not a settled stub (want tool name + token figure): %q", i, trim(row.Content))
		}
	}

	rendered := trsToolRows(renderedContext(t, r, sid))
	if len(rendered) != 2 {
		t.Fatalf("expected 2 rendered tool rows, got %d", len(rendered))
	}
	if !trsBulk(rendered[0].Content) {
		t.Errorf("§1 IN-WINDOW: the small result must render whole in its own turn")
	}
	if trsBulk(rendered[1].Content) || !strings.Contains(rendered[1].Content, "query_tool_result(") {
		t.Errorf("§3: the large result must render as a receipt advertising the DATA door (query_tool_result), got: %q", trim(rendered[1].Content))
	}
}

// §9.2 WRITTEN ONCE: pressure may reshape the render, never the row. The first
// result's durable bytes are identical before and after a route that crosses
// the marks.
func TestStorage_Arrival_RowNeverRewrittenUnderPressure(t *testing.T) {
	requireGate(t)
	script := []scriptedTurn{callTools(emit("seed", trsSmall, "string")), sayText("seeded")}
	for i := 0; i < 9; i++ {
		script = append(script, callTools(emit(fmt.Sprintf("p%d", i), trsSmall, "string")), sayText(fmt.Sprintf("done-%02d", i)))
	}
	r := newRig(t, t.TempDir(), script, trsTight, trsReserved, trsThreshold)
	sid := "trs-write-once"

	trsChat(t, r, sid, "seed scan")
	before := trsToolRows(durableMessages(t, r, sid))
	if len(before) == 0 {
		t.Fatal("no durable tool row after the seed turn")
	}
	for i := 0; i < 9; i++ {
		trsChat(t, r, sid, fmt.Sprintf("turn-%02d scan", i))
	}
	after := trsToolRows(durableMessages(t, r, sid))
	if after[0].Content != before[0].Content {
		t.Errorf("§9.2: the seed row's durable content changed under pressure —\nbefore: %q\nafter:  %q",
			trim(before[0].Content), trim(after[0].Content))
	}
}

// ---------------------------------------------------------------------------
// §1/§2 the K-window — default K=1: in-window at distance ≤1, settled at 2.
// ---------------------------------------------------------------------------

// §2 K=1: a small result renders whole in its own turn and the next, then
// settles to the stored stub at distance 2.
func TestStorage_Window_SmallSettlesAtDistanceTwo(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("w1", trsSmall, "string")), sayText("captured"),
		sayText("turn-2 filler"),
		sayText("turn-3 filler"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-window-small"

	trsChat(t, r, sid, "turn-1 scan")
	if rows := trsToolRows(renderedContext(t, r, sid)); !trsBulk(rows[0].Content) {
		t.Fatalf("distance 0: the small result must render whole")
	}
	trsChat(t, r, sid, "turn-2 continue")
	if rows := trsToolRows(renderedContext(t, r, sid)); !trsBulk(rows[0].Content) {
		t.Errorf("§2 distance 1: the previous turn's result is the current turn's subject — it must still render whole")
	}
	trsChat(t, r, sid, "turn-3 continue")
	rows := trsToolRows(renderedContext(t, r, sid))
	if !trsSettled(rows[0].Content) {
		t.Errorf("§1 SETTLED at distance 2: want the stored stub (token figure + run-again, no load path, no bulk), got: %q",
			trim(rows[0].Content))
	}
}

// §3/§8: the settled stub's exact anatomy — tool name, a token figure sized to
// the ORIGINAL payload (never re-derived from the stub), reproduce wording, a
// preview head, and no nesting.
func TestStorage_Stub_SettledAnatomyAndTokenFigure(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("s1", trsLarge, "string")), sayText("captured"),
		sayText("turn-2 filler"),
		sayText("turn-3 filler"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-stub-anatomy"
	trsChat(t, r, sid, "turn-1 scan")
	trsChat(t, r, sid, "turn-2 continue")
	trsChat(t, r, sid, "turn-3 continue")

	row := trsToolRows(renderedContext(t, r, sid))[0]
	c := row.Content
	if !strings.Contains(c, emitToolName) {
		t.Errorf("§3: settled stub must name the originating tool: %q", trim(c))
	}
	if !strings.Contains(c, "again") {
		t.Errorf("§3: settled stub's action is reproduce — it must tell the model to run the tool again: %q", trim(c))
	}
	if strings.Contains(c, `recall("msg:`) {
		t.Errorf("§3: a settled stub offers no load path — there is nothing to load: %q", trim(c))
	}
	if !strings.Contains(c, "preview") {
		t.Errorf("§3: settled stub must carry the preview head: %q", trim(c))
	}
	m := trsTokenFigure.FindStringSubmatch(c)
	if m == nil {
		t.Fatalf("§3: settled stub carries no token figure: %q", trim(c))
	}
	if n, _ := strconv.Atoi(m[1]); n < trsLarge/8 {
		t.Errorf("§3 renderer rule: token figure %d is stub-sized — the stored stub was re-derived from itself instead of emitted verbatim", n)
	}
	if strings.Count(c, "result,") > 1 {
		t.Errorf("§3: nested stub — the stored stub must never be input to stub-building: %q", trim(c))
	}
}

// §3: while in-window, a large tabular result's stub advertises BOTH load
// paths — recall pages and the query index.
func TestStorage_Stub_OffloadedTabularAdvertisesQueryPath(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("q1", trsLarge, "table")), sayText("table stored"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-stub-table"
	trsChat(t, r, sid, "pull the table")

	row := trsToolRows(renderedContext(t, r, sid))[0]
	if !strings.Contains(row.Content, "query_tool_result(") || !strings.Contains(row.Content, "sql=") {
		t.Errorf("§3: a tabular receipt must advertise the SQL door with its argument: %q", trim(row.Content))
	}
	if !strings.Contains(row.Content, "SQLite") {
		t.Errorf("§3: a tabular receipt must name the dialect so the model does not learn it by failing: %q", trim(row.Content))
	}
	if strings.Contains(row.Content, `recall("msg:`) {
		t.Errorf("§3: recall is the MESSAGE door — a data receipt must not advertise it: %q", trim(row.Content))
	}
}

// ---------------------------------------------------------------------------
// §4 retrieval — the window is the retrieval boundary.
// ---------------------------------------------------------------------------

// IN-WINDOW DATA (must survive the fix): while the payload is in the window,
// query_tool_result reads it — by line for text — and recall returns the
// MESSAGE (the receipt), never the bytes. Two doors, two answers, one address.
func TestStorage_Retrieval_LargeLoadableAtDistanceOne(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("g1", trsLarge, "string")), sayText("stored"),
		{derive: func(ms []types.Message) []types.ToolCall {
			addr := trsStubAddr(ms)
			if addr == "" {
				return nil
			}
			return []types.ToolCall{
				{ID: "q1", Name: "query_tool_result", Input: map[string]interface{}{
					"reference_id": addr, "offset": float64(0), "limit": float64(5)}},
				{ID: "rc1", Name: "recall", Input: map[string]interface{}{"address": addr}},
			}
		}},
		sayText("read it"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-load-d1"
	trsChat(t, r, sid, "turn-1 scan")
	trsChat(t, r, sid, "turn-2 what was in it?")

	rows := trsToolRows(renderedContext(t, r, sid))
	var dataCame, receiptCame bool
	for _, row := range rows {
		if strings.Contains(row.Content, "lines ") && strings.Contains(row.Content, "scan-row") {
			dataCame = true // query_tool_result paged the text
		}
		if strings.Contains(row.Content, "[msg:") && strings.Contains(row.Content, "stored as msg:") {
			receiptCame = true // recall returned the message as it stands
		}
	}
	if !dataCame {
		trsLogRows(t, rows)
		t.Errorf("query_tool_result must read an in-window payload by line — the window IS the data promise")
	}
	if !receiptCame {
		trsLogRows(t, rows)
		t.Errorf("recall must return the MESSAGE (its receipt), not the payload")
	}
}

// §4 SETTLED MISS: at distance 2 the same recall is an honest miss that names
// the tool to re-run — never the payload.
func TestStorage_Retrieval_RecallMissNamesToolAfterSettle(t *testing.T) {
	requireGate(t)
	var addr string
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("g2", trsLarge, "string")), sayText("stored"),
		{text: "noted", derive: func(ms []types.Message) []types.ToolCall {
			addr = trsStubAddr(ms)
			return nil
		}},
		{derive: func([]types.Message) []types.ToolCall {
			return []types.ToolCall{{ID: "rc2", Name: "recall",
				Input: map[string]interface{}{"address": addr}}}
		}},
		sayText("done"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-miss-d2"
	trsChat(t, r, sid, "turn-1 scan")
	trsChat(t, r, sid, "turn-2 filler")
	trsChat(t, r, sid, "turn-3 bring it back")

	rows := trsToolRows(renderedContext(t, r, sid))
	last := rows[len(rows)-1]
	if trsBulk(last.Content) {
		t.Fatalf("§4: recall returned a settled payload — once out, it stays out")
	}
	if !strings.Contains(last.Content, emitToolName) {
		t.Errorf("§4: the miss must name the tool to re-run (the paired call above holds the recipe): %q", trim(last.Content))
	}
}

// §4/§5: query_tool_result addresses results by MESSAGE id — the id the stub
// shows — and answers SELECTs over an in-window tabular payload.
func TestStorage_Retrieval_QueryByMsgAddress(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("q2", trsLarge, "table")), sayText("table stored"),
		{derive: func(ms []types.Message) []types.ToolCall {
			if addr := trsStubAddr(ms); addr != "" {
				return []types.ToolCall{{ID: "qy1", Name: "query_tool_result",
					Input: map[string]interface{}{
						"reference_id": addr,
						// SUM over ids: 1..125 → 7875, a figure no storage
						// summary or stub can leak — only a real SELECT.
						"sql": "SELECT SUM(id) AS s FROM results",
					}}}
			}
			return nil
		}},
		sayText("counted"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-query-addr"
	trsChat(t, r, sid, "turn-1 pull the table")
	trsChat(t, r, sid, "turn-2 count it")

	rows := trsToolRows(renderedContext(t, r, sid))
	if len(rows) < 2 {
		trsLogRows(t, rows)
		t.Fatalf("§5: no query call was made — the stub carried no msg address to copy")
	}
	last := rows[len(rows)-1]
	if !strings.Contains(last.Content, "7875") { // SUM(1..125)
		t.Errorf("§5: query by msg address must answer over the index (want SUM 7875): %q", trim(last.Content))
	}
}

// The ruled lifetime: queryability ends WITH the window. Past K the sidecar
// row is retired, the turn-scoped index cannot rebuild, and the answer is an
// honest miss telling the model to re-run — never stale data, never a claim
// the system cannot back.
func TestStorage_Retrieval_QueryMissesHonestlyPastWindow(t *testing.T) {
	requireGate(t)
	var addr string
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("q3", trsLarge, "table")), sayText("table stored"),
		{text: "noted", derive: func(ms []types.Message) []types.ToolCall {
			addr = trsStubAddr(ms)
			return nil
		}},
		{derive: func([]types.Message) []types.ToolCall {
			return []types.ToolCall{{ID: "qy2", Name: "query_tool_result",
				Input: map[string]interface{}{
					"reference_id": addr,
					"sql":          "SELECT SUM(id) AS s FROM results",
				}}}
		}},
		sayText("still counted"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-query-outlives"
	trsChat(t, r, sid, "turn-1 pull the table")
	trsChat(t, r, sid, "turn-2 filler")
	trsChat(t, r, sid, "turn-3 count it again")

	// The payload settled at distance 2 (K=1): the sidecar row is retired and
	// the answer must be the honest miss — not data, not a stale copy.
	rows := trsToolRows(renderedContext(t, r, sid))
	if len(rows) < 2 {
		trsLogRows(t, rows)
		t.Fatalf("no query call was made — no address was available to copy")
	}
	last := rows[len(rows)-1]
	if strings.Contains(last.Content, "7875") {
		t.Errorf("past the window the query must MISS — data answered from beyond K means something outlived its ruled lifetime: %q", trim(last.Content))
	}
	if !strings.Contains(last.Content, "re-run") && !strings.Contains(last.Content, "again") {
		t.Errorf("the miss must carry the remedy (re-run): %q", trim(last.Content))
	}
}

// §5.3 AFTER RELOAD: the index is empty and cannot be rebuilt — the answer is
// gone/re-run, never data. (Vacuously green today; guards the contract once
// the index exists.)
func TestStorage_Retrieval_QueryGoneAfterReload(t *testing.T) {
	requireGate(t)
	var addr string
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("q4", trsLarge, "table")), sayText("table stored"),
		{text: "noted", derive: func(ms []types.Message) []types.ToolCall {
			addr = trsStubAddr(ms)
			return nil
		}},
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-query-reload"
	trsChat(t, r, sid, "turn-1 pull the table")
	trsChat(t, r, sid, "turn-2 filler")

	// A twin process: same durable store, fresh memory — the restore path.
	twinLLM := &scriptedLLM{turns: []scriptedTurn{
		{derive: func([]types.Message) []types.ToolCall {
			return []types.ToolCall{{ID: "qy3", Name: "query_tool_result",
				Input: map[string]interface{}{
					"reference_id": addr,
					"sql":          "SELECT COUNT(*) AS c FROM results",
				}}}
		}},
		sayText("asked after reload"),
	}}
	twin, _, twinMem := buildAgentWithMemory(t, twinLLM, r.store, r.skillsDir, r.patternsDir,
		trsBudget, trsReserved, trsThreshold, false)
	if _, err := twin.Chat(context.Background(), sid, "turn-3 count it after restart"); err != nil {
		t.Fatalf("twin chat: %v", err)
	}
	twinSession, ok := twinMem.GetSession(sid)
	if !ok {
		t.Fatal("twin session not found")
	}
	rows := trsToolRows(compiledContext(t, twinSession))
	last := rows[len(rows)-1]
	if strings.Contains(last.Content, "7875") {
		t.Errorf("§5.3: after reload the index must be empty — data answered across a restart means a payload survived somewhere: %q", trim(last.Content))
	}
}

// §4: a recalled range returns conversation whole and tool rows as their stub
// line — history is honest about what it kept.
func TestStorage_Retrieval_RangeReturnsStubLineForToolRows(t *testing.T) {
	requireGate(t)
	var rangeAddr string
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("r1", trsSmall, "string")), sayText("the scan verdict is HEALTHY"),
		sayText("turn-2 filler"),
		sayText("turn-3 filler"),
		{derive: func([]types.Message) []types.ToolCall {
			return []types.ToolCall{{ID: "rr1", Name: "recall",
				Input: map[string]interface{}{"address": rangeAddr}}}
		}},
		sayText("range read"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-range"
	trsChat(t, r, sid, "turn-1 scan")
	trsChat(t, r, sid, "turn-2 continue")
	trsChat(t, r, sid, "turn-3 continue")

	// Build the range from the durable log's ids (the test may look; the model
	// would copy ids out of summaries).
	lo, hi := 1<<62, 0
	for _, m := range durableMessages(t, r, sid) {
		id, err := strconv.Atoi(m.ID)
		if err != nil {
			continue
		}
		if id < lo {
			lo = id
		}
		if id > hi {
			hi = id
		}
	}
	if hi == 0 {
		t.Fatal("durable log has no integer addresses — §9.1 seq ids missing")
	}
	rangeAddr = fmt.Sprintf("msg:%d-%d", lo, hi)
	trsChat(t, r, sid, "turn-4 replay the beginning")

	rows := trsToolRows(renderedContext(t, r, sid))
	last := rows[len(rows)-1]
	if !strings.Contains(last.Content, "HEALTHY") {
		t.Errorf("§4: the range must return conversation whole (assistant verdict missing): %q", trim(last.Content))
	}
	if trsBulk(last.Content) {
		t.Errorf("§4: a tool row inside a range returns its stub line, not its payload")
	}
}

// ---------------------------------------------------------------------------
// §1 PRESSURE — within-turn relief and the near-disappearance of cross-turn
// pressure.
// ---------------------------------------------------------------------------

// §1 PRESSURE: a single long agentic turn (many results inside one user turn)
// survives — current-turn tool results are evictable oldest-first even inside
// the working set; the boundary user message is untouchable.
func TestStorage_Pressure_LongTurnRelievesWithoutTerminal(t *testing.T) {
	requireGate(t)
	script := make([]scriptedTurn, 0, 13)
	for i := 0; i < 12; i++ {
		script = append(script, callTools(emit(fmt.Sprintf("lt%d", i), trsSmall, "string")))
	}
	script = append(script, sayText("long turn complete"))
	r := newRig(t, t.TempDir(), script, trsTight, trsReserved, trsThreshold)
	sid := "trs-long-turn"

	if _, err := r.agent.Chat(context.Background(), sid, "one giant sweep please"); err != nil {
		t.Fatalf("§1 PRESSURE: a long turn must relieve by shedding its own oldest tool results, not error: %v", err)
	}
	rendered := renderedContext(t, r, sid)
	rows := trsToolRows(rendered)
	if len(rows) == 0 {
		t.Fatal("no tool rows rendered")
	}
	if trsBulk(rows[0].Content) {
		t.Errorf("§1 PRESSURE: the oldest current-turn result must have been shed to a stub")
	}
	var boundaryWhole bool
	for _, m := range rendered {
		if m.Role == "user" && strings.Contains(m.Content, "one giant sweep") {
			boundaryWhole = true
		}
	}
	if !boundaryWhole {
		t.Errorf("§1 PRESSURE: conversation in the working set is untouchable — the boundary user message must render whole")
	}
}

// §8/§10: across turns, tool bulk self-cleans at the K-boundary, so the old
// pressure machinery stays quiet — no eviction wording, no folds.
func TestStorage_Pressure_RareAcrossTurns(t *testing.T) {
	requireGate(t)
	script := make([]scriptedTurn, 0, 24)
	for i := 0; i < 12; i++ {
		script = append(script, callTools(emit(fmt.Sprintf("x%d", i), trsSmall, "string")), sayText(fmt.Sprintf("done-%02d", i)))
	}
	r := newRig(t, t.TempDir(), script, trsTight, trsReserved, trsThreshold)
	sid := "trs-rare-pressure"
	for i := 0; i < 12; i++ {
		trsChat(t, r, sid, fmt.Sprintf("turn-%02d scan", i))
	}

	if n := len(trsSegMem(t, r, sid).FoldEntries()); n != 0 {
		t.Errorf("§10: %d folds fired on a route whose bulk self-settles — fold is for conversation, and this conversation is tiny", n)
	}
	for _, m := range renderedContext(t, r, sid) {
		if strings.Contains(m.Content, "evicted from context") {
			t.Errorf("§10: pressure eviction fired across turns — the K-boundary should have settled this row first: %q", trim(m.Content))
		}
	}
}

// §10 CACHE SHAPE: turn over turn, the only region of the render that changes
// is the K-boundary settling — everything ahead of it is byte-stable.
func TestStorage_Cache_PrefixStableExceptKBoundary(t *testing.T) {
	requireGate(t)
	const turns = 10
	script := make([]scriptedTurn, 0, turns*2)
	for i := 0; i < turns; i++ {
		script = append(script, callTools(emit(fmt.Sprintf("c%d", i), trsSmall, "string")), sayText(fmt.Sprintf("done-%02d", i)))
	}
	r := newRig(t, t.TempDir(), script, trsTight, trsReserved, trsThreshold)
	sid := "trs-cache"

	type snap struct {
		keys      []string
		userIndex []int // position of each user message in keys
	}
	snaps := make([]snap, 0, turns)
	for i := 0; i < turns; i++ {
		trsChat(t, r, sid, fmt.Sprintf("turn-%02d scan", i))
		var s snap
		for j, m := range renderedContext(t, r, sid) {
			s.keys = append(s.keys, m.Role+"\x00"+m.Content)
			if m.Role == "user" && strings.Contains(m.Content, "turn-") {
				s.userIndex = append(s.userIndex, j)
			}
		}
		snaps = append(snaps, s)
	}

	for i := 1; i < turns; i++ {
		prev, cur := snaps[i-1], snaps[i]
		diff := 0
		for diff < len(prev.keys) && diff < len(cur.keys) && prev.keys[diff] == cur.keys[diff] {
			diff++
		}
		// Entering turn i (0-based), results born in turn i-2 settle (K=1), so
		// change may start no earlier than turn i-2's user message.
		floorTurn := i - 2
		if floorTurn < 0 {
			continue
		}
		if floorTurn >= len(prev.userIndex) {
			continue
		}
		floor := prev.userIndex[floorTurn]
		if diff < floor {
			t.Errorf("§10 cache: between turn %d and %d the render changed at index %d, before the K-boundary at %d — something rewrote history deeper than the settling region", i-1, i, diff, floor)
		}
	}
}

// ---------------------------------------------------------------------------
// §6 reload and crash.
// ---------------------------------------------------------------------------

// §6: a restored session carries conversation whole and every tool row as its
// settled stub — no payload bytes anywhere in the rebuilt render.
func TestStorage_Reload_SettledStubsNoPayloadBytes(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("rl1", trsSmall, "string")), sayText("the verdict is HEALTHY"),
		callTools(emit("rl2", trsLarge, "string")), sayText("large stored"),
		sayText("turn-3 filler"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-reload"
	trsChat(t, r, sid, "turn-1 scan")
	trsChat(t, r, sid, "turn-2 big scan")
	trsChat(t, r, sid, "turn-3 continue")

	restored := r.restoredSession(t, sid, trsBudget, trsReserved, trsThreshold)
	var sawConversation bool
	for _, m := range compiledContext(t, restored) {
		if trsBulk(m.Content) {
			t.Errorf("§6: restored render carries payload bytes — the DB must not hold any: %q", trim(m.Content))
		}
		if m.Role == "assistant" && strings.Contains(m.Content, "HEALTHY") {
			sawConversation = true
		}
	}
	if !sawConversation {
		t.Errorf("§6: conversation is the durable record — the assistant verdict must survive reload whole")
	}
}

// §1a/§6: a reload mid-window KEEPS the window — the payload sidecar makes it
// durable. The in-window large result restores as its OFFLOADED stub, its load
// path resolves in the fresh process, and once the window passes it settles
// and the sidecar row is gone.
func TestStorage_Reload_MidWindowSurvivesViaSidecar(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("mw1", trsLarge, "string")), sayText("stored"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-midwindow"
	trsChat(t, r, sid, "turn-1 big scan")

	// Fresh process, same store: the window must continue where it was — the
	// DATA door reads the payload out of the sidecar, in a process that never
	// saw it arrive.
	twinLLM := &scriptedLLM{turns: []scriptedTurn{
		{derive: func(ms []types.Message) []types.ToolCall {
			if addr := trsStubAddr(ms); addr != "" {
				return []types.ToolCall{{ID: "mw-q", Name: "query_tool_result",
					Input: map[string]interface{}{
						"reference_id": addr, "offset": float64(0), "limit": float64(5)}}}
			}
			return nil
		}},
		sayText("read after reload"),
	}}
	twin, _, twinMem := buildAgentWithMemory(t, twinLLM, r.store, r.skillsDir, r.patternsDir,
		trsBudget, trsReserved, trsThreshold, false)
	if _, err := twin.Chat(context.Background(), sid, "turn-2 read it"); err != nil {
		t.Fatalf("twin chat: %v", err)
	}
	sess, ok := twinMem.GetSession(sid)
	if !ok {
		t.Fatal("twin session missing")
	}
	rows := trsToolRows(compiledContext(t, sess))
	last := rows[len(rows)-1].Content
	if !strings.Contains(last, "scan-row") {
		t.Errorf("§1a: the durable window must survive a fresh process — the data door read nothing: %s", trim(last))
	}
}

func TestStorage_KMatrix_WindowHoldsAcrossRestartPerK(t *testing.T) {
	requireGate(t)
	for _, k := range []int{0, 1, 2, 3} {
		k := k
		t.Run(fmt.Sprintf("K=%d", k), func(t *testing.T) {
			script := []scriptedTurn{callTools(emit("km", trsSmall, "string")), sayText("captured")}
			for i := 0; i < k+2; i++ {
				script = append(script, sayText(fmt.Sprintf("filler-%d", i)))
			}
			r := newRig(t, t.TempDir(), script, trsBudget, trsReserved, trsThreshold)
			trsSegMemFor := func(mem *agent.Memory, sid string) *agent.SegmentedMemory {
				s, ok := mem.GetSession(sid)
				if !ok {
					t.Fatalf("session %s missing", sid)
				}
				return s.SegmentedMem.(*agent.SegmentedMemory)
			}
			sid := fmt.Sprintf("trs-kmatrix-%d", k)
			trsChat(t, r, sid, "turn-1 scan")
			trsSegMemFor(r.mem, sid).SetRetentionTurns(k)

			// Live: whole through distance K, settled at K+1.
			for d := 1; d <= k+1; d++ {
				trsChat(t, r, sid, fmt.Sprintf("turn-%d continue", d+1))
				rows := trsToolRows(renderedContext(t, r, sid))
				if d <= k {
					if !trsBulk(rows[0].Content) {
						t.Fatalf("K=%d live: distance %d must render whole", k, d)
					}
				} else if !trsSettled(rows[0].Content) {
					t.Fatalf("K=%d live: distance %d must be settled: %s", k, d, trim(rows[0].Content))
				}
			}

			// Restart mid-window (only meaningful when K ≥ 1): a fresh process
			// at distance K must still hold the payload.
			if k == 0 {
				return
			}
			r2 := newRig(t, t.TempDir(), []scriptedTurn{
				callTools(emit("km", trsSmall, "string")), sayText("captured"),
			}, trsBudget, trsReserved, trsThreshold)
			sid2 := fmt.Sprintf("trs-kmatrix-restart-%d", k)
			trsChat(t, r2, sid2, "turn-1 scan")
			trsSegMemFor(r2.mem, sid2).SetRetentionTurns(k)

			twinLLM := &scriptedLLM{turns: []scriptedTurn{sayText("resumed")}}
			twin, _, twinMem := buildAgentWithMemory(t, twinLLM, r2.store, r2.skillsDir, r2.patternsDir,
				trsBudget, trsReserved, trsThreshold, false)
			twinMem.SetRetentionTurns(k)
			if _, err := twin.Chat(context.Background(), sid2, "turn-2 after restart"); err != nil {
				t.Fatalf("twin chat: %v", err)
			}
			twinSession, ok := twinMem.GetSession(sid2)
			if !ok {
				t.Fatal("twin session missing")
			}
			rows := trsToolRows(compiledContext(t, twinSession))
			if !trsBulk(rows[0].Content) {
				t.Fatalf("K=%d: distance 1 across a restart must render whole — the sidecar is the window; got %s",
					k, trim(rows[0].Content))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §5 the index is memory-only; §4 no resurrected surfaces; the K dial.
// ---------------------------------------------------------------------------

// §5: nothing about tool results is written to local disk — the only file the
// route leaves behind is the session store itself.
func TestStorage_Index_NothingOnDisk(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("d1", trsLarge, "table")), sayText("table stored"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-disk"
	trsChat(t, r, sid, "pull the table")

	workDir := filepath.Dir(r.dbPath)
	sessionsBase := filepath.Base(r.dbPath)
	seen := 0
	err := filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		seen++
		base := filepath.Base(path)
		t.Logf("on disk: %s (%d bytes)", path, info.Size())
		if strings.HasPrefix(base, sessionsBase) { // sessions.db + sqlite sidecars
			return nil
		}
		if strings.HasSuffix(base, ".yaml") { // the skill/pattern fixtures
			return nil
		}
		t.Errorf("§5: unexpected file on disk after a tabular route: %s", path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if seen == 0 {
		t.Fatal("walk saw no files at all — the fence is watching the wrong directory")
	}
}

// §4: the retrieval surface is recall + query_tool_result. get_tool_result and
// conversation_memory stay dead.
func TestStorage_Toolset_NoResurrectedSurfaces(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{sayText("hello")}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-toolset"
	trsChat(t, r, sid, "hello")

	names := advertisedToolNames(t, r, sid)
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, dead := range []string{"get_tool_result", "conversation_memory"} {
		if have[dead] {
			t.Errorf("§4: %s must stay unregistered", dead)
		}
	}
	for _, alive := range []string{"recall", "query_tool_result"} {
		if !have[alive] {
			t.Errorf("§4: %s must be advertised — it is a retrieval surface of the design", alive)
		}
	}
}

// The retention dial: storage is identical at every K — only the render moves.
// Feature-detected: skips until the dial exists, then binds it permanently.
func TestStorage_Dial_StorageIdenticalAcrossK(t *testing.T) {
	requireGate(t)
	type retentionDialer interface{ SetRetentionTurns(int) }

	script := func() []scriptedTurn {
		return []scriptedTurn{
			callTools(emit("k1", trsSmall, "string")), sayText("captured"),
			sayText("turn-2 filler"),
		}
	}
	run := func(k int) (dbRows []string, renderedRow string, dialled bool) {
		r := newRig(t, t.TempDir(), script(), trsBudget, trsReserved, trsThreshold)
		sid := fmt.Sprintf("trs-dial-%d", k)
		trsChat(t, r, sid, "turn-1 scan")
		d, ok := any(trsSegMem(t, r, sid)).(retentionDialer)
		if !ok {
			return nil, "", false
		}
		d.SetRetentionTurns(k)
		trsChat(t, r, sid, "turn-2 continue")
		for _, m := range trsToolRows(durableMessages(t, r, sid)) {
			dbRows = append(dbRows, m.Content)
		}
		rows := trsToolRows(renderedContext(t, r, sid))
		return dbRows, rows[0].Content, true
	}

	db0, render0, ok := run(0)
	if !ok {
		t.Skip("retention dial not implemented yet — this test binds it when it lands")
	}
	db2, render2, _ := run(2)
	if strings.Join(db0, "\x00") != strings.Join(db2, "\x00") {
		t.Errorf("K must never touch storage: durable rows differ between K=0 and K=2")
	}
	if trsBulk(render0) {
		t.Errorf("K=0: distance-1 result must be settled in the render")
	}
	if !trsBulk(render2) {
		t.Errorf("K=2: distance-1 result must still render whole")
	}
}

// §1a FAIL AT LOAD: a result over the persist ceiling gets the over-limit
// receipt in the same breath as the fetch — durable row and live render alike
// carry the verdict and remedy, and NO handle exists anywhere.
func TestStorage_OverLimit_FailsAtLoad(t *testing.T) {
	requireGate(t)
	over := int(agent.DefaultMaxToolResultBytes) + (1 << 20)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("huge", over, "string")), sayText("noted the verdict"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-over-limit"
	trsChat(t, r, sid, "pull everything raw")

	for name, rows := range map[string][]types.Message{
		"durable": trsToolRows(durableMessages(t, r, sid)),
		"live":    trsToolRows(renderedContext(t, r, sid)),
	} {
		if len(rows) == 0 {
			t.Fatalf("%s: no tool rows", name)
		}
		c := rows[0].Content
		if !strings.Contains(c, "too large to hold") || !strings.Contains(c, "again") {
			t.Errorf("§1a %s: want the fail-at-load receipt (verdict + remedy), got: %s", name, trim(c))
		}
		if stubAddress.MatchString(c) || strings.Contains(c, "query_tool_result(") {
			t.Errorf("§1a %s: an over-limit receipt must print NO handle: %s", name, trim(c))
		}
	}
}

// §5.3 STATELESS PATH: a fresh process — index nil, not merely empty — must
// re-materialize from the durable sidecar and answer. This is the exact shape
// of every cloud message, where the session is rebuilt per request.
func TestStorage_QueryRematerializesInFreshProcess(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("fp1", trsLarge, "table")), sayText("loaded"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-fresh-process"
	trsChat(t, r, sid, "load the working set")

	addr := ""
	for _, m := range trsToolRows(renderedContext(t, r, sid)) {
		if a := stubAddress.FindString(m.Content); a != "" {
			addr = a
		}
	}
	if addr == "" {
		t.Fatal("no msg address advertised for the tabular result")
	}

	// A twin agent over the same durable store: fresh memory, nil index —
	// exactly what a stateless server hands every message.
	twinLLM := &scriptedLLM{turns: []scriptedTurn{
		{derive: func([]types.Message) []types.ToolCall {
			return []types.ToolCall{{ID: "fq1", Name: "query_tool_result",
				Input: map[string]interface{}{
					"reference_id": addr,
					"sql":          "SELECT SUM(id) AS s FROM results",
				}}}
		}},
		sayText("counted in the fresh process"),
	}}
	twin, _, twinMem := buildAgentWithMemory(t, twinLLM, r.store, r.skillsDir, r.patternsDir,
		trsBudget, trsReserved, trsThreshold, false)
	if _, err := twin.Chat(context.Background(), sid, "count it"); err != nil {
		t.Fatalf("twin chat: %v", err)
	}
	sess, ok := twinMem.GetSession(sid)
	if !ok {
		t.Fatal("twin session missing")
	}
	rows := trsToolRows(compiledContext(t, sess))
	last := rows[len(rows)-1].Content
	if !strings.Contains(last, "7875") { // SUM(1..125)
		t.Errorf("§5.3: a fresh process must re-materialize the index from the sidecar and answer; got: %s", trim(last))
	}
}

// Turn-end cleanup: the on-disk query index leaves no trace after the turn.
func TestStorage_QueryIndexFileGoneAfterTurn(t *testing.T) {
	requireGate(t)
	r := newRig(t, t.TempDir(), []scriptedTurn{
		callTools(emit("cf1", trsLarge, "table")), sayText("loaded"),
		{derive: func(ms []types.Message) []types.ToolCall {
			if addr := trsStubAddr(ms); addr != "" {
				return []types.ToolCall{{ID: "cq1", Name: "query_tool_result",
					Input: map[string]interface{}{"reference_id": addr,
						"sql": "SELECT SUM(id) AS s FROM results"}}}
			}
			return nil
		}},
		sayText("counted"),
	}, trsBudget, trsReserved, trsThreshold)
	sid := "trs-clean-file"
	trsChat(t, r, sid, "turn-1 load")
	trsChat(t, r, sid, "turn-2 count it")

	// The index answered during the turn (proof it existed) …
	rows := trsToolRows(renderedContext(t, r, sid))
	if last := rows[len(rows)-1].Content; !strings.Contains(last, "7875") {
		t.Fatalf("query did not answer, cannot assert cleanup meaningfully: %s", trim(last))
	}
	// … and its file is gone the moment the turn ended.
	pattern := filepath.Join(os.TempDir(), "loom-qindex", sid+".db*")
	if matches, _ := filepath.Glob(pattern); len(matches) != 0 {
		t.Errorf("turn-end cleanup left index files behind: %v", matches)
	}
}
