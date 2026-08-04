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
package shuttle

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/teradata-labs/loom/pkg/session"
)

// D-7 acceptance tests for ask → HITL approval at the tool-admit seam. Each case
// drives the shuttle Executor's real Execute interface with an Ask-returning hook
// wired to the concrete NewHITLAskResolver over a real in-memory HITL store, and
// asserts the seam behavior a separate actor resolving the request produces: the
// pending approval request raised at the seam, and the terminal Result plus the
// tool's ExecuteCount after the request is approved, rejected, or left to time out.
//
// A separate actor resolves the request via the store's RespondToRequest (the SE-3
// poll-respond responder helper) — the model never self-approves.

// askFixture wires an Executor whose only admission hook returns Ask, resolved by
// the concrete hitlAskResolver over a real in-memory HITL store. The call context
// carries the session ID, and the identity resolver supplies the user ID, so the
// resolver scopes the raised request to both.
type askFixture struct {
	exec  *Executor
	tool  *MockTool
	store *InMemoryHumanRequestStore
	ctx   context.Context
}

// newAskFixture builds an askFixture. timeout bounds the resolver's blocking wait
// and poll is its store poll interval; sessionID/userID are the identities the
// raised approval request is scoped to.
func newAskFixture(t *testing.T, toolName, sessionID, userID string, timeout, poll time.Duration) askFixture {
	t.Helper()

	reg := NewRegistry()
	tool := &MockTool{MockName: toolName}
	reg.Register(tool)

	store := NewInMemoryHumanRequestStore()

	exec := NewExecutor(reg)
	exec.SetIdentityResolver(func(context.Context) string { return userID })
	exec.SetAdmissionChain(NewChain(
		[]Hook{fixedHook{decision: Decision{Kind: Ask}}},
		nil,
		NewHITLAskResolver(store, timeout, poll),
	))

	return askFixture{
		exec:  exec,
		tool:  tool,
		store: store,
		ctx:   session.WithSessionID(context.Background(), sessionID),
	}
}

