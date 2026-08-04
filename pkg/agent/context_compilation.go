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
package agent

// ContextCompilation (HLD §5): data structures hold no logic — ALL logic lives
// in the one algorithm here, run at every LLM call. Compile renders the context
// (offload is a pure render condition); releasePressure is the only mechanism
// that mutates context state (write-once flags + summary versions), entered
// only from the provider's context-too-long refusal.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.uber.org/zap"
)

// summaryState is L2 — the session summary: ONE cumulative text, persisted as
// version rows; the newest version is the summary (HLD §2, §5.4).
type summaryState struct {
	n    int
	text string
}

// syntheticFailedResult is the normative text of HLD §5.2 step 7: a call with
// no matching result row gets a synthetic failed result emitted in its place —
// synthetic, never strip, because the signature is the re-run door.
const syntheticFailedResult = "[no result recorded — the call did not complete. Re-run it if needed.]"

// offloadStubFormat is the normative offload stub of HLD §5.5 — a turn=T result
// strictly over the threshold; the payload is in memory and queryable.
const offloadStubFormat = "[%s result, ~%d tokens, held in memory this turn — query_tool_result(message_id=%d, offset=0, limit=100) to read it; sql=\"SELECT ... FROM results\" for tabular data.%s\n preview: %s]"

// evictedStubFormat is the normative evicted stub of HLD §5.5 — evicted=true or
// legacy oversize; the only door is re-run.
const evictedStubFormat = "[%s result, ~%d tokens, evicted from context — re-run the call above if this data is needed again.%s\n preview: %s]"

// compileLocked is ContextCompilation §5.2 steps 1–7: KERNEL rides the provider
// tools parameter (its bytes are counted via kernelBytes); ROM; the summary's
// newest version as one system message; then L1 in seq order under the five
// render cases, with pair atomicity restored by synthetic failed results.
// Must hold lock.
func (sm *SegmentedMemory) compileLocked() []Message {
	out := make([]Message, 0, len(sm.contextMessages)+2)

	// Step 2: ROM.
	if sm.romContent != "" {
		out = append(out, Message{Role: "system", Content: sm.romContent})
	}

	// Step 3: the summary's newest version, one system message.
	if sm.summary.text != "" {
		out = append(out, Message{Role: "system", Content: sm.summary.text})
	}

	// Step 5: T — the session's current turn number.
	t := sm.currentTurnLocked()

	// Tool names by call id, for the stub headers.
	callName := make(map[string]string)
	for i := range sm.contextMessages {
		for _, c := range sm.contextMessages[i].ToolCalls {
			if c.ID != "" {
				callName[c.ID] = c.Name
			}
		}
	}

	// Step 6 render cases + step 7 pair atomicity.
	msgs := sm.contextMessages
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		out = append(out, sm.renderLocked(&m, t, callName))

		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			// Consume this call batch's contiguous tool results, then emit a
			// synthetic failed result for every call left without one — never
			// strip the call: the signature is the re-run door (§5.2 step 7).
			missing := make(map[string]bool, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				if c.ID != "" {
					missing[c.ID] = true
				}
			}
			j := i + 1
			for j < len(msgs) && msgs[j].Role == "tool" {
				r := msgs[j]
				delete(missing, r.ToolUseID)
				out = append(out, sm.renderLocked(&r, t, callName))
				j++
			}
			for _, c := range m.ToolCalls {
				if c.ID != "" && missing[c.ID] {
					out = append(out, Message{
						Role:      "tool",
						ToolUseID: c.ID,
						Content:   syntheticFailedResult,
					})
				}
			}
			i = j - 1
		}
	}

	return out
}

// renderLocked applies §5.2 step 6's five render cases — first matching case
// wins. Conversation is never stubbed; offload is a pure render condition of
// the current turn; evicted and legacy-oversize rows render the evicted stub.
func (sm *SegmentedMemory) renderLocked(m *Message, t int64, callName map[string]string) Message {
	if m.Role != "tool" {
		return *m
	}
	switch {
	case m.Evicted:
		r := *m
		r.Content = sm.evictedStub(m, callName)
		return r
	case len(m.Content) > sm.threshold && m.Turn == t:
		r := *m
		r.Content = sm.offloadStub(m, callName)
		return r
	case len(m.Content) > sm.threshold && m.Turn < t:
		r := *m
		r.Content = sm.evictedStub(m, callName)
		return r
	default:
		return *m
	}
}

