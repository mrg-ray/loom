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

package contextoptimiser

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/types"
)

// Gates are the checks a machine can settle with certainty. They run after every
// turn and the route halts on the first failure, so a structurally broken run
// never produces stages for anyone — or any model — to read.
//
// Everything that needs judgement ("is this context RIGHT") is deliberately not
// here: that is the reading step, against expected/.

// gateErr names the turn and the check that failed.
type gateErr struct {
	turn  int
	name  string
	cause string
}

func (e *gateErr) Error() string {
	return fmt.Sprintf("turn %d: gate %q failed: %s", e.turn, e.name, e.cause)
}

// scriptIntact fails when the route ran past its script. Past the script the
// provider is improvising, the run is no longer deterministic, and every stage
// after that point is meaningless.
func scriptIntact(turn int, llm *scriptedLLM) error {
	if llm.exhausted() {
		return &gateErr{turn, "script-intact",
			"the route consumed more provider calls than the script defines — stages past this point are not deterministic"}
	}
	return nil
}

// wireValid checks the two facts that make a context dispatchable at all: every
// tool result belongs to a tool call that precedes it, and no two user-role
// messages sit next to each other (which Anthropic-family providers reject).
func wireValid(turn int, s stage) error {
	seen := map[string]bool{}
	for i, m := range s.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				seen[tc.ID] = true
			}
		}
		if m.Role == "tool" {
			if m.ToolUseID == "" {
				return &gateErr{turn, "wire-valid", fmt.Sprintf("message %d is a tool result with no tool_use_id", i)}
			}
			if !seen[m.ToolUseID] {
				return &gateErr{turn, "wire-valid",
					fmt.Sprintf("message %d is a tool result for %q with no preceding tool call — orphan", i, m.ToolUseID)}
			}
		}
		if i > 0 && m.Role == "user" && s.Messages[i-1].Role == "user" {
			return &gateErr{turn, "wire-valid",
				fmt.Sprintf("messages %d and %d are both user-role — the provider rejects consecutive user messages", i-1, i)}
		}
	}
	return nil
}

// bodyAppearsOnce checks that a loaded skill's body is in the context exactly
// once. Zero means the load delivered nothing; more than one means it was
// duplicated — both are silent faults that no ordinary assertion watches.
func bodyAppearsOnce(turn int, s stage, marker string) error {
	n := 0
	for _, m := range s.Messages {
		if strings.Contains(m.Content, marker) {
			n++
		}
	}
	if n != 1 {
		return &gateErr{turn, "body-appears-once",
			fmt.Sprintf("skill body %q appears %d times in the compiled context, expected exactly 1", marker, n)}
	}
	return nil
}

// contextAdvanced checks that a turn actually changed the context. A turn that
// leaves the compiled context byte-identical to the previous stage did nothing
// observable, which is the shape a silent no-op takes.
func contextAdvanced(turn int, prev, cur stage) error {
	if len(prev.Messages) == 0 {
		return nil
	}
	if len(prev.Messages) != len(cur.Messages) {
		return nil
	}
	for i := range prev.Messages {
		if prev.Messages[i].Content != cur.Messages[i].Content || prev.Messages[i].Role != cur.Messages[i].Role {
			return nil
		}
	}
	return &gateErr{turn, "context-advanced",
		"the compiled context is byte-identical to the previous provider call — the turn changed nothing"}
}

// restoreMatchesLive is the strongest gate and needs no expectation at all: it
// compares two things the system itself produced. The live session's messages
// and the same session replayed from durable storage must agree — same count,
// same roles, same content, same order.
//
// A divergence here is a real finding wherever it appears: either the live
// context holds something never persisted, or replay reconstructs something the
// live session does not have.
func restoreMatchesLive(turn int, live, restored []types.Message) error {
	if len(live) != len(restored) {
		return &gateErr{turn, "restore-matches-live",
			fmt.Sprintf("live session has %d messages, the restored twin has %d", len(live), len(restored))}
	}
	for i := range live {
		if live[i].Role != restored[i].Role {
			return &gateErr{turn, "restore-matches-live",
				fmt.Sprintf("message %d: live role %q, restored role %q", i, live[i].Role, restored[i].Role)}
		}
		if live[i].Content != restored[i].Content {
			return &gateErr{turn, "restore-matches-live",
				fmt.Sprintf("message %d (%s): content differs\n  live:     %s\n  restored: %s",
					i, live[i].Role, trim(live[i].Content), trim(restored[i].Content))}
		}
		if live[i].ToolUseID != restored[i].ToolUseID {
			return &gateErr{turn, "restore-matches-live",
				fmt.Sprintf("message %d: live tool_use_id %q, restored %q", i, live[i].ToolUseID, restored[i].ToolUseID)}
		}
	}
	return nil
}

