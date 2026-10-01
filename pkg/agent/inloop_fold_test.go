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

// In-loop full collapse — the model's fold reply carries only the summary;
// the harness owns the range: operator rows verbatim, recent assistant text
// kept with pairs stripped whole, pending pair untouched, all other tool
// matter and old thread excised. An empty summary folds nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedInloopSession(sm *SegmentedMemory) {
	big := strings.Repeat("r", 3000)
	sm.AddMessage(context.Background(), Message{Role: "user", Content: "the JOB", Turn: 1})
	for turn := int64(1); turn <= 8; turn++ {
		sm.AddMessage(context.Background(), Message{Role: "assistant", Content: "thinking turn " + string(rune('0'+turn)), Turn: turn,
			ToolCalls: []ToolCall{{ID: "c" + string(rune('0'+turn)), Name: "shell_execute"}}})
		sm.AddMessage(context.Background(), Message{Role: "tool", ToolUseID: "c" + string(rune('0'+turn)), Content: big, Turn: turn})
	}
	sm.AddMessage(context.Background(), Message{Role: "user", Content: "operator ruling: keep it exact", Turn: 5})
	sm.AddMessage(context.Background(), Message{Role: "assistant", Content: "current work", Turn: 9,
		ToolCalls: []ToolCall{{ID: "c9", Name: "shell_execute"}}})
	sm.AddMessage(context.Background(), Message{Role: "tool", ToolUseID: "c9", Content: "pending result", Turn: 9})
}

// TestInloopFold_ParseLenient — the summary is found inside prose and strict
// JSON alike; junk and empty summaries are refused.
func TestInloopFold_ParseLenient(t *testing.T) {
	s, ok := parseFoldReply(`Here you go: {"summary": "state of work"} hope that helps`)
	require.True(t, ok)
	assert.Equal(t, "state of work", s)
	_, ok = parseFoldReply("no json here")
	assert.False(t, ok)
	_, ok = parseFoldReply(`{"summary": "  "}`)
	assert.False(t, ok)
}

// TestInloopFold_FullCollapseSemantics — one call: operator rows survive
// verbatim, recent assistant text survives stripped of calls, the pending
// pair is whole, everything else is gone, the summary is L2.
func TestInloopFold_FullCollapseSemantics(t *testing.T) {
	sm := newCompileMemory(t)
	sm.SetProtectedRecentTurns(2)
	seedInloopSession(sm)

	applied, _ := sm.ApplyFoldReply(context.Background(), "LTV fix in hand; nulls in discounts ruled the cause")

	require.True(t, applied)
	assert.Contains(t, sm.summary.text, "nulls in discounts")
	var users, results, keptAssistants int
	var pendingWhole bool
	for _, m := range sm.GetMessages() {
		switch m.Role {
		case "user":
			users++
		case "tool":
			results++
		case "assistant":
			keptAssistants++
			if m.Content == "current work" {
				pendingWhole = len(m.ToolCalls) == 1
			} else {
				assert.Nil(t, m.ToolCalls, "kept recent thread is stripped of calls")
			}
		}
	}
	assert.Equal(t, 2, users, "the JOB and the mid-session ruling both survive verbatim")
	assert.Equal(t, 1, results, "only the pending pair's result remains")
	assert.True(t, pendingWhole, "the pending pair keeps its call signature")
	assert.GreaterOrEqual(t, keptAssistants, 2, "recent thread text + pending assistant")
}

// TestInloopFold_EmptySummaryRefused — a fold without a summary is amnesia.
func TestInloopFold_EmptySummaryRefused(t *testing.T) {
	sm := newCompileMemory(t)
	seedInloopSession(sm)
	before := len(sm.GetMessages())

	applied, _ := sm.ApplyFoldReply(context.Background(), "  ")

	assert.False(t, applied)
	assert.Len(t, sm.GetMessages(), before)
}

// TestInloopFold_NeedsFoldTracksMark — under the mark no fold is asked for;
// past it, one is.
func TestInloopFold_NeedsFoldTracksMark(t *testing.T) {
	sm := NewSegmentedMemory("ROM", 4000, 400) // tiny window: mark ≈ a few K tokens
	sm.SetThreshold(1024)
	assert.False(t, sm.NeedsFold())
	for turn := int64(1); turn <= 9; turn++ {
		sm.AddMessage(context.Background(), Message{Role: "assistant", Content: strings.Repeat("x ", 800), Turn: turn})
	}
	assert.True(t, sm.NeedsFold())
}

// TestInloopFold_InFlightResultsNeverTrigger — the estimate excludes tool
// results the model has not yet consumed (rows after the last assistant row).
// A fold fired on their mass cannot commit — the collapse protects the
// pending pair — so unconsumed mass must not arm the fold. Once the model's
// next response consumes the batch, the same mass counts and the fold arms.
func TestInloopFold_InFlightResultsNeverTrigger(t *testing.T) {
	sm := NewSegmentedMemory("ROM", 4000, 400) // tiny window: mark ≈ a few K tokens
	sm.SetThreshold(100000)                    // no offload stubs — raw results in the estimate
	ctx := context.Background()

	sm.AddMessage(ctx, Message{Role: "user", Content: "find the bug", Turn: 1})
	sm.AddMessage(ctx, Message{Role: "assistant", Turn: 1,
		ToolCalls: []ToolCall{{ID: "c1", Name: "file_read", Input: map[string]interface{}{}}}})
	// A huge unconsumed batch: far past the mark on its own.
	sm.AddMessage(ctx, Message{Role: "tool", ToolUseID: "c1", Content: strings.Repeat("y ", 8000), Turn: 1})

	assert.False(t, sm.NeedsFold(),
		"unconsumed results are unshedable — they must not arm the fold")

	// The model consumes the batch: the same mass is now past, and sheddable.
	sm.AddMessage(ctx, Message{Role: "assistant", Content: "the bug is in the parser", Turn: 2})

	assert.True(t, sm.NeedsFold(),
		"once consumed, the batch counts and the fold arms")
}
