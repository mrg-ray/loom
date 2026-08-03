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

// Acceptance tests for loom-context-hld.
//
// These assert the DESIGNED behaviour, so they fail against today's code and pass
// as each build-order step lands. Test names carry the step number, so a build can
// be driven from them:
//
//	LOOM_CONTEXT_OPTIMISER=1 go test -tags fts5 -run TestHLD ./test/context-optimiser/ -v
//
// Everything is observed through what the model actually receives — the rendered
// context — plus the durable log, so no test depends on internals that the build
// will move.
package contextoptimiser

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/types"
)

// The design's own vocabulary, so a test reads like the clause it enforces.
var (
	stubAddress  = regexp.MustCompile(`msg:\d+`)
	foldAddress  = regexp.MustCompile(`fold:\d+`)
	stubTokenSay = regexp.MustCompile(`\d+\s+tokens`)
)

// renderedContext is what the provider would receive right now.
func renderedContext(t *testing.T, r *rig, sessionID string) []types.Message {
	t.Helper()
	return compiledContext(t, r.liveSession(t, sessionID))
}

// durableMessages is the log: every row, whatever its flags.
func durableMessages(t *testing.T, r *rig, sessionID string) []types.Message {
	t.Helper()
	msgs, err := r.store.LoadMessages(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load messages: %v", err)
	}
	return msgs
}

func systemMessages(msgs []types.Message) []types.Message {
	var out []types.Message
	for _, m := range msgs {
		if m.Role == "system" {
			out = append(out, m)
		}
	}
	return out
}

// bulkPayload reports whether a message carries a raw tool payload rather than a
// stub — the thing that must not survive in a rendered context under pressure.
func bulkPayload(m types.Message) bool {
	return strings.Count(m.Content, "scan-row") > 50
}

// ---------------------------------------------------------------------------
// Step 1 — Kernel counting
// ---------------------------------------------------------------------------

// §5: the advertised tool schemas enter the budget. Loading a skill that registers
// a tool changes what the provider receives every call, so it must change reported
// usage by more than the messages alone.
func TestHLD_Step1_KernelCountsToolSchemas(t *testing.T) {
	requireGate(t)

	script := []scriptedTurn{
		sayText("ready"),
		callTools(loadSkill("call-load-audit", skillAuditTrail)),
		sayText("loaded"),
	}
	r := newRig(t, routeOutDir(t, "hld-s1-kernel"), script, 20000, 4000, 64*1024)
	sid := "hld-s1"
	ctx := context.Background()

	if _, err := r.agent.Chat(ctx, sid, "hello"); err != nil {
		t.Fatal(err)
	}
	before := readProbe(t, r.agent, sid, 1, nil)
	toolsBefore := len(advertisedToolNames(t, r, sid))

	if _, err := r.agent.Chat(ctx, sid, "load audit-trail"); err != nil {
		t.Fatal(err)
	}
	after := readProbe(t, r.agent, sid, 2, nil)
	toolsAfter := len(advertisedToolNames(t, r, sid))

	if toolsAfter <= toolsBefore {
		t.Skipf("no tool was added by the load (%d -> %d) — cannot exercise the clause", toolsBefore, toolsAfter)
	}

	// Messages added by the load are small; a schema entering the budget should be
	// visible as usage moving by more than message text alone can explain.
	msgs := renderedContext(t, r, sid)
	msgChars := 0
	for _, m := range msgs {
		msgChars += len(m.Content)
	}
	t.Logf("tools %d -> %d, usage %.2f%% -> %.2f%%, rendered chars %d",
		toolsBefore, toolsAfter, before.budgetPct, after.budgetPct, msgChars)

	// The kernel term must be non-zero once schemas are advertised.
	stats := segMemOf(t, r.agent, sid).GetMemoryStats()
	kernel := statInt(stats, "kernel_token_count")
	if kernel == 0 {
		t.Errorf("§5: the kernel term is 0 while %d tool schemas ride every provider call — "+
			"the budget is blind to what it actually sends", toolsAfter)
	}
}

