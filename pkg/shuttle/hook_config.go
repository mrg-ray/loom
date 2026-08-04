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

// HooksConfig is the `tools.hooks` block Viper unmarshals: an ordered list of
// library-policy bindings. It lives in package shuttle (not cmd/looms) so a
// library builder can construct an admission chain without importing main.
type HooksConfig struct {
	Bindings []HookBinding `mapstructure:"hooks"`
}

// HookBinding declares one library policy purely from config: which policy
// (Kind), the tools it governs (Scope), the params it selects on (Matcher), and
// the policy-specific parameters. A binding is turned into a live Hook by
// BuildChainFromConfig; a malformed binding fails serve startup (fail-closed).
type HookBinding struct {
	// Kind selects the policy: "gated-allowlist" | "denylist" | "audit" | "custom".
	Kind string `mapstructure:"kind"`
	// Scope is the tool selector: an exact tool name or a "<prefix>*" pattern,
	// with the same match semantics as tools.permissions.
	Scope string `mapstructure:"scope"`
	// Matcher selects calls by their params, including a dispatch tool's
	// carried/nested op addressed by path.
	Matcher MatcherSpec `mapstructure:"matcher"`
	// StateKey names the approved-set partition a gated-allowlist reads.
	StateKey string `mapstructure:"state_key"`
	// ReadPattern admits a read-only call with no approved-set entry (gated-allowlist).
	ReadPattern string `mapstructure:"read_pattern"`
	// StmtParam names the param holding the statement whose identity is checked
	// (gated-allowlist); shared with the render/record side's canonicalization.
	StmtParam string `mapstructure:"stmt_param"`
	// Pattern is the denylist match expression.
	Pattern string `mapstructure:"pattern"`
	// SourceTool names the render/record tool a gated-allowlist trusts as the
	// source of approved-set entries.
	SourceTool string `mapstructure:"source_tool"`
	// ResultPath locates the rendered statements within a render Result.
	ResultPath string `mapstructure:"result_path"`
	// Name is the registry key of a custom hook.
	Name string `mapstructure:"name"`
}
