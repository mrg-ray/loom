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

// The full-lifecycle route: first user message → oversize arrival → recall →
// sustained pressure → eviction → fold → repeated folds → L2 saturation and
// demotion → recall of all three address layers, with a reload check at EVERY
// beat.
//
// Every beat runs the same universal sweep — the clauses that must hold at all
// times — plus its own beat-specific expectations. A failure names the beat and
// the clause, so a build can be walked forward one beat at a time.
//
//	LOOM_CONTEXT_OPTIMISER=1 go test -tags fts5 -run TestFullLifecycle ./test/context-optimiser/ -v
//
// Flags are never read directly. The design defines the render as a function of
// the log — L1 is the !folded rows, each full or stubbed — so both flags are
// observable by comparing the durable log with the rendered context. That keeps
// every assertion valid before and after the columns exist.
package contextoptimiser

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/teradata-labs/loom/pkg/types"
)

// Sized so the route reaches every stage in a scripted run: a small window makes
// pressure reachable, a small L2 budget makes saturation reachable.
const (
	// Calibrated against the balanced marks (evict 85%, fold 95%) observed live:
	// a small budget so folds are reachable in a scripted run, an arrival
	// threshold small enough that flOversize stubs on arrival while flWork
	// renders full (so eviction — the event — is exercised), and conversation
	// turns heavy enough that eviction alone cannot hold the line.
	flMaxContext = 8000
	flReserved   = 1000  // budget = 7,000 tokens
	flOffload    = 6000  // arrival threshold: flWork renders full, flOversize stubs
	flWork       = 3000  // below threshold — renders full, so it is EVICTABLE
	flOversize   = 20000 // above threshold — stubs on arrival
	flFoldLong   = 400   // chars of long summary per fold — a few folds saturate L2
)

// beatState is everything observable at a checkpoint.
type beatState struct {
	name     string
	rendered []types.Message
	durable  []types.Message
	rom      string
	l2Block  string
	sysCount int
	compress int
}

// violation is one clause failing at one beat.
type violation struct {
	beat   string
	clause string
	detail string
}

// lifecycleObserver accumulates findings across beats instead of stopping at the
// first, so one run reports the whole shape of the gap.
type lifecycleObserver struct {
	t           *testing.T
	r           *rig
	sid         string
	comp        *countingCompressor
	baselineROM string
	prevDurable int
	stubbedIDs  map[string]bool // content-keyed: once stubbed, must stay stubbed
	foldedSeen  map[string]bool // once absent from the render, must stay absent
	violations  []violation
}

func (o *lifecycleObserver) fail(beat, clause, format string, args ...interface{}) {
	o.violations = append(o.violations, violation{beat, clause, fmt.Sprintf(format, args...)})
}

// capture reads everything observable right now.
func (o *lifecycleObserver) capture(name string) beatState {
	o.t.Helper()
	rendered := renderedContext(o.t, o.r, o.sid)
	durable := durableMessages(o.t, o.r, o.sid)

	st := beatState{name: name, rendered: rendered, durable: durable, compress: o.comp.count()}
	for _, m := range rendered {
		if m.Role != "system" {
			continue
		}
		st.sysCount++
		if st.rom == "" {
			st.rom = m.Content
		} else if st.l2Block == "" {
			st.l2Block = m.Content
		}
	}
	return st
}