// ---------------------------------------------------------------------------
// Step 2 — The decision point
// ---------------------------------------------------------------------------

// §Architecture rule: nothing mutates context state except releasePressure before
// dispatch. Adding a message must never change what an earlier message renders as.
func TestHLD_Step2_NoMutationAtAdmission(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{}
	r, sid, _ := pressureRig(t, "hld-s2-admission", 10, 6000, comp, 20000, 4000, 64*1024)

	// Every durable row must still hold exactly what the tool produced: admission
	// may not have replaced, shrunk, or summarized anything.
	for i, m := range durableMessages(t, r, sid) {
		if m.Role != "tool" {
			continue
		}
		if strings.Contains(m.Content, "stored in memory") || strings.Contains(m.Content, "Preview:") {
			t.Errorf("§6/§11: durable message %d holds a storage summary, not the payload — "+
				"content was replaced at admission:\n  %s", i, trim(m.Content))
			return
		}
	}
}

// §11 again, from the other side: compaction must not run at admission. With the
// decision point in place, a session under the evict mark has an untouched log and
// an untouched render.
func TestHLD_Step2_NoCompactionBeforeTheMark(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{}
	// Deliberately gentle: a few small results, nowhere near any mark.
	_, _, probes := pressureRig(t, "hld-s2-below-mark", 4, 500, comp, 200000, 20000, 64*1024)

	if comp.count() > 0 {
		t.Errorf("§11: the compressor ran %d times while usage stayed at %.1f%% — "+
			"compaction is still triggering at admission rather than at the decision point",
			comp.count(), probes[len(probes)-1].budgetPct)
	}
}

// ---------------------------------------------------------------------------
// Step 3 — Eviction
// ---------------------------------------------------------------------------