// offloadStub renders the §5.5 offload stub for a current-turn result strictly
// over the threshold.
func (sm *SegmentedMemory) offloadStub(m *Message, callName map[string]string) string {
	id, _ := strconv.ParseInt(m.ID, 10, 64)
	return fmt.Sprintf(offloadStubFormat,
		stubToolName(m, callName),
		tokenFigure(len(m.Content)),
		id,
		harvestTails(m.Content),
		previewOf(m.Content))
}

// evictedStub renders the §5.5 evicted stub.
func (sm *SegmentedMemory) evictedStub(m *Message, callName map[string]string) string {
	return fmt.Sprintf(evictedStubFormat,
		stubToolName(m, callName),
		tokenFigure(len(m.Content)),
		harvestTails(m.Content),
		previewOf(m.Content))
}

// stubToolName resolves the producing call's tool name via the paired
// assistant row's signature.
func stubToolName(m *Message, callName map[string]string) string {
	if name := callName[m.ToolUseID]; name != "" {
		return name
	}
	return "tool"
}

// harvestTails extracts the row's trailing [harvested: …] / [harvest failed …]
// line(s), prefixed by one space — the tails sit at the end of stored content,
// outside the preview, so the stub must carry them itself (HLD §5.5, §4.4).
func harvestTails(content string) string {
	var tails []string
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "[harvested: ") || strings.HasPrefix(line, "[harvest failed for ") {
			tails = append([]string{line}, tails...)
			continue
		}
		break
	}
	if len(tails) == 0 {
		return ""
	}
	return " " + strings.Join(tails, " ")
}

// previewOf renders the §5.5 preview: the first 160 bytes of the content with
// every whitespace run collapsed to a single space, cut backward to a rune
// boundary (whole runes are appended, so no rune is ever split).
func previewOf(content string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range content {
		var s string
		if unicode.IsSpace(r) {
			if lastSpace {
				continue
			}
			lastSpace = true
			s = " "
		} else {
			lastSpace = false
			s = string(r)
		}
		if b.Len()+len(s) > 160 {
			break
		}
		b.WriteString(s)
	}
	return b.String()
}

// --- estimate & target (HLD §5.1) -------------------------------------------

// estimateLocked returns UTF-8 bytes of the compiled context, exactly as it
// would be sent (serialized KERNEL + ROM + summary + L1 view), ÷ 3. The divisor
// is 3, not the folk value 4: JSON and tabular data run ≈3 bytes per token.
// Computed only inside releasePressure — never on the normal path, nothing
// cached. Must hold lock.
func (sm *SegmentedMemory) estimateLocked() int {
	bytes := sm.kernelBytes
	for _, m := range sm.compileLocked() {
		bytes += len(m.Content)
		if len(m.ToolCalls) > 0 {
			if b, err := json.Marshal(m.ToolCalls); err == nil {
				bytes += len(b)
			}
		}
	}
	return tokenFigure(bytes)
}

// targetLocked returns WarningThresholdPercent% of (window − reserved output),
// both bound via SetContextLimits (HLD §5.1). Must hold lock.
func (sm *SegmentedMemory) targetLocked() int {
	pct := sm.compressionProfile.WarningThresholdPercent
	if pct <= 0 {
		pct = 75
	}
	return pct * (sm.tokenBudget.MaxTokens - sm.tokenBudget.ReservedTokens) / 100
}

// proactiveReliefPercent is the self-imposed relief limit as a percentage of the
// model window: loom sheds when its own estimate reaches this, so the context
// stays off the provider's hard window and relief never depends on catching a
// provider refusal. The 10% headroom absorbs the output reservation and the
// estimate's error; the provider-refusal path stays only as a backstop.
const proactiveReliefPercent = 90

// proactiveLimitLocked returns proactiveReliefPercent% of the window. Must hold
// lock. Sits above targetLocked (75% of usable) so a shed does not immediately
// re-trigger.
func (sm *SegmentedMemory) proactiveLimitLocked() int {
	return proactiveReliefPercent * sm.tokenBudget.MaxTokens / 100
}