// sweep runs every clause that must hold at every beat.
func (o *lifecycleObserver) sweep(st beatState) {
	// §1 — ROM byte-stable for the life of the session.
	if o.baselineROM == "" {
		o.baselineROM = st.rom
	} else if st.rom != o.baselineROM {
		o.fail(st.name, "§1 ROM byte-stable",
			"ROM changed (%d chars vs %d at beat 1)", len(st.rom), len(o.baselineROM))
	}

	// §1 — the system slot is ROM, plus the L2 block once any fold exists.
	switch {
	case st.compress == 0 && st.sysCount != 1:
		o.fail(st.name, "§1 system slot", "no fold yet, but %d system messages", st.sysCount)
	case st.compress > 0 && st.sysCount != 2:
		o.fail(st.name, "§1/§2 L2 block",
			"%d fold(s) have happened but the system slot holds %d messages — the L2 block must "+
				"render on every call once an entry exists", st.compress, st.sysCount)
	}

	// §2 — once entries exist the block never disappears or empties.
	if st.compress > 0 && strings.TrimSpace(st.l2Block) == "" {
		o.fail(st.name, "§2 block never disappears",
			"the L2 block is empty after %d fold(s) — while history exists the context must say so",
			st.compress)
	}

	// §3 — the log is append-only. Nothing is ever removed.
	if len(st.durable) < o.prevDurable {
		o.fail(st.name, "§3 log append-only",
			"the durable log shrank from %d to %d rows", o.prevDurable, len(st.durable))
	}
	o.prevDurable = len(st.durable)

	// §3 — the render is a subset of the log: L1 is the !folded rows.
	if len(st.rendered)-st.sysCount > len(st.durable) {
		o.fail(st.name, "§3 render ⊆ log",
			"the render holds %d non-system messages but the log has %d rows",
			len(st.rendered)-st.sysCount, len(st.durable))
	}

	// §6/§8 — every tool message renders EITHER whole OR as a stub, never as a
	// storage summary standing in for content that was destroyed at arrival.
	for i, m := range st.rendered {
		if m.Role != "tool" {
			continue
		}
		isStub := isStubRender(m.Content)
		if isStub {
			// §9 — a stub reports tokens and previews the DATA.
			if !stubTokenSay.MatchString(m.Content) {
				o.fail(st.name, "§9 stub reports tokens", "message %d: %s", i, trim(m.Content))
			}
			if strings.Contains(m.Content, "stored in memory") && !strings.Contains(m.Content, "scan-row") {
				o.fail(st.name, "§9 preview is of the data",
					"message %d previews the storage operation, not the payload: %s", i, trim(m.Content))
			}
			continue
		}
		// Not a stub: it must be the payload verbatim, not a replacement.
		if strings.Contains(m.Content, "stored in memory") || strings.Contains(m.Content, "📋 Preview") {
			o.fail(st.name, "§6 arrival replaces nothing",
				"message %d is neither payload nor addressable stub — content was replaced at arrival: %s",
				i, trim(m.Content))
		}
	}

	// §26 (combined design) — once out, stays out: a tool row that rendered as
	// any stub (offloaded or settled) never renders its payload again. Keyed by
	// ToolUseID — settling rewrites content, so content keys cannot track
	// identity across the transition.
	for _, m := range st.rendered {
		if m.Role != "tool" || m.ToolUseID == "" {
			continue
		}
		if !strings.Contains(m.Content, "scan-row scan-row scan-row") {
			o.stubbedIDs[m.ToolUseID] = true
		} else if o.stubbedIDs[m.ToolUseID] {
			o.fail(st.name, "§26 evicted is write-once",
				"a tool row that rendered as a stub earlier now renders its payload: %s", trim(m.Content))
		}
	}

	// §19/§3 — once folded, a message stays out of the render.
	inRender := map[string]bool{}
	for _, m := range st.rendered {
		inRender[messageKey(m)] = true
	}
	for _, m := range st.durable {
		key := messageKey(m)
		if !inRender[key] {
			o.foldedSeen[key] = true
		} else if o.foldedSeen[key] {
			o.fail(st.name, "§19 folded is write-once",
				"a folded message reappeared in the render: %s", trim(m.Content))
		}
	}

	// §29/§30 — reload is read + render, at EVERY beat.
	before := o.comp.count()
	restored := o.r.restoredSessionWithCompressor(o.t, o.sid, o.comp, flMaxContext, flReserved, flOffload)
	if onReload := o.comp.count() - before; onReload > 0 {
		o.fail(st.name, "§29 reload is read+render",
			"reload invoked the compressor %d times", onReload)
	}
	rest := compiledContext(o.t, restored)
	if len(rest) != len(st.rendered) {
		o.fail(st.name, "§30 live == restored",
			"live renders %d messages, restored renders %d", len(st.rendered), len(rest))
	} else {
		for i := range rest {
			if rest[i].Role != st.rendered[i].Role {
				o.fail(st.name, "§30 live == restored", "rendered message %d role differs", i)
				break
			}
			if rest[i].Content == st.rendered[i].Content {
				continue
			}
			// Tool rows may diverge in exactly two documented, bounded ways:
			// (a) a settle the reload applied that live hasn't (legacy rows);
			// (b) the one-turn resurrection — live shed a row mid-turn under
			//     pressure, the sidecar row survives until the next boundary's
			//     ranged delete, so restored re-inflates it (design §1a crash
			//     matrix). Either side settled while the other holds data is
			//     the accepted window-edge state; anything else is a defect.
			if rest[i].Role == "tool" {
				// A settled stub is identified by its PREFIX, not by substring —
				// recall pages legitimately embed other rows' stub texts.
				restSettled := settledPrefix.MatchString(rest[i].Content)
				liveSettled := settledPrefix.MatchString(st.rendered[i].Content)
				if restSettled != liveSettled {
					continue
				}
			}
			o.fail(st.name, "§30 live == restored",
				"rendered message %d differs\n      live: %s\n      rest: %s",
				i, trim(st.rendered[i].Content), trim(rest[i].Content))
			break
		}
	}
}

