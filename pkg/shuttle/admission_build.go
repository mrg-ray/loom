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
	"fmt"
	"regexp"
	"strings"
)

// CustomHookRegistry resolves a custom binding's Name to the constructor that
// builds its hook. The constructor receives the full binding, so a custom hook
// is placed in the chain with the binding's scope and matcher like any library
// policy. A name with no registered constructor is a build error.
type CustomHookRegistry interface {
	lookup(name string) (func(HookBinding) (Hook, error), bool)
}

// ChainDeps carries the collaborators BuildChainFromConfig needs to turn
// bindings into live hooks: the name-level permission checker (folded in as the
// first hook), the approved-set accessor a gated-allowlist reads, the resolver
// an Ask verdict defers to, and the registry a custom binding resolves against.
type ChainDeps struct {
	Perm        *PermissionChecker
	ApprovedSet ApprovedSetAccessor
	Ask         AskResolver
	Custom      CustomHookRegistry
}

// BuildChainFromConfig assembles the admission chain from a tools.hooks config.
// The name-level permission hook is placed first when a checker is supplied,
// then one hook per binding in config order. A malformed binding — unknown
// kind, empty scope, bad matcher/read pattern, a gated-allowlist missing
// state_key/source_tool, or a custom name with no registered hook — is an
// error, so serve aborts rather than run silently ungoverned (fail-closed).
func BuildChainFromConfig(cfg HooksConfig, deps ChainDeps) (*Chain, error) {
	hooks := make([]Hook, 0, len(cfg.Bindings)+1)
	if deps.Perm != nil {
		hooks = append(hooks, newPermHook(deps.Perm))
	}
	for i, b := range cfg.Bindings {
		h, err := buildHook(b, deps)
		if err != nil {
			return nil, fmt.Errorf("tools.hooks[%d] (kind %q): %w", i, b.Kind, err)
		}
		hooks = append(hooks, h)
	}
	return NewChain(hooks, nil, deps.Ask), nil
}

// buildHook compiles one binding into a Hook, validating every field the kind
// requires. Selection (scope ∧ matcher) is compiled here for every kind; the
// decision body is the kind's policy.
func buildHook(b HookBinding, deps ChainDeps) (Hook, error) {
	if strings.TrimSpace(b.Scope) == "" {
		return nil, fmt.Errorf("scope is required")
	}
	scope := NewToolScope(b.Scope)
	matcher, err := b.Matcher.Compile()
	if err != nil {
		return nil, err
	}

	switch b.Kind {
	case "denylist":
		return denylistHook(scope, matcher), nil
	case "gated-allowlist":
		return gatedAllowlistHook(b, scope, matcher, deps)
	case "audit":
		return libraryAuditHook{scope: scope, matcher: matcher}, nil
	case "custom":
		return customHook(b, deps)
	default:
		return nil, fmt.Errorf("unknown kind (want gated-allowlist|denylist|audit|custom)")
	}
}

// libraryHook is a config-built policy whose Matches is the D-2 selection
// contract — scope ∧ matcher — and whose Evaluate is the kind's decision body,
// supplied as a closure at build time.
type libraryHook struct {
	scope   ToolScope
	matcher Matcher
	eval    func(req AdmissionRequest) Decision
}

// Matches reports whether the binding governs this call: its tool is in scope
// and its params satisfy the matcher.
func (h libraryHook) Matches(req AdmissionRequest) bool {
	return h.scope.MatchesTool(req.ToolName) && h.matcher.MatchesParams(req.Params)
}

// Evaluate returns the policy's verdict for a governed call.
func (h libraryHook) Evaluate(req AdmissionRequest) Decision {
	return h.eval(req)
}

// denylistHook denies any governed call. Because Matches already gates on the
// matcher, a governed call is by definition one whose params match the deny
// selector.
func denylistHook(scope ToolScope, matcher Matcher) libraryHook {
	return libraryHook{
		scope:   scope,
		matcher: matcher,
		eval: func(req AdmissionRequest) Decision {
			return Decision{Kind: Deny, Reason: "denied by denylist"}
		},
	}
}