// MaybeReleaseProactively runs one relief pass IFF the compiled-context estimate
// is at or above the proactive limit (§5.1). This is the primary relief trigger:
// loom's own accounting, evaluated at compile before every send — not the
// provider's refusal. Safe to call every provider call; it is a cheap estimate
// unless the limit is crossed.
//
// Returns:
//   - shed:     a relief pass ran.
//   - terminal: after shedding everything sheddable the estimate still exceeds
//     the window — the current turn alone overflows loom's window, so the turn
//     cannot proceed (§5.2). loom enforces its own window here rather than
//     waiting for the provider, since a deployment's window may sit below the
//     provider's real ceiling.
//   - estimate,target: post-pass figures for logging / the terminal error.
func (sm *SegmentedMemory) MaybeReleaseProactively(ctx context.Context) (shed, terminal bool, estimate, target int) {
	sm.mu.Lock()
	over := sm.estimateLocked() >= sm.proactiveLimitLocked()
	window := sm.tokenBudget.MaxTokens
	sm.mu.Unlock()
	if !over {
		return false, false, 0, 0
	}
	// ReleasePressure takes the lock itself; its returned estimate is post-pass.
	estimate, target = sm.ReleasePressure(ctx)
	return true, estimate >= window, estimate, target
}

// --- releasePressure (HLD §5.2) ----------------------------------------------

// ReleasePressure is one complete relief pass, no sends inside it, entered only
// from the provider's context-too-long refusal (§5.2 step 12): four operations
// — evict(T−K) · evict(T−1) · fold(T−K) · fold(T−1) — all eviction before any
// fold. After each operation that changed anything the recompiled context is
// estimated; at ≤ target the pass returns — enough is shed. After the fourth
// operation it returns regardless. Returns the final {estimate, target} for the
// caller's single resend and terminal error.
func (sm *SegmentedMemory) ReleasePressure(ctx context.Context) (estimate, target int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	t := sm.currentTurnLocked()
	k := int64(sm.protectedRecentTurns)
	if k <= 0 {
		k = defaultProtectedRecentTurns
	}
	target = sm.targetLocked()
	estimate = -1

	ops := []struct {
		name     string
		boundary int64
		run      func(context.Context, int64) bool
	}{
		{"evict", t - k, sm.evictLocked},
		{"evict", t - 1, sm.evictLocked},
		{"fold", t - k, sm.foldLocked},
		{"fold", t - 1, sm.foldLocked},
	}
	for _, op := range ops {
		if !op.run(ctx, op.boundary) {
			// An operation that changed nothing (its rows were already flagged
			// by an earlier pressure event) passes to the next.
			continue
		}
		estimate = sm.estimateLocked()
		zap.L().Info("releasePressure: operation complete",
			zap.String("session_id", sm.sessionID),
			zap.String("operation", op.name),
			zap.Int64("boundary_turn", op.boundary),
			zap.Int("estimate_tokens", estimate),
			zap.Int("target_tokens", target))
		if estimate <= target {
			return estimate, target
		}
	}

	if estimate < 0 {
		// The pass computed no estimate (nothing changed) — compute once for
		// the terminal error (§5.1).
		estimate = sm.estimateLocked()
	}
	return estimate, target
}

// evictLocked marks evicted=true on every tool row with turn ≤ b whose stored
// content ≥ 2× its stub (the eviction floor, §5.1) — one transaction, in-memory
// copies updated in the same step. Returns whether anything changed. Must hold
// lock.
func (sm *SegmentedMemory) evictLocked(ctx context.Context, b int64) bool {
	callName := make(map[string]string)
	for i := range sm.contextMessages {
		for _, c := range sm.contextMessages[i].ToolCalls {
			if c.ID != "" {
				callName[c.ID] = c.Name
			}
		}
	}

	var marked []int
	var seqs []int64
	for i := range sm.contextMessages {
		m := &sm.contextMessages[i]
		if m.Role != "tool" || m.Evicted || m.Turn > b {
			continue
		}
		stub := sm.evictedStub(m, callName)
		if len(m.Content) < 2*len(stub) {
			// Below the floor, stubbing saves nothing or goes negative.
			continue
		}
		marked = append(marked, i)
		if seq, err := strconv.ParseInt(m.ID, 10, 64); err == nil {
			seqs = append(seqs, seq)
		}
	}
	if len(marked) == 0 {
		return false
	}

	if sm.sessionStore != nil && sm.sessionID != "" && len(seqs) > 0 {
		if err := sm.sessionStore.MarkEvicted(ctx, sm.sessionID, seqs); err != nil {
			zap.L().Error("releasePressure: evict flag persist failed",
				zap.String("session_id", sm.sessionID),
				zap.Int64("boundary_turn", b),
				zap.Error(err))
			return false
		}
	}

	// Flag mirror: update the in-memory L1 copies in the same step (§5.2).
	for _, i := range marked {
		sm.contextMessages[i].Evicted = true
	}
	sm.l1Dirty = true
	sm.updateTokenCount()
	sm.tokenCountDirty = false

	zap.L().Info("releasePressure: evict",
		zap.String("session_id", sm.sessionID),
		zap.Int64("boundary_turn", b),
		zap.Int("rows_marked", len(marked)),
		zap.Int64s("seqs", seqs))
	return true
}

