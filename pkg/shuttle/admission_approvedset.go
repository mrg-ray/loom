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
	"encoding/json"
	"fmt"

	loomv1 "github.com/teradata-labs/loom/gen/go/loom/v1"
	"github.com/teradata-labs/loom/pkg/session"
	"github.com/teradata-labs/loom/pkg/storage"
)

// approvedSet is the membership layer over SharedMemoryStore. The store is a
// blob-by-id keyval with no membership op; approvedSet stores one blob per
// state key — the marshalled collection of approved CallIdentity strings — and
// answers membership by decoding it. The store stamps each blob with the
// recording session and resolves a foreign session's read as not-found, so the
// per-session (transitively per-tenant) isolation is inherited from the store,
// not re-implemented here.
type approvedSet struct {
	mem *storage.SharedMemoryStore
}

// NewApprovedSet builds an ApprovedSetAccessor backed by the shared-memory
// store. The composition layer binds it to the executor so a gated-allowlist
// reads it as AdmissionRequest.State.
func NewApprovedSet(mem *storage.SharedMemoryStore) ApprovedSetAccessor {
	return &approvedSet{mem: mem}
}

// approvedSetID is the deterministic blob id for a state key's approved set.
func approvedSetID(stateKey string) string {
	return "admission/approved/" + stateKey
}

// Record writes the approved set for stateKey as the marshalled collection of
// CallIdentity strings, owned by the current session (from ctx) so only that
// session — hence that tenant — can later read it.
func (a *approvedSet) Record(ctx context.Context, stateKey string, ids []CallIdentity) error {
	if a.mem == nil {
		return fmt.Errorf("approved-set store not configured")
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = string(id)
	}
	data, err := json.Marshal(strs)
	if err != nil {
		return fmt.Errorf("marshal approved set for %q: %w", stateKey, err)
	}
	sessionID := session.SessionIDFromContext(ctx)
	if _, err := a.mem.Store(approvedSetID(stateKey), data, "application/json", map[string]string{
		"source": "admission_approved_set",
	}, sessionID); err != nil {
		return fmt.Errorf("store approved set for %q: %w", stateKey, err)
	}
	return nil
}

// Contains reports whether id is in stateKey's approved set for the caller's
// session. A blob owned by another session resolves to not-found, and no blob at
// all is the same not-found — both mean "no membership" and yield false, so a
// call before any set is established and a cross-tenant call both deny at the
// gated hook. A decode failure of a present blob is a real error (fail-closed).
func (a *approvedSet) Contains(ctx context.Context, stateKey string, id CallIdentity) (bool, error) {
	if a.mem == nil {
		return false, nil
	}
	sessionID := session.SessionIDFromContext(ctx)
	data, err := a.mem.Get(&loomv1.DataReference{Id: approvedSetID(stateKey)}, sessionID)
	if err != nil {
		return false, nil
	}
	var strs []string
	if err := json.Unmarshal(data, &strs); err != nil {
		return false, fmt.Errorf("decode approved set for %q: %w", stateKey, err)
	}
	for _, s := range strs {
		if CallIdentity(s) == id {
			return true, nil
		}
	}
	return false, nil
}
