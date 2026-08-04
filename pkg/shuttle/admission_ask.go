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
	"time"

	"github.com/google/uuid"
)

// hitlAskResolver turns the chain's Ask verdict into a terminal Allow/Deny by
// raising a human approval request and blocking on the HITL store until a
// separate actor resolves it. It implements AskResolver. A separate actor
// resolves via RespondToRequest — the harness raises the request, the human
// decides; the model cannot self-approve.
type hitlAskResolver struct {
	store   HumanRequestStore
	timeout time.Duration // how long to block before failing closed
	poll    time.Duration // store poll interval
}

// NewHITLAskResolver builds the Ask resolver wired into ChainDeps.Ask. timeout
// bounds the turn-blocking wait (default 300s when non-positive); poll is the
// store poll interval (default 1s, mirroring ContactHumanConfig).
func NewHITLAskResolver(store HumanRequestStore, timeout, poll time.Duration) AskResolver {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	if poll <= 0 {
		poll = 1 * time.Second
	}
	return &hitlAskResolver{store: store, timeout: timeout, poll: poll}
}

// Resolve creates a pending "approval" HumanRequest scoped to the call's session
// and user, then blocks polling the store until the request leaves "pending" or
// the timeout elapses. "approved" admits the call (Allow); "rejected", "timeout",
// a deadline, a store failure, or context cancellation all Deny (fail-closed).
// The block is synchronous within the agent turn, bounded by timeout.
func (r *hitlAskResolver) Resolve(req AdmissionRequest, d Decision) Decision {
	if r == nil || r.store == nil {
		return Decision{Kind: Deny, Reason: "approval required but no HITL store is configured"}
	}

	ctx := req.Ctx
	if ctx == nil {
		return Decision{Kind: Deny, Reason: "approval required but request has no context"}
	}

	now := time.Now()
	hr := &HumanRequest{
		ID:        uuid.New().String(),
		SessionID: req.SessionID,
		Question:  fmt.Sprintf("Approve tool call %q?", req.ToolName),
		Context: map[string]interface{}{
			"tool":    req.ToolName,
			"user_id": req.UserID,
			"reason":  d.Reason,
		},
		RequestType: "approval",
		Priority:    "normal",
		Timeout:     r.timeout,
		CreatedAt:   now,
		ExpiresAt:   now.Add(r.timeout),
		Status:      "pending",
	}

	if err := r.store.Store(ctx, hr); err != nil {
		return Decision{Kind: Deny, Reason: fmt.Sprintf("failed to raise approval request: %v", err)}
	}

	return r.wait(ctx, hr.ID)
}

// wait polls the store until the request is resolved, the deadline passes, or
// the context is canceled. Only an explicit "approved" status admits; every
// other terminal condition denies (fail-closed).
func (r *hitlAskResolver) wait(ctx context.Context, requestID string) Decision {
	deadline := time.Now().Add(r.timeout)
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return Decision{Kind: Deny, Reason: "approval canceled before a response"}
		case <-ticker.C:
			if time.Now().After(deadline) {
				return Decision{Kind: Deny, Reason: "approval timed out"}
			}
			hr, err := r.store.Get(ctx, requestID)
			if err != nil {
				continue // transient read; retry until the deadline
			}
			if hr.Status == "pending" {
				continue
			}
			if hr.Status == "approved" {
				return Decision{Kind: Allow}
			}
			return Decision{Kind: Deny, Reason: fmt.Sprintf("approval %s", hr.Status)}
		}
	}
}