// foldLocked folds every folded=false row with turn ≤ b into summary version
// n+1 (HLD §5.4): pairs fold whole; compressor input = current summary + the
// region AS RENDERED (stubs as stubs); the version insert and the region's
// folded flags commit in one transaction; the new version and flags are
// mirrored into memory in the same step; skills whose manage_skills load pair
// lies inside the region are deactivated. Returns whether anything changed.
// Must hold lock.
func (sm *SegmentedMemory) foldLocked(ctx context.Context, b int64) bool {
	// Region: the seq-ordered prefix with turn ≤ b, pair-adjusted so a call
	// and its result are never split across the boundary.
	count := 0
	for count < len(sm.contextMessages) && sm.contextMessages[count].Turn <= b {
		count++
	}
	count = sm.adjustCompressionBoundary(count)
	if count <= 0 {
		return false
	}
	region := sm.contextMessages[:count]

	// The covered span, from the region's persisted seqs.
	var loSeq, hiSeq int64
	var seqs []int64
	for i := range region {
		if seq, err := strconv.ParseInt(region[i].ID, 10, 64); err == nil {
			if loSeq == 0 || seq < loSeq {
				loSeq = seq
			}
			if seq > hiSeq {
				hiSeq = seq
			}
			seqs = append(seqs, seq)
		}
	}

	// Compressor input: the current summary text + the region as rendered —
	// stubs as stubs, so it summarizes conversation, never payloads — with
	// msg: addresses visible for citation (§5.4.2/4).
	t := sm.currentTurnLocked()
	callName := make(map[string]string)
	for i := range sm.contextMessages {
		for _, c := range sm.contextMessages[i].ToolCalls {
			if c.ID != "" {
				callName[c.ID] = c.Name
			}
		}
	}
	input := make([]Message, 0, len(region)+1)
	if sm.summary.text != "" {
		input = append(input, Message{Role: "system", Content: "Current summary:\n" + sm.summary.text})
	}
	for i := range region {
		r := sm.renderLocked(&region[i], t, callName)
		if r.ID != "" {
			r.Content = "msg:" + r.ID + " " + r.Content
		}
		input = append(input, r)
	}

	// Compressor = the same model as the agent, or better; its output is
	// version n+1 whole (superseded, not merged). Fold is rare and its output
	// is the session's whole memory, so the call gets 120s, not the old 5s.
	newText := ""
	fallback := false
	if sm.compressor != nil && sm.compressor.IsEnabled() {
		compressCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		compressed, err := sm.compressor.CompressMessages(compressCtx, input)
		cancel()
		if err == nil && compressed != "" {
			newText = strings.TrimSpace(compressed)
			// The first line states the covered span (§5.4.4) — enforced here
			// when the compressor omitted it.
			if !strings.HasPrefix(newText, "covers msg:") {
				newText = fmt.Sprintf("covers msg:%d-%d\n", loSeq, hiSeq) + newText
			}
		}
	}
	if newText == "" {
		// Compressor failure (§5.4.5): version n+1 = the previous text
		// unchanged plus one line — coverage is never silently claimed and
		// never lost; the next fold's compressor pass absorbs it.
		fallback = true
		line := fmt.Sprintf("also covers msg:%d-%d (unsummarized): %s", loSeq, hiSeq, firstUserLine(region))
		if sm.summary.text == "" {
			newText = line
		} else {
			newText = sm.summary.text + "\n" + line
		}
	}

	n1 := sm.summary.n + 1

	// One transaction: the version insert and the region's folded flags
	// (§5.4.6). A failed insert fails the step — never a fold that evaporates
	// on reload.
	if sm.sessionStore != nil && sm.sessionID != "" {
		if err := sm.sessionStore.FoldMessages(ctx, sm.sessionID, seqs, n1, newText); err != nil {
			zap.L().Error("releasePressure: fold persist failed",
				zap.String("session_id", sm.sessionID),
				zap.Int64("boundary_turn", b),
				zap.Int("version", n1),
				zap.Error(err))
			return false
		}
	}

	// Deactivate every skill whose manage_skills load pair lies inside the
	// region (HLD §4.5) — its tools leave KERNEL at the next compile;
	// re-loading the skill is the door back.
	if sm.skillDeactivation != nil {
		for _, name := range foldedSkillLoads(region) {
			sm.skillDeactivation(sm.sessionID, name)
		}
	}

	// Mirror into memory in the same step: install the new version and drop
	// the folded region from L1 — the recompile that follows must see
	// post-stage state without a re-read (folded rows are filtered at the
	// database read on reload).
	sm.summary = summaryState{n: n1, text: newText}
	remaining := make([]Message, len(sm.contextMessages)-count)
	copy(remaining, sm.contextMessages[count:])
	sm.contextMessages = remaining
	sm.l1Dirty = true
	sm.updateTokenCount()
	sm.tokenCountDirty = false

	zap.L().Info("releasePressure: fold",
		zap.String("session_id", sm.sessionID),
		zap.Int64("boundary_turn", b),
		zap.Int("rows_folded", count),
		zap.Int64("seq_lo", loSeq),
		zap.Int64("seq_hi", hiSeq),
		zap.Int("version", n1),
		zap.Int("output_bytes", len(newText)),
		zap.Bool("fallback", fallback))
	return true
}