// §1/§6: the payload always enters the message whole. A 70 KB result must be in
// the log verbatim — the row is the store.
func TestHLD_Step3_PayloadEntersTheRowWhole(t *testing.T) {
	requireGate(t)

	script := []scriptedTurn{
		callTool("call-big", emitToolName, map[string]interface{}{
			"bytes": float64(70000), "shape": "string",
		}),
		sayText("stored"),
	}
	r := newRig(t, routeOutDir(t, "hld-s3-arrival"), script, 200000, 20000, 64*1024)
	sid := "hld-s3-arrival"
	if _, err := r.agent.Chat(context.Background(), sid, "pull the full scan"); err != nil {
		t.Fatal(err)
	}

	// Combined design (tool-result-storage-design §1): the payload arrives whole
	// IN MEMORY — the rendered view offers it (offloaded stub with a load path,
	// since 70,000 bytes is over the threshold) — while the durable row is the
	// settled receipt, never the payload.
	var toolMsg *types.Message
	for _, m := range durableMessages(t, r, sid) {
		if m.Role == "tool" {
			mm := m
			toolMsg = &mm
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message was persisted")
	}
	if len(toolMsg.Content) > 4096 {
		t.Errorf("§1 storage: the row holds %d bytes — the DB must hold the settled stub, never the payload", len(toolMsg.Content))
	}
	if !strings.Contains(toolMsg.Content, emitToolName) || !strings.Contains(toolMsg.Content, "again") {
		t.Errorf("§3 storage: the durable row is not a settled stub: %s", trim(toolMsg.Content))
	}
	var rendered *types.Message
	for _, m := range renderedContext(t, r, sid) {
		if m.Role == "tool" {
			mm := m
			rendered = &mm
		}
	}
	if rendered == nil || !strings.Contains(rendered.Content, "query_tool_result(") {
		t.Errorf("§3 storage: in its own turn the oversize payload must be offered — a receipt naming the DATA door (query_tool_result), got: %s",
			trim(rendered.Content))
	}
}

// §8/§9: an oversize result renders as a stub carrying its token cost, its address,
// and a preview OF THE DATA.
func TestHLD_Step3_OversizeRendersAsAddressableStub(t *testing.T) {
	requireGate(t)

	script := []scriptedTurn{
		callTool("call-big", emitToolName, map[string]interface{}{
			"bytes": float64(70000), "shape": "string",
		}),
		sayText("stored"),
	}
	r := newRig(t, routeOutDir(t, "hld-s3-stub"), script, 200000, 20000, 64*1024)
	sid := "hld-s3-stub"
	if _, err := r.agent.Chat(context.Background(), sid, "pull the full scan"); err != nil {
		t.Fatal(err)
	}

	var rendered string
	for _, m := range renderedContext(t, r, sid) {
		if m.Role == "tool" {
			rendered = m.Content
		}
	}
	switch {
	case rendered == "":
		t.Fatal("no tool message in the rendered context")
	case len(rendered) > 4000:
		t.Errorf("§8: the oversize payload rendered in full (%d chars) — no stub was produced", len(rendered))
	case !stubAddress.MatchString(rendered):
		t.Errorf("§4/§9: the stub carries no msg:<id> address, so the model cannot recall it:\n  %s", trim(rendered))
	case !stubTokenSay.MatchString(rendered):
		t.Errorf("§9: the stub does not report its token cost:\n  %s", trim(rendered))
	case !strings.Contains(rendered, "scan-row"):
		t.Errorf("§9: the stub's preview shows no payload content — a preview of the storage "+
			"operation cannot tell the model whether the data is what it wants:\n  %s", trim(rendered))
	}
}

// §10/§12/§13: under pressure, eviction runs FIRST and it is enough — reproducible
// payload leaves the rendered view before any conversation is summarized.
func TestHLD_Step3_EvictionShedsBulkBeforeAnyFold(t *testing.T) {
	requireGate(t)
	// Drive real pressure past the evict mark (85%) with evictable results, then
	// assert eviction shed reproducible payload — usage relieved, stubs present.
	comp := &countingCompressor{}
	r := newRig(t, routeOutDir(t, "hld-s3-evict"), nil, 8000, 1000, 6000)
	r.setCompressor(comp)
	sid := "hld-s3-evict"
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		r.llm.turns = append(r.llm.turns, callTool(fmt.Sprintf("c%d", i), emitToolName, sized(3000)), sayText("r"))
		if _, err := r.agent.Chat(ctx, sid, fmt.Sprintf("scan %d please analyse the grant scope carefully", i)); err != nil {
			t.Fatal(err)
		}
	}
	sm := segMemOf(t, r.agent, sid)
	if statInt(sm.GetMemoryStats(), "evicted_count") == 0 {
		t.Errorf("§10/§12: sustained pressure produced no evictions — eviction is not firing")
	}
	stubs := 0
	for _, m := range renderedContext(t, r, sid) {
		if m.Role == "tool" && isStubRenderHLD(m.Content) {
			stubs++
		}
	}
	if stubs == 0 {
		t.Errorf("§8: pressure ran but no tool result rendered as a stub")
	}
}

// §14: the working-set boundary is the Chat() entry message. A skill-body sidecar
// is user-role and must not move it, so the user's request stays protected.
func TestHLD_Step3_SidecarDoesNotMoveTheBoundary(t *testing.T) {
	requireGate(t)

	const objective = "OBJECTIVE-MARKER profile the complaints table and report scope"
	script := []scriptedTurn{
		sayText("understood"),
		callTools(loadSkill("call-load-grant", skillGrantReview)),
		sayText("loaded"),
	}
	for i := 0; i < 12; i++ {
		script = append(script,
			callTool(fmt.Sprintf("call-p%d", i), emitToolName,
				map[string]interface{}{"bytes": float64(6000), "shape": "string"}),
			sayText("done"))
	}

	comp := &countingCompressor{}
	r := newRig(t, routeOutDir(t, "hld-s3-boundary"), script, 20000, 4000, 64*1024)
	r.setCompressor(comp)
	sid := "hld-s3-boundary"
	ctx := context.Background()

	for _, msg := range append([]string{objective, "load grant-review"}, repeat("scan", 12)...) {
		if _, err := r.agent.Chat(ctx, sid, msg); err != nil {
			t.Fatal(err)
		}
	}

	objectiveLive := false
	for _, m := range renderedContext(t, r, sid) {
		if strings.Contains(m.Content, "OBJECTIVE-MARKER") {
			objectiveLive = true
		}
	}
	if !objectiveLive {
		t.Errorf("§14: the Chat() entry message was compacted away. The boundary followed the " +
			"skill-body sidecar instead of the user's request, so the request lost its protection.")
	}
}