// waitForPending blocks until exactly one pending request is present in the store
// and returns it, failing the test if none appears within the deadline. Called on
// the test goroutine while the resolver blocks in a separate goroutine.
func waitForPending(t *testing.T, store *InMemoryHumanRequestStore) *HumanRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := store.ListPending(context.Background())
		require.NoError(t, err)
		if len(pending) == 1 {
			return pending[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no pending approval request appeared at the seam")
	return nil
}

// respondOncePending is the SE-3 responder: a background actor that polls
// ListPending and resolves the first pending request with the given status,
// mirroring the human_tool poll-respond pattern (human_tool_test.go).
func respondOncePending(store *InMemoryHumanRequestStore, status string) {
	go func() {
		ctx := context.Background()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			pending, err := store.ListPending(ctx)
			if err == nil && len(pending) > 0 {
				_ = store.RespondToRequest(ctx, pending[0].ID, status, "", "reviewer@example.com", nil)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// failingHumanStore is a HumanRequestStore whose Store always errors, exercising
// the resolver's fail-closed path when the approval request cannot be raised.
type failingHumanStore struct{}

func (failingHumanStore) Store(context.Context, *HumanRequest) error {
	return fmt.Errorf("hitl store unavailable")
}
func (failingHumanStore) Get(context.Context, string) (*HumanRequest, error) {
	return nil, fmt.Errorf("hitl store unavailable")
}
func (failingHumanStore) Update(context.Context, *HumanRequest) error {
	return fmt.Errorf("hitl store unavailable")
}
func (failingHumanStore) ListPending(context.Context) ([]*HumanRequest, error) {
	return nil, fmt.Errorf("hitl store unavailable")
}
func (failingHumanStore) ListBySession(context.Context, string) ([]*HumanRequest, error) {
	return nil, fmt.Errorf("hitl store unavailable")
}
func (failingHumanStore) Close() error { return nil }

// AC1: on an ask decision, an approval HumanRequest is created at the seam and
// appears pending in the HITL store, scoped to the call's session and user, while
// the resolver blocks awaiting a response.
func TestAskHITL_AskRaisesPendingApprovalAtSeam(t *testing.T) {
	f := newAskFixture(t, "needs_approval", "sess-1", "user-1", 2*time.Second, 10*time.Millisecond)

	// Drive the gated call; it blocks in the resolver until the request resolves.
	done := make(chan *Result, 1)
	go func() {
		res, _ := f.exec.Execute(f.ctx, "needs_approval", map[string]interface{}{"k": "v"})
		done <- res
	}()

	// While the resolver blocks, exactly one pending "approval" request is present,
	// scoped to the call's session and user.
	hr := waitForPending(t, f.store)
	require.Equal(t, "approval", hr.RequestType)
	require.Equal(t, "pending", hr.Status)
	require.Equal(t, "sess-1", hr.SessionID)
	require.Equal(t, "user-1", hr.Context["user_id"])

	// Resolve so the blocked call returns and the driving goroutine exits.
	require.NoError(t, f.store.RespondToRequest(context.Background(), hr.ID, "approved", "", "reviewer@example.com", nil))
	<-done
}

// AC2: approved → the call is admitted and the tool runs.
func TestAskHITL_Approved_AdmitsAndRunsTool(t *testing.T) {
	f := newAskFixture(t, "needs_approval", "sess-1", "user-1", 2*time.Second, 10*time.Millisecond)

	respondOncePending(f.store, "approved")

	res, err := f.exec.Execute(f.ctx, "needs_approval", nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Success)
	require.Nil(t, res.Error)
	require.Equal(t, 1, f.tool.ExecuteCount, "an approved call runs the tool exactly once")
}

// AC3: rejected → the call is denied and the tool does not run. Any non-"approved"
// terminal status resolves to Deny, mirroring the resolver's fail-closed default.
func TestAskHITL_NonApproved_DeniesAndSkipsTool(t *testing.T) {
	for _, status := range []string{"rejected", "timeout", "responded"} {
		t.Run(status, func(t *testing.T) {
			f := newAskFixture(t, "needs_approval", "sess-1", "user-1", 2*time.Second, 10*time.Millisecond)

			respondOncePending(f.store, status)

			res, err := f.exec.Execute(f.ctx, "needs_approval", nil)
			require.NoError(t, err, "a policy deny is a Result, not a transport error")
			require.NotNil(t, res)
			require.False(t, res.Success)
			require.NotNil(t, res.Error)
			require.Equal(t, "permission_denied", res.Error.Code)
			require.Equal(t, 0, f.tool.ExecuteCount, "a non-approved response never runs the tool")
		})
	}
}

// AC4: no response within the configured timeout → deny, tool does not run
// (fail-closed). Guards C-005 edge "ask timeout → deny".
func TestAskHITL_Timeout_DeniesFailClosed(t *testing.T) {
	f := newAskFixture(t, "needs_approval", "sess-1", "user-1", 60*time.Millisecond, 10*time.Millisecond)

	res, err := f.exec.Execute(f.ctx, "needs_approval", nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.Success)
	require.NotNil(t, res.Error)
	require.Equal(t, "permission_denied", res.Error.Code)
	require.Equal(t, 0, f.tool.ExecuteCount, "an unanswered request denies without running the tool")

	// The unanswered request is still pending in the store after the deny.
	pending, err := f.store.ListPending(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

// AC4 (sibling fail-closed): a context canceled before a response denies and the
// tool does not run.
func TestAskHITL_ContextCanceled_DeniesFailClosed(t *testing.T) {
	f := newAskFixture(t, "needs_approval", "sess-1", "user-1", 5*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan *Result, 1)
	go func() {
		res, _ := f.exec.Execute(ctx, "needs_approval", nil)
		done <- res
	}()

	// Cancel only after the request is raised, proving the resolver was blocking.
	waitForPending(t, f.store)
	cancel()

	res := <-done
	require.NotNil(t, res)
	require.False(t, res.Success)
	require.NotNil(t, res.Error)
	require.Equal(t, "permission_denied", res.Error.Code)
	require.Equal(t, 0, f.tool.ExecuteCount, "a canceled approval never runs the tool")
}

// AC4 (sibling fail-closed): a store that cannot raise the approval request denies
// and the tool does not run.
func TestAskHITL_StoreFailure_DeniesFailClosed(t *testing.T) {
	reg := NewRegistry()
	tool := &MockTool{MockName: "needs_approval"}
	reg.Register(tool)

	exec := NewExecutor(reg)
	exec.SetAdmissionChain(NewChain(
		[]Hook{fixedHook{decision: Decision{Kind: Ask}}},
		nil,
		NewHITLAskResolver(failingHumanStore{}, time.Second, 10*time.Millisecond),
	))

	res, err := exec.Execute(session.WithSessionID(context.Background(), "sess-1"), "needs_approval", nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.Success)
	require.NotNil(t, res.Error)
	require.Equal(t, "permission_denied", res.Error.Code)
	require.Equal(t, 0, tool.ExecuteCount, "a store that cannot raise the request never runs the tool")
}