// firstUserLine returns the first line of the region's first user message —
// the §5.4.5 fallback citation.
func firstUserLine(region []Message) string {
	for i := range region {
		if region[i].Role == "user" && region[i].Content != "" {
			line, _, _ := strings.Cut(region[i].Content, "\n")
			return line
		}
	}
	return ""
}

// foldedSkillLoads returns the names of skills whose manage_skills load pair —
// the load call paired with a "Skill loaded: " confirmation — lies inside the
// region.
func foldedSkillLoads(region []Message) []string {
	loadCalls := make(map[string]string) // tool_use_id → skill name
	for i := range region {
		for _, c := range region[i].ToolCalls {
			if c.Name != "manage_skills" || c.ID == "" {
				continue
			}
			if action, _ := c.Input["action"].(string); action != "load" {
				continue
			}
			if name, _ := c.Input["name"].(string); name != "" {
				loadCalls[c.ID] = name
			}
		}
	}
	var names []string
	seen := make(map[string]bool)
	for i := range region {
		m := &region[i]
		if m.Role != "tool" || m.ToolUseID == "" {
			continue
		}
		name := loadCalls[m.ToolUseID]
		if name == "" || seen[name] {
			continue
		}
		if strings.HasPrefix(m.Content, "Skill loaded: ") {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// defaultProtectedRecentTurns is K (HLD §5.1): protected newest user turns.
const defaultProtectedRecentTurns = 5

// SetProtectedRecentTurns configures K (config ProtectedRecentTurns, §9).
func (sm *SegmentedMemory) SetProtectedRecentTurns(k int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if k > 0 {
		sm.protectedRecentTurns = k
	}
}

// SetAdvertisedToolsBytes records the serialized bytes of the advertised tool
// schemas — the provider tools parameter as built (HLD §2, blueprint A6). The
// schemas' bytes are part of the compiled artifact and therefore of the
// estimate; nothing acts on the derived token count.
func (sm *SegmentedMemory) SetAdvertisedToolsBytes(bytes int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if bytes < 0 {
		bytes = 0
	}
	sm.kernelBytes = bytes
	sm.recountKernel()
	sm.kernelDirty = false
	sm.updateTokenCount()
	sm.tokenCountDirty = false
}

// SetSkillDeactivationHook wires the skills orchestrator's deactivation path,
// used when fold flags a region containing a manage_skills load pair.
func (sm *SegmentedMemory) SetSkillDeactivationHook(fn func(sessionID, skillName string)) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.skillDeactivation = fn
}

// setSummary installs a restored summary version (reload, HLD §8).
func (sm *SegmentedMemory) setSummary(n int, text string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.summary = summaryState{n: n, text: text}
	sm.cachedL2Tokens = sm.tokenCounter.CountTokens(text)
	sm.updateTokenCount()
	sm.tokenCountDirty = false
}