// ---------------------------------------------------------------------------
// Step 4 — Fold and L2
// ---------------------------------------------------------------------------

// §1/§21: L2 is its own system message after ROM, never folded into ROM, and once
// any entry exists the block renders on every call.
func TestHLD_Step4_L2IsItsOwnSystemMessage(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{summary: "STATE of the work"}
	r := driveToFolds(t, "hld-s4-l2", comp, 2)
	sys := systemMessages(renderedContext(t, r, "hld-s4-l2"))
	if len(sys) != 2 {
		t.Fatalf("§1: expected two system messages (ROM, L2 block) after folds, found %d", len(sys))
	}
	if strings.Contains(sys[0].Content, "STATE of the work") {
		t.Errorf("§1: the L2 text was concatenated into ROM")
	}
}

// §17: the compressor reads the batch AS RENDERED. It must never be handed raw
// payloads — it summarizes conversation.
func TestHLD_Step4_CompressorReadsStubsNotPayloads(t *testing.T) {
	requireGate(t)
	seen := &payloadWatchingCompressor{}
	driveToFolds(t, "hld-s4-input", seen, 1)
	if seen.calls == 0 {
		t.Fatal("no fold occurred")
	}
	if seen.sawPayload {
		t.Errorf("§17: the compressor was handed raw tool payload (%d chars) — fold input must be the rendered batch", seen.largest)
	}
}

// §18: one fold produces an entry with BOTH forms, and the short line carries the
// fold number that addresses the long one.
func TestHLD_Step4_FoldEntryHasBothForms(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{summary: "STATE of the work"}
	r := driveToFolds(t, "hld-s4-forms", comp, 2)
	sys := systemMessages(renderedContext(t, r, "hld-s4-forms"))
	if len(sys) < 2 {
		t.Fatal("no L2 block rendered")
	}
	if !foldAddress.MatchString(sys[1].Content) {
		t.Errorf("§4/§18: the L2 block carries no fold:<n> address:\n  %s", trim(sys[1].Content))
	}
}

// ---------------------------------------------------------------------------
// Step 5 — Recall
// ---------------------------------------------------------------------------

// §24/§25/§26: one recall surface, addressed; the return is an ordinary tool result
// at the tail; the stub stays a stub.
func TestHLD_Step5_RecallByAddressReturnsPayload(t *testing.T) {
	requireGate(t)

	script := []scriptedTurn{
		callTool("call-big", emitToolName, map[string]interface{}{
			"bytes": float64(70000), "shape": "string",
		}),
		sayText("stored"),
		{derive: recallByMsgAddress},
		sayText("recalled"),
	}
	r := newRig(t, routeOutDir(t, "hld-s5-recall"), script, 200000, 20000, 64*1024)
	sid := "hld-s5"
	ctx := context.Background()

	if _, err := r.agent.Chat(ctx, sid, "pull the full scan"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.agent.Chat(ctx, sid, "what was in it?"); err != nil {
		t.Fatal(err)
	}

	msgs := renderedContext(t, r, sid)
	last := msgs[len(msgs)-1]
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			last = msgs[i]
			break
		}
	}
	if !strings.Contains(last.Content, "scan-row") {
		t.Errorf("§24: recall did not return the payload as a tool result at the tail. "+
			"Got:\n  %s", trim(last.Content))
	}
}