// compiledContextMatches compares what the two sessions would actually DISPATCH
// — ROM, any summary layer, and the live messages, assembled exactly as the
// provider would receive them. The message-list comparison above cannot see the
// assembled layers: a live and a replayed session can hold identical messages and
// still compile to different contexts if one carries a summary the other lacks.
func compiledContextMatches(turn int, live, restored []types.Message) error {
	if len(live) != len(restored) {
		return &gateErr{turn, "compiled-context-matches",
			fmt.Sprintf("live compiles to %d messages, the restored twin to %d", len(live), len(restored))}
	}
	for i := range live {
		if live[i].Role != restored[i].Role || live[i].Content != restored[i].Content {
			return &gateErr{turn, "compiled-context-matches",
				fmt.Sprintf("compiled message %d differs\n  live     [%s]: %s\n  restored [%s]: %s",
					i, live[i].Role, trim(live[i].Content), restored[i].Role, trim(restored[i].Content))}
		}
	}
	return nil
}

// compiledContext returns the context a session would dispatch, read straight
// from its tiered memory — no provider call, so reading it changes nothing.
func compiledContext(t *testing.T, s *types.Session) []types.Message {
	t.Helper()
	if s == nil || s.SegmentedMem == nil {
		return nil
	}
	sm, ok := s.SegmentedMem.(*agent.SegmentedMemory)
	if !ok {
		t.Fatalf("session %s carries no *agent.SegmentedMemory", s.ID)
	}
	return sm.GetMessagesForLLM()
}

func trim(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 160 {
		return s[:160] + fmt.Sprintf(" … [%d chars]", len(s))
	}
	return s
}

// liveSession returns the session the route has been driving.
func (r *rig) liveSession(t *testing.T, sessionID string) *types.Session {
	t.Helper()
	s, ok := r.agent.GetSession(sessionID)
	if !ok {
		t.Fatalf("live session %s not found", sessionID)
	}
	return s
}

// restoredSession builds a twin over the same durable store and replays the
// session, returning what the twin reconstructed.
func (r *rig) restoredSession(t *testing.T, sessionID string, maxContext, reservedOutput, offloadThreshold int) *types.Session {
	t.Helper()
	twinLLM := &scriptedLLM{}
	_, _, mem := buildAgentWithMemory(t, twinLLM, r.store, r.skillsDir, r.patternsDir,
		maxContext, reservedOutput, offloadThreshold, false)
	s := mem.GetOrCreateSession(context.Background(), sessionID)
	if s == nil {
		t.Fatalf("restored session %s is nil", sessionID)
	}
	return s
}

// setCompressor installs a compressor on the live agent's memory, so a test can
// count how often compaction actually ran.
func (r *rig) setCompressor(c agent.MemoryCompressor) {
	r.mem.SetCompressor(c)
}

// restoredSessionWithCompressor replays a session on a twin wired with the given
// compressor, so compressor calls made during reload are attributable.
func (r *rig) restoredSessionWithCompressor(t *testing.T, sessionID string, c agent.MemoryCompressor,
	maxContext, reservedOutput, offloadThreshold int) *types.Session {
	t.Helper()
	_, _, mem := buildAgentWithMemory(t, &scriptedLLM{}, r.store, r.skillsDir, r.patternsDir,
		maxContext, reservedOutput, offloadThreshold, false)
	mem.SetCompressor(c)
	if r.maxL2Tok > 0 {
		mem.SetMaxL2Tokens(r.maxL2Tok) // a deployment applies its L2 bound on every load
	}
	s := mem.GetOrCreateSession(context.Background(), sessionID)
	if s == nil {
		t.Fatalf("restored session %s is nil", sessionID)
	}
	return s
}

// advertisedToolNames reports what the session would advertise, read from the
// most recent dump record for it.
func advertisedToolNames(t *testing.T, r *rig, sessionID string) []string {
	t.Helper()
	stages := r.readStages(t)
	for i := len(stages) - 1; i >= 0; i-- {
		if stages[i].SessionID == sessionID {
			return stages[i].toolNames()
		}
	}
	return nil
}

// setMaxL2Tokens shrinks the rendered L2 block bound so a scripted route can
// reach L2 saturation and demotion in a handful of folds.
func (r *rig) setMaxL2Tokens(n int) {
	r.maxL2Tok = n
	r.mem.SetMaxL2Tokens(n)
}