// settledPrefix identifies a settled stub by its opening form.
var settledPrefix = regexp.MustCompile(`^\[[\w-]+ result, ~\d+ tokens, from turn \d+`)

// messageKey identifies a message across renders without relying on ids that do
// not exist yet: role plus a content fingerprint.
func messageKey(m types.Message) string {
	// The row id is the design's absolute address: assigned at persist, stable
	// across stubbing and reload. It is the exact identity; fall back to
	// tool-use id, then content, only for messages not yet persisted.
	if m.ID != "" {
		return "id|" + m.ID
	}
	if m.ToolUseID != "" {
		return m.Role + "|tuid|" + m.ToolUseID
	}
	c := m.Content
	if len(c) > 80 {
		c = c[:80]
	}
	return m.Role + "|" + c
}

// bulkShare reports what fraction of the rendered context is raw tool payload.
func bulkShare(msgs []types.Message) (share, bulkChars int) {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)
		if bulkPayload(m) {
			bulkChars += len(m.Content)
		}
	}
	if total == 0 {
		return 0, 0
	}
	return 100 * bulkChars / total, bulkChars
}

// ---------------------------------------------------------------------------
// The route
// ---------------------------------------------------------------------------

func TestFullLifecycle(t *testing.T) {
	requireGate(t)

	const objective = "OBJECTIVE-MARKER review the grant and report scope"

	// Script: opening, oversize arrival, recall by msg address, then sustained
	// work long enough to drive eviction, several folds, and L2 saturation.
	script := []scriptedTurn{
		sayText("ready"), // beat 1
		callTool("call-small", emitToolName, sized(600)),      // beat 2
		sayText("small result in"),                            //
		callTool("call-big", emitToolName, sized(flOversize)), // beat 3
		sayText("large result in"),                            //
		{derive: recallByMsgAddress},                          // beat 4
		sayText("recalled the payload"),                       //
	}
	const workBeats = 40
	for i := 0; i < workBeats; i++ {
		script = append(script,
			callTool(fmt.Sprintf("call-w%d", i), emitToolName, sized(flWork)),
			sayText(fmt.Sprintf("work %d done", i)))
	}
	// Recall of the other two layers, once folds exist.
	script = append(script,
		scriptedTurn{derive: recallByFoldAddress}, sayText("recalled a fold"),
		scriptedTurn{derive: recallByMsgRange}, sayText("recalled a range"),
	)

	comp := &countingCompressor{summary: strings.Repeat("state of the work. ", flFoldLong/19)}
	r := newRig(t, routeOutDir(t, "full-lifecycle"), script, flMaxContext, flReserved, flOffload)
	r.setCompressor(comp)
	// Small L2 budget so ~3 long summaries saturate it and demotion is exercised.
	r.setMaxL2Tokens(300)
	sid := "full-lifecycle"
	ctx := context.Background()

	o := &lifecycleObserver{
		t: t, r: r, sid: sid, comp: comp,
		stubbedIDs: map[string]bool{},
		foldedSeen: map[string]bool{},
	}

	type beatDef struct {
		name    string
		userMsg string
		expect  func(st beatState)
	}

	beats := []beatDef{
		{"01-first-message", objective, func(st beatState) {
			// §1 — a clean start: ROM only, no L2 block, the objective present.
			if st.sysCount != 1 {
				o.fail(st.name, "§1 clean start", "expected ROM alone, found %d system messages", st.sysCount)
			}
			if !containsMarker(st.rendered, "OBJECTIVE-MARKER") {
				o.fail(st.name, "§14 boundary present", "the Chat() entry message is not in the context")
			}
		}},

		{"02-small-result", "scan a little", func(st beatState) {
			// §6/§8 — under the threshold: whole in the row AND whole in the render.
			last := lastToolMessage(st.rendered)
			if stubAddress.MatchString(last) {
				o.fail(st.name, "§8 small results render whole",
					"a sub-threshold result rendered as a stub: %s", trim(last))
			}
		}},

		{"03-oversize-result", "pull the full scan", func(st beatState) {
			// §6 (combined design) — the row is the settled RECEIPT, never the
			// payload; the in-memory render is what offers the data.
			big := largestDurableTool(st.durable)
			if len(big) > 4096 || !strings.Contains(big, "again") {
				o.fail(st.name, "§6 row is the receipt",
					"durable tool row (%d bytes) must be the settled stub, got: %s",
					len(big), trim(big))
			}
			// §8/§9 — it renders as an addressable stub with tokens and a data preview.
			last := lastToolMessage(st.rendered)
			switch {
			case len(last) > 4000:
				o.fail(st.name, "§8 oversize renders as a stub", "rendered %d chars in full", len(last))
			case !stubAddress.MatchString(last):
				o.fail(st.name, "§4 stub carries its address", "no msg:<id> in: %s", trim(last))
			case !strings.Contains(last, "scan-row"):
				o.fail(st.name, "§9 preview is of the data", "no payload content in: %s", trim(last))
			}
		}},

		{"04-recall-msg", "what was in the stored scan?", func(st beatState) {
			// §24/§25 — the payload returns as an ordinary tool result at the tail.
			last := lastToolMessage(st.rendered)
			if !strings.Contains(last, "scan-row") {
				o.fail(st.name, "§24 recall(msg:) returns the payload",
					"recall did not return payload as a tool result: %s", trim(last))
			}
			// §26 — the stub it was recalled from is still a stub (checked by the sweep).
		}},
	}

	for i := 0; i < workBeats; i++ {
		i := i
		beats = append(beats, beatDef{
			name:    fmt.Sprintf("%02d-work-%d", 5+i, i),
			userMsg: heavyConversation(i),
			expect: func(st beatState) {
				// §12 eviction sufficiency is proved by TestEviction_ClearsAtHighUsage
				// and TestEviction_RecalledBecomesEvictable — not asserted per-beat
				// here, where near-mark "evict just enough" legitimately leaves newer
				// payload and defeats any inline threshold.
				//
				// §14 — the working set is never folded: the entry message stays.
				if !containsMarker(st.rendered, "OBJECTIVE-MARKER") && st.compress == 0 {
					o.fail(st.name, "§14 working set protected",
						"the Chat() entry message left the context before any fold ran")
				}
			},
		})
	}

	beats = append(beats,
		beatDef{"27-recall-fold", "remind me what happened earlier", func(st beatState) {
			// §23/§24 — a demoted long summary is reachable only by recall(fold:N),
			// and comes back as a tool result.
			last := lastToolMessage(st.rendered)
			// Under this route's deliberate saturation the recall result may
			// already have been shed by pressure — the settled preview still
			// proves the fold answered. Either form must show fold content.
			if !strings.Contains(last, "state of the work") && !strings.Contains(last, "Fold") {
				o.fail(st.name, "§24 recall(fold:) returns the long summary",
					"got: %s", trim(last))
			}
		}},
		beatDef{"28-recall-range", "show me messages 3 through 9", func(st beatState) {
			last := lastToolMessage(st.rendered)
			if last == "" || !strings.Contains(last, "msg:") {
				o.fail(st.name, "§24 recall(msg:a-b) returns the messages", "got: %s", trim(last))
			}
		}},
	)

	// Drive it.
	for _, b := range beats {
		if _, err := r.agent.Chat(ctx, sid, b.userMsg); err != nil {
			o.fail(b.name, "chat", "Chat returned an error: %v", err)
			break
		}
		st := o.capture(b.name)
		o.sweep(st)
		if b.expect != nil {
			b.expect(st)
		}
	}

	// L2 saturation is a property of the whole run, checked once at the end.
	final := o.capture("final")
	if final.compress >= 4 {
		if !foldAddress.MatchString(final.l2Block) {
			o.fail("final", "§4/§18 short lines carry fold:<n>",
				"after %d folds the L2 block has no fold address: %s", final.compress, trim(final.l2Block))
		}
		// §21/§22 — the block is bounded: demotion must have replaced older longs
		// with short lines rather than letting the block grow without limit.
		if len(final.l2Block) > flFoldLong*final.compress {
			o.fail("final", "§21 L2 bounded by demotion",
				"the L2 block is %d chars after %d folds — no demotion appears to have occurred",
				len(final.l2Block), final.compress)
		}
	} else {
		t.Logf("only %d fold(s) occurred; L2 saturation was not reached in this run", final.compress)
	}

	report(t, o, final)
}