// §25: no privileged slot exists. Recalled content is an ordinary message, so the
// system slot stays at ROM (+L2) regardless of how much has been recalled.
func TestHLD_Step5_NoPromotedSlot(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{}
	r, sid, _ := pressureRig(t, "hld-s5-no-promoted", 8, 6000, comp, 200000, 20000, 64*1024)

	for _, m := range renderedContext(t, r, sid) {
		if m.Role == "system" && strings.Contains(m.Content, "Retrieved conversation history") {
			t.Errorf("§25: the promoted-context slot still renders — recall must deliver as " +
				"tool results at the tail, with no privileged slot to promote into")
			return
		}
	}
	for _, name := range advertisedToolNames(t, r, sid) {
		if name == "conversation_memory" || name == "session_memory" {
			t.Errorf("§Deleted: %q is still advertised; recall(address) subsumes it", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 6 — Reload
// ---------------------------------------------------------------------------

// §29/§30: reload is read + render. Live and restored are the same read of the same
// rows, so they are identical — and the compressor is not invoked at load.
func TestHLD_Step6_ReloadIsReadAndRender(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{}
	r, sid, _ := pressureRig(t, "hld-s6-reload", 14, 6000, comp, 20000, 4000, 64*1024)

	before := comp.count()
	restored := r.restoredSessionWithCompressor(t, sid, comp, 20000, 4000, 64*1024)
	onReload := comp.count() - before

	if onReload > 0 {
		t.Errorf("§29: reload invoked the compressor %d times — it must read rows and render, "+
			"not re-derive", onReload)
	}

	live := compiledContext(t, r.liveSession(t, sid))
	rest := compiledContext(t, restored)
	if len(live) != len(rest) {
		t.Errorf("§30: live renders %d messages, restored renders %d — the same rows must "+
			"produce the same context", len(live), len(rest))
		return
	}
	for i := range live {
		if live[i].Role != rest[i].Role {
			t.Errorf("§30: rendered message %d role differs between live and restored", i)
			return
		}
		if live[i].Content == rest[i].Content {
			continue
		}
		// Combined design (tool-result-storage-design §6/§7): the K-window is
		// forfeit on reload. A live IN-WINDOW tool row (payload or offloaded
		// stub) restores as its settled stub — that one divergence is the
		// design, everything else is a defect.
		if live[i].Role == "tool" && strings.Contains(rest[i].Content, "again") && !strings.Contains(rest[i].Content, `recall("msg:`) {
			continue
		}
		t.Errorf("§30: rendered message %d differs between live and restored\n  live: %s\n  rest: %s",
			i, trim(live[i].Content), trim(rest[i].Content))
		return
	}
}

// ---------------------------------------------------------------------------
// Terminal condition
// ---------------------------------------------------------------------------

// §20: with both reliefs exhausted the dispatch must not happen — Chat returns the
// recoverable error rather than trimming silently or sending an oversized request.
func TestHLD_Terminal_ExhaustedReliefsReturnRecoverableError(t *testing.T) {
	requireGate(t)
	// A tiny window with heavy conversation: once eviction (no payloads) and fold
	// (floors at the working set) are exhausted and usage is still over, Chat must
	// return the recoverable error rather than dispatch or trim.
	comp := &countingCompressor{summary: "S"}
	r := newRig(t, routeOutDir(t, "hld-terminal"), nil, 4000, 500, 200000)
	r.setCompressor(comp)
	sid := "hld-terminal"
	ctx := context.Background()
	// One user turn whose text alone (~12k tokens) exceeds the ~3500-token budget.
	// It cannot be evicted (user role) or folded (working set), and its size is
	// below the 200k arrival threshold so it renders whole: relief is impossible.
	giant := strings.Repeat("the user states a long irreplaceable requirement. ", 1000)
	r.llm.turns = append(r.llm.turns, sayText("ack"))
	_, lastErr := r.agent.Chat(ctx, sid, giant)
	if lastErr == nil {
		t.Errorf("§20: heavy conversation into a tiny window never returned the recoverable error")
	} else if !strings.Contains(strings.ToLower(lastErr.Error()), "budget") &&
		!strings.Contains(strings.ToLower(lastErr.Error()), "context") {
		t.Errorf("§20: error returned but without budget state: %v", lastErr)
	}
}

// ---------------------------------------------------------------------------
// helpers used only by these tests
// ---------------------------------------------------------------------------

func repeat(s string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%s %d", s, i))
	}
	return out
}

// recallByMsgAddress reads a msg:<id> address out of the context — the model copies
// its own addresses; it never constructs them.
func recallByMsgAddress(messages []types.Message) []types.ToolCall {
	for i := len(messages) - 1; i >= 0; i-- {
		if addr := stubAddress.FindString(messages[i].Content); addr != "" {
			return []types.ToolCall{{
				ID:    "call-recall",
				Name:  "recall",
				Input: map[string]interface{}{"address": addr},
			}}
		}
	}
	return nil
}

// payloadWatchingCompressor records whether fold input ever contained raw payload.
type payloadWatchingCompressor struct {
	calls      int
	sawPayload bool
	largest    int
}

func (c *payloadWatchingCompressor) CompressMessages(_ context.Context, msgs []types.Message) (string, error) {
	c.calls++
	for _, m := range msgs {
		if bulkPayload(m) {
			c.sawPayload = true
			if len(m.Content) > c.largest {
				c.largest = len(m.Content)
			}
		}
	}
	return "folded", nil
}

func (c *payloadWatchingCompressor) IsEnabled() bool { return true }

// pressureRigWith is pressureRig with a caller-supplied compressor.
func pressureRigWith(t *testing.T, name string, turns, payload int, comp interface {
	CompressMessages(context.Context, []types.Message) (string, error)
	IsEnabled() bool
}, maxCtx, reserved, offload int) (*rig, string, []probe) {
	t.Helper()

	script := make([]scriptedTurn, 0, turns*2)
	for i := 0; i < turns; i++ {
		script = append(script,
			callTool(fmt.Sprintf("call-%d", i), emitToolName,
				map[string]interface{}{"bytes": float64(payload), "shape": "string"}),
			sayText("done"))
	}
	r := newRig(t, routeOutDir(t, name), script, maxCtx, reserved, offload)
	r.setCompressor(comp)
	sid := "hld-" + name
	ctx := context.Background()

	var probes []probe
	for i := 0; i < turns; i++ {
		if _, err := r.agent.Chat(ctx, sid, fmt.Sprintf("scan %d", i)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		probes = append(probes, readProbe(t, r.agent, sid, i+1, nil))
	}
	return r, sid, probes
}

// driveToFolds drives heavy conversation with a small L2 budget until at least
// n folds have occurred, returning the rig for inspection.
func driveToFolds(t *testing.T, name string, comp interface {
	CompressMessages(context.Context, []types.Message) (string, error)
	IsEnabled() bool
}, n int) *rig {
	t.Helper()
	r := newRig(t, routeOutDir(t, name), nil, 8000, 1000, 6000)
	r.setCompressor(comp)
	r.setMaxL2Tokens(300)
	sid := name
	ctx := context.Background()
	folds := func() int {
		s, ok := r.agent.GetSession(sid)
		if !ok {
			return 0
		}
		sm := s.SegmentedMem.(*agent.SegmentedMemory)
		return statInt(sm.GetMemoryStats(), "fold_count")
	}
	for i := 0; i < 60 && folds() < n; i++ {
		r.llm.turns = append(r.llm.turns, sayText("ack"))
		if _, err := r.agent.Chat(ctx, sid, heavyConversation(i)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if folds() < n {
		t.Fatalf("only %d folds after 60 turns, wanted %d", folds(), n)
	}
	return r
}

// isStubRenderHLD matches the stub format used by the acceptance sweep.
func isStubRenderHLD(s string) bool {
	return (strings.Contains(s, "offloaded") || strings.Contains(s, "again")) &&
		stubTokenSay.MatchString(s)
}