// gatedAllowlistHook admits a governed write only when its call identity is in
// the approved set at the binding's state_key; a read-pattern match is admitted
// with no entry. A missing entry, an accessor error, or no accessor wired is a
// hard deny (fail-closed) — never an ask.
func gatedAllowlistHook(b HookBinding, scope ToolScope, matcher Matcher, deps ChainDeps) (Hook, error) {
	if strings.TrimSpace(b.StateKey) == "" {
		return nil, fmt.Errorf("gated-allowlist requires state_key")
	}
	if strings.TrimSpace(b.SourceTool) == "" {
		return nil, fmt.Errorf("gated-allowlist requires source_tool")
	}
	var readRe *regexp.Regexp
	if b.ReadPattern != "" {
		re, err := regexp.Compile(b.ReadPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid read_pattern %q: %w", b.ReadPattern, err)
		}
		readRe = re
	}
	stateKey := b.StateKey
	stmtParam := b.StmtParam
	accessor := deps.ApprovedSet

	eval := func(req AdmissionRequest) Decision {
		stmt := stmtValue(req.Params, stmtParam)
		if readRe != nil && readRe.MatchString(stmt) {
			return Decision{Kind: Allow}
		}
		state := req.State
		if state == nil {
			state = accessor
		}
		if state == nil {
			return Decision{Kind: Deny, Reason: "gated-allowlist has no approved-set store"}
		}
		ok, err := state.Contains(req.Ctx, stateKey, CallIdentity(stmt))
		if err != nil {
			return Decision{Kind: Deny, Reason: fmt.Sprintf("approved-set lookup failed: %v", err)}
		}
		if ok {
			return Decision{Kind: Allow}
		}
		return Decision{Kind: Deny, Reason: "call not in approved set"}
	}

	return libraryHook{scope: scope, matcher: matcher, eval: eval}, nil
}

// customHook resolves a custom binding's Name to its constructor and builds the
// hook from the binding, so the binding's scope and matcher apply. An unnamed
// binding, an unconfigured registry, or an unregistered name is a build error.
func customHook(b HookBinding, deps ChainDeps) (Hook, error) {
	if strings.TrimSpace(b.Name) == "" {
		return nil, fmt.Errorf("custom hook requires name")
	}
	if deps.Custom == nil {
		return nil, fmt.Errorf("custom hook %q named but no custom hook registry is configured", b.Name)
	}
	ctor, ok := deps.Custom.lookup(b.Name)
	if !ok {
		return nil, fmt.Errorf("custom hook %q is not registered", b.Name)
	}
	return ctor(b)
}

// libraryAuditHook records but never gates: Evaluate is Allow, and the AuditHook
// marker reports the chain's final decision to the persist path.
type libraryAuditHook struct {
	scope   ToolScope
	matcher Matcher
}

// Matches reports whether the audit binding governs this call.
func (h libraryAuditHook) Matches(req AdmissionRequest) bool {
	return h.scope.MatchesTool(req.ToolName) && h.matcher.MatchesParams(req.Params)
}

// Evaluate always allows; audit is observability, not enforcement.
func (h libraryAuditHook) Evaluate(req AdmissionRequest) Decision {
	return Decision{Kind: Allow}
}

// AuditDecisionFor renders the chain's final verdict for the audit record.
func (h libraryAuditHook) AuditDecisionFor(final Decision) string {
	return decisionLabel(final.Kind)
}

// stmtValue reads the statement param a gated-allowlist keys on; a missing or
// non-string value yields the empty statement.
func stmtValue(params map[string]interface{}, stmtParam string) string {
	if stmtParam == "" {
		return ""
	}
	if v, ok := params[stmtParam]; ok {
		return valueToString(v)
	}
	return ""
}

// decisionLabel maps a decision kind to its audit label; a non-gating outcome
// has no label.
func decisionLabel(k DecisionKind) string {
	switch k {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	case Ask:
		return "ask"
	default:
		return ""
	}
}