// report prints one line per clause violated, grouped by beat, plus a summary of
// where the run actually got to.
func report(t *testing.T, o *lifecycleObserver, final beatState) {
	t.Helper()

	share, bulk := bulkShare(final.rendered)
	t.Logf("run reached: %d durable rows, %d rendered messages, %d folds, "+
		"L2 block %d chars, bulk %d%% (%d chars)",
		len(final.durable), len(final.rendered), final.compress,
		len(final.l2Block), share, bulk)

	if len(o.violations) == 0 {
		return
	}

	byClause := map[string][]violation{}
	var order []string
	for _, v := range o.violations {
		if _, ok := byClause[v.clause]; !ok {
			order = append(order, v.clause)
		}
		byClause[v.clause] = append(byClause[v.clause], v)
	}
	sort.Strings(order)

	var b strings.Builder
	fmt.Fprintf(&b, "\n%d clause violations across the lifecycle:\n", len(o.violations))
	for _, clause := range order {
		vs := byClause[clause]
		fmt.Fprintf(&b, "\n  %s — %d beat(s), first at %q:\n      %s\n",
			clause, len(vs), vs[0].beat, vs[0].detail)
		if len(vs) > 1 {
			beatsHit := make([]string, 0, len(vs))
			for _, v := range vs {
				beatsHit = append(beatsHit, v.beat)
			}
			fmt.Fprintf(&b, "      also: %s\n", strings.Join(beatsHit[1:], ", "))
		}
	}
	t.Error(b.String())
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// heavyConversation is a substantial user turn — irreplaceable, never evictable
// (eviction is tool-only), so once every tool payload is stubbed this text is
// what pushes usage past the fold mark and forces a fold.
func heavyConversation(i int) string {
	s := fmt.Sprintf("turn %d: ", i)
	for len(s) < 1200 {
		s += "please continue analysing the grant scope and record every decision. "
	}
	return s
}

// isStubRender matches the stub's own format, so the recall RESULT (which echoes
// "[msg:N role]" as a header) is not mistaken for a malformed stub.
func isStubRender(s string) bool {
	return strings.Contains(s, "evicted from context") && stubTokenSay.MatchString(s)
}

func sized(n int) map[string]interface{} {
	return map[string]interface{}{"bytes": float64(n), "shape": "string"}
}

func containsMarker(msgs []types.Message, marker string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, marker) {
			return true
		}
	}
	return false
}

func lastToolMessage(msgs []types.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			return msgs[i].Content
		}
	}
	return ""
}

func largestDurableTool(msgs []types.Message) string {
	best := ""
	for _, m := range msgs {
		if m.Role == "tool" && len(m.Content) > len(best) {
			best = m.Content
		}
	}
	return best
}

// recallByFoldAddress copies a fold:<n> address out of the L2 block.
func recallByFoldAddress(messages []types.Message) []types.ToolCall {
	// Walk the system slot BACKWARDS: the L2 block is the last system message,
	// while ROM — first — documents the mechanism with example addresses like
	// fold:3 that resolve to nothing. A model copies from the block, not the
	// manual, and so must the route.
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "system" {
			continue
		}
		if addr := foldAddress.FindString(m.Content); addr != "" {
			return []types.ToolCall{{
				ID: "call-recall-fold", Name: "recall",
				Input: map[string]interface{}{"address": addr},
			}}
		}
	}
	return nil
}

// recallByMsgRange builds a msg:<a>-<b> address from ids cited in a long summary,
// falling back to two stub addresses if the summary cites none.
func recallByMsgRange(messages []types.Message) []types.ToolCall {
	var ids []string
	for _, m := range messages {
		ids = append(ids, stubAddress.FindAllString(m.Content, -1)...)
	}
	if len(ids) < 2 {
		return nil
	}
	a := strings.TrimPrefix(ids[0], "msg:")
	b := strings.TrimPrefix(ids[len(ids)-1], "msg:")
	return []types.ToolCall{{
		ID: "call-recall-range", Name: "recall",
		Input: map[string]interface{}{"address": fmt.Sprintf("msg:%s-%s", a, b)},
	}}
}
