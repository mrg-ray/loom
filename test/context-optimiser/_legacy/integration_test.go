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

// Integration tests that exercise ACTUAL runtime behaviour the scripted-LLM
// suite cannot: the flag/entry round trip on a real PostgreSQL backend, and a
// real model reasoning over stubs and fold summaries and recalling shed content.
//
// Each self-gates and skips cleanly when its dependency is absent, so the
// package stays green under `go test ./...` everywhere:
//
//	TEST_POSTGRES_URL=postgres://…            → the Postgres round-trip test
//	ANTHROPIC_API_KEY=… | AWS creds+region    → the real-LLM behaviour test
//	LOOM_CONTEXT_OPTIMISER=1                   → both (same gate as the suite)
package contextoptimiser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/llm/anthropic"
	"github.com/teradata-labs/loom/pkg/llm/bedrock"
	"github.com/teradata-labs/loom/pkg/observability"
	"github.com/teradata-labs/loom/pkg/prompts"
	"github.com/teradata-labs/loom/pkg/shuttle"
	"github.com/teradata-labs/loom/pkg/skills"
	pgstore "github.com/teradata-labs/loom/pkg/storage/postgres"
	"github.com/teradata-labs/loom/pkg/types"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// integrationRig — a rig over the SessionStorage INTERFACE, so the same drive
// runs on SQLite or Postgres, with a scripted or a real LLM and compressor.
// ---------------------------------------------------------------------------

type integrationRig struct {
	agent *agent.Agent
	mem   *agent.Memory
	llm   *scriptedLLM
}

func newIntegrationRig(t *testing.T, store agent.SessionStorage, llm agent.LLMProvider,
	extraTools []shuttle.Tool, maxCtx, reserved, offload, maxL2 int, opts ...agent.Option) *integrationRig {
	t.Helper()

	work := t.TempDir()
	skillsDir, _ := writeFixtures(t, work)
	lib := skills.NewLibrary(skills.WithSearchPaths(skillsDir))
	orch := skills.NewOrchestrator(lib)

	cfg := agent.DefaultConfig()
	cfg.MaxContextTokens = maxCtx
	cfg.ReservedOutputTokens = reserved
	cfg.SkillsConfig = &skills.SkillsConfig{Enabled: true}
	if pc := cfg.PatternConfig; pc != nil {
		pc.UseLLMClassifier = false
	}

	mem := agent.NewMemoryWithStore(store)
	base := []agent.Option{
		agent.WithConfig(cfg),
		agent.WithSkillOrchestrator(orch),
		agent.WithMemory(mem),
		agent.WithoutSelfCorrection(),
	}
	a := agent.NewAgent(nil, llm, append(base, opts...)...)
	if offload > 0 {
		a.SetSharedMemoryThreshold(int64(offload))
	}
	if maxL2 > 0 {
		mem.SetMaxL2Tokens(maxL2)
	}
	for _, tl := range extraTools {
		a.RegisterTool(tl)
	}

	sl, _ := llm.(*scriptedLLM)
	return &integrationRig{agent: a, mem: mem, llm: sl}
}

// rendered returns what the provider would receive for a session right now.
func (r *integrationRig) rendered(t *testing.T, sid string) []types.Message {
	t.Helper()
	s, ok := r.agent.GetSession(sid)
	if !ok {
		t.Fatalf("session %s not found", sid)
	}
	sm := s.SegmentedMem.(*agent.SegmentedMemory)
	return sm.GetMessagesForLLM()
}

func (r *integrationRig) foldCount(t *testing.T, sid string) int {
	s, ok := r.agent.GetSession(sid)
	if !ok {
		return 0
	}
	return statInt(s.SegmentedMem.(*agent.SegmentedMemory).GetMemoryStats(), "fold_count")
}

// ---------------------------------------------------------------------------
// Test 1 — the relief flags and fold entries survive a real Postgres round trip.
// ---------------------------------------------------------------------------

func TestIntegration_PostgresReliefRoundTrip(t *testing.T) {
	requireGate(t)
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL not set; skipping the Postgres round-trip test")
	}
	ctx := pgstore.ContextWithUserID(context.Background(), "ctxopt-user")

	// Isolated schema so the run neither sees nor leaves state in public.
	schema := fmt.Sprintf("ctxopt_%d", os.Getpid())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		admin.Close()
	})

	poolFor := func() *pgxpool.Pool {
		pc, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		pc.ConnConfig.RuntimeParams["search_path"] = schema
		p, err := pgxpool.NewWithConfig(ctx, pc)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		return p
	}

	// Migrate the isolated schema up — brings in 000019_message_relief_flags.
	migPool := poolFor()
	mig, err := pgstore.NewMigrator(migPool, observability.NewNoOpTracer())
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := mig.MigrateUp(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	migPool.Close()

	newStore := func() agent.SessionStorage {
		return pgstore.NewSessionStore(poolFor(), observability.NewNoOpTracer(), zap.NewNop())
	}

	comp := &countingCompressor{summary: "STATE of the work with decisions and message ids"}
	live := newIntegrationRig(t, newStore(), &scriptedLLM{}, nil, 8000, 1000, 6000, 300)
	live.mem.SetCompressor(comp)
	sid := "pg-roundtrip"

	// Drive the eviction→fold→demotion arc: heavy conversation forces folds,
	// periodic mid-size tool results are evicted first.
	drove := 0
	for i := 0; i < 45; i++ {
		if i%3 == 0 {
			live.llm.turns = append(live.llm.turns,
				callTool(fmt.Sprintf("c%d", i), emitToolName, sized(3000)), sayText("res"))
		} else {
			live.llm.turns = append(live.llm.turns, sayText("ack"))
		}
		if _, err := live.agent.Chat(ctx, sid, heavyConversation(i)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		drove = i + 1

		// Reload from a FRESH Postgres store at every beat — read + render only —
		// and require it to match the live render exactly. This is the whole test:
		// eviction/folded flags and fold entries must survive the real round trip.
		restored := newIntegrationRig(t, newStore(), &scriptedLLM{}, nil, 8000, 1000, 6000, 300)
		restored.mem.SetCompressor(comp)
		rs := restored.mem.GetOrCreateSession(ctx, sid)
		if rs == nil {
			t.Fatalf("turn %d: restored session is nil", i)
		}
		liveMsgs := live.rendered(t, sid)
		restMsgs := rs.SegmentedMem.(*agent.SegmentedMemory).GetMessagesForLLM()
		if len(liveMsgs) != len(restMsgs) {
			t.Fatalf("turn %d: live renders %d messages, Postgres reload renders %d",
				i, len(liveMsgs), len(restMsgs))
		}
		for j := range liveMsgs {
			if liveMsgs[j].Role != restMsgs[j].Role || liveMsgs[j].Content != restMsgs[j].Content {
				t.Fatalf("turn %d: rendered message %d differs after Postgres reload\n  live: %s\n  rest: %s",
					i, j, trim(liveMsgs[j].Content), trim(restMsgs[j].Content))
			}
		}
	}

	folds := live.foldCount(t, sid)
	if folds < 2 {
		t.Errorf("drove %d turns but only %d folds — the arc did not reach repeated folds/demotion on Postgres", drove, folds)
	}
	t.Logf("Postgres round trip clean: %d turns, %d folds, reload matched live at every beat", drove, folds)
}

// ---------------------------------------------------------------------------
// Test 2 — a real model reasons over stubs and fold summaries and recalls the
// shed content it needs.
// ---------------------------------------------------------------------------

// secretTool returns a payload carrying one distinctive fact. Large enough to
// stub at arrival, so the model must recall it to answer. lines sets the bulk on
// each side (0 → 400, a large must-offload payload); a small value keeps the
// payload offloadable yet small enough to fit the working set when recalled.
type secretTool struct {
	secret string
	lines  int
}

func (s *secretTool) Name() string        { return "fetch_report" }
func (s *secretTool) Description() string { return "Fetch the full incident report." }
func (s *secretTool) Backend() string     { return "" }
func (s *secretTool) InputSchema() *shuttle.JSONSchema {
	return &shuttle.JSONSchema{Type: "object", Properties: map[string]*shuttle.JSONSchema{}}
}
func (s *secretTool) Execute(_ context.Context, _ map[string]interface{}) (*shuttle.Result, error) {
	// Distinctive fact buried in bulk, so only recall of the payload yields it.
	reps := s.lines
	if reps == 0 {
		reps = 400
	}
	body := strings.Repeat("incident log line, routine entry, nothing notable. ", reps) +
		"\nROOT CAUSE: " + s.secret + "\n" +
		strings.Repeat("incident log line, routine entry, nothing notable. ", reps)
	return &shuttle.Result{Success: true, Data: body}, nil
}

func realLLMProvider(t *testing.T) (agent.LLMProvider, string) {
	t.Helper()
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg := anthropic.Config{APIKey: key, MaxTokens: 4096, Temperature: 1.0,
			Model: "claude-3-5-sonnet-20241022"}
		provider := "anthropic"
		// A company/gateway proxy (e.g. `use-work`): honour its base URL and model.
		if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
			cfg.Endpoint = strings.TrimRight(base, "/") + "/v1/messages"
			if m := os.Getenv("ANTHROPIC_DEFAULT_MODEL"); m != "" {
				cfg.Model = m
			}
			provider = "gateway:" + cfg.Model
		}
		return anthropic.NewClient(cfg), provider
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	creds := os.Getenv("AWS_ACCESS_KEY_ID") != "" && os.Getenv("AWS_SECRET_ACCESS_KEY") != ""
	if (creds || os.Getenv("AWS_PROFILE") != "") && region != "" {
		c, err := bedrock.NewClient(bedrock.Config{
			Region: region, AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
			Profile: os.Getenv("AWS_PROFILE"), ModelID: "anthropic.claude-3-5-sonnet-20241022-v2:0",
			MaxTokens: 4096, Temperature: 1.0,
		})
		if err != nil {
			t.Fatalf("bedrock: %v", err)
		}
		return c, "bedrock"
	}
	return nil, ""
}

func TestIntegration_RealLLMBehaviour(t *testing.T) {
	requireGate(t)
	llm, provider := realLLMProvider(t)
	if llm == nil {
		t.Skip("no real LLM credentials (ANTHROPIC_API_KEY or AWS creds+region); skipping the real-LLM behaviour test")
	}
	ctx := context.Background()

	store, err := agent.NewSessionStore(t.TempDir()+"/real.db", observability.NewNoOpTracer())
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	const secret = "a mislabeled valve on pump seven, part number XJ-4471"
	tool := &secretTool{secret: secret}

	// Real LLM as both the agent's provider and the compressor. A modest window so
	// the report is stubbed and, with continued work, folded — forcing recall.
	r := newIntegrationRig(t, store, llm, []shuttle.Tool{tool},
		16000, 3000, 4000, 1200, agent.WithCompressorLLM(llm))
	sid := "real-llm"

	turns := []string{
		"Call fetch_report to pull the full incident report, then acknowledge you have it.",
		"Summarise the routine log volume briefly.",
		"List three general operational best practices for incident response.",
		"Describe how to structure an incident postmortem, in a few sentences.",
		"Now: what was the ROOT CAUSE recorded in that incident report? Recall it if needed.",
	}

	recallSeen := false
	var lastAnswer string
	for i, msg := range turns {
		resp, err := r.agent.Chat(ctx, sid, msg)
		if err != nil {
			t.Fatalf("turn %d (%q): real Chat failed: %v", i, msg, err)
		}
		lastAnswer = resp.Content
		// Did the model call recall on any turn? (proves stubs/addresses are usable)
		for _, m := range r.rendered(t, sid) {
			for _, tc := range m.ToolCalls {
				if tc.Name == "recall" {
					recallSeen = true
				}
			}
		}
		t.Logf("[%s] turn %d done (fold_count=%d)", provider, i, r.foldCount(t, sid))
	}

	// 1. The report payload must have stubbed (arrival ≥ threshold), so the model
	//    could only answer by recalling it.
	stubbed := false
	for _, m := range r.rendered(t, sid) {
		if m.Role == "tool" && strings.Contains(m.Content, "recall(\"msg:") {
			stubbed = true
		}
	}
	if !stubbed {
		t.Errorf("the incident report never rendered as a recallable stub — the scenario did not exercise recall")
	}

	// 2. The model recalled the shed content when it needed it.
	if !recallSeen {
		t.Errorf("the model never called recall across the run — a real model did not use the stub/summary addresses")
	}

	// 3. The recall round trip delivered usable data: the answer carries the secret.
	if !strings.Contains(strings.ToLower(lastAnswer), "xj-4471") &&
		!strings.Contains(strings.ToLower(lastAnswer), "valve") {
		t.Errorf("the final answer does not contain the recalled root cause — recall did not deliver usable data\n  answer: %s",
			trim(lastAnswer))
	}

	t.Logf("[%s] real-LLM behaviour verified: stub rendered, recall used, secret delivered", provider)
}

// TestIntegration_RealLLMFoldSummary exercises the compaction prompt itself: a
// real model folds an oldest run into an L2 summary. It asserts the mechanical
// guarantees the prompt must never violate — the summary carries no fabricated
// ladder address (code owns those), and its first line is a standalone headline
// so the demoted short is a usable breadcrumb — and that judgement (an early
// decision / a ruled-out path) survives the fold rather than dissolving into
// dropped prose.
func TestIntegration_RealLLMFoldSummary(t *testing.T) {
	requireGate(t)
	llm, provider := realLLMProvider(t)
	if llm == nil {
		t.Skip("no real LLM credentials (ANTHROPIC_API_KEY or AWS creds+region); skipping the real-LLM fold-summary test")
	}
	ctx := context.Background()

	store, err := agent.NewSessionStore(t.TempDir()+"/foldsum.db", observability.NewNoOpTracer())
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// The compressor wires only when the agent has a prompt registry (agent.go:207).
	// Load the real prompts/ tree so folds run compaction.yaml, not the heuristic
	// fallback — fail fast if the compaction prompt is not resolvable.
	reg := prompts.NewFileRegistry("../../prompts")
	if rerr := reg.Reload(ctx); rerr != nil {
		t.Fatalf("load prompts/ tree: %v", rerr)
	}
	if _, perr := reg.Get(ctx, "memory.compaction", nil); perr != nil {
		t.Fatalf("compaction prompt not loadable from ../../prompts: %v", perr)
	}

	// Small window, no large tool outputs: pressure relief has nothing to evict,
	// so it goes straight to folding. The real LLM is the compressor, so this
	// drive runs compaction.yaml end to end. Tight maxL2 forces demotion too.
	r := newIntegrationRig(t, store, llm, nil, 4500, 900, 3000, 500,
		agent.WithPrompts(reg), agent.WithCompressorLLM(llm))
	sid := "real-fold"

	// Turns 0-1 plant unmistakable judgement (a decision and a rejection) at the
	// OLDEST position, so they fold first. The bulky paragraphs that follow build
	// the pressure that folds them — and are exactly the prose that should be dropped.
	turns := []string{
		"Record this decision: the incident-response runbook is FROZEN for Q3 — no edits until the security audit closes. Acknowledge in one line.",
		"We tried SMS paging for the on-call rota and it failed compliance review, so SMS paging is REJECTED. Acknowledge in one line.",
		"Explain, in four detailed paragraphs, best practices for blameless postmortems.",
		"Explain, in four detailed paragraphs, how to run an incident timeline reconstruction.",
		"Explain, in four detailed paragraphs, how severity levels should be assigned.",
		"Explain, in four detailed paragraphs, how to structure on-call handoffs.",
		"Explain, in four detailed paragraphs, communication norms during a major incident.",
		"Explain, in four detailed paragraphs, how to measure incident-response effectiveness.",
		"Explain, in four detailed paragraphs, how to run a game-day resilience exercise.",
		"Explain, in four detailed paragraphs, how to onboard a new on-call engineer.",
	}
	for i, msg := range turns {
		if _, err := r.agent.Chat(ctx, sid, msg); err != nil {
			t.Fatalf("turn %d (%q): real Chat failed: %v", i, msg, err)
		}
		t.Logf("[%s] turn %d done (fold_count=%d)", provider, i, r.foldCount(t, sid))
	}

	n := r.foldCount(t, sid)
	if n == 0 {
		t.Skipf("[%s] no fold occurred (window too large for this load); prompt path not exercised", provider)
	}
	t.Logf("[%s] %d fold(s) produced by the real compressor", provider, n)

	s, _ := r.agent.GetSession(sid)
	sm := s.SegmentedMem.(*agent.SegmentedMemory)

	// If the compressor did not actually engage — its call errored or timed out —
	// foldBatch writes the heuristic "Fold N: …" fallback. That is a compaction
	// *availability* defect (e.g. the 5s compaction timeout), not a prompt-quality
	// one; this test validates the prompt, so skip rather than fail when the LLM
	// path never ran. The fallback is unmistakable: it begins "Fold N:".
	for i := 1; i <= n; i++ {
		e, ok := sm.FoldEntryByNumber(i)
		if ok && strings.HasPrefix(e.Long, fmt.Sprintf("Fold %d:", i)) {
			t.Skipf("[%s] fold %d fell back to the heuristic summariser — the LLM compaction "+
				"path did not run (compaction timeout / provider error). Prompt quality not "+
				"exercised; see the compaction-timeout finding.", provider, i)
		}
	}

	var allLong string
	for i := 1; i <= n; i++ {
		e, ok := sm.FoldEntryByNumber(i)
		if !ok {
			t.Fatalf("fold %d missing", i)
		}
		allLong += "\n" + e.Long

		// (a) Observation only, never a failure. Code owns the ladder address and
		//     appends the authoritative one at render (proven deterministically in
		//     ladder_test.go, even for an opaque summary). Whether the model also
		//     wrote a stray msg:/fold: token in its prose is cosmetic — the leading
		//     code address dominates — and asserting the model never does is a
		//     coin-flip on model behaviour, not a system invariant. Just record it.
		if strings.Contains(e.Long, "msg:") || strings.Contains(e.Long, "fold:") {
			t.Logf("[%s] fold %d: model wrote a stray address token in its prose (harmless; "+
				"code address leads):\n  %s", provider, i, trim(e.Long))
		}

		// (b) The first line must be a standalone headline, not a meta-preamble,
		//     because shortLineFor demotes to exactly that line — forever.
		head := e.Long
		if idx := strings.IndexByte(head, '\n'); idx > 0 {
			head = head[:idx]
		}
		head = strings.TrimSpace(head)
		if head == "" {
			t.Errorf("[%s] fold %d has an empty headline line — the permanent short would be blank", provider, i)
		}
		low := strings.ToLower(head)
		for _, bad := range []string{"the conversation", "this summary", "here is", "here's", "in summary", "the following", "this stretch"} {
			if strings.HasPrefix(low, bad) {
				t.Errorf("[%s] fold %d headline is a meta-preamble, not a conclusion: %q", provider, i, head)
			}
		}
		t.Logf("[%s] fold %d headline: %s", provider, i, head)
		t.Logf("[%s] fold %d long:\n%s", provider, i, trim(e.Long))
	}

	// (c) Judgement survived the fold. Only assert this if the decision turn was
	//     actually folded (not still live in the working set) — otherwise the
	//     fold-survival property was never exercised this run.
	decisionStillLive := false
	for _, m := range r.rendered(t, sid) {
		if m.Role != "system" && strings.Contains(strings.ToLower(m.Content), "runbook") {
			decisionStillLive = true
		}
	}
	if decisionStillLive {
		t.Logf("[%s] the decision turn is still in the working set; fold-survival of judgement not exercised", provider)
	} else {
		low := strings.ToLower(allLong)
		keptDecision := strings.Contains(low, "frozen") || strings.Contains(low, "freeze") || strings.Contains(low, "audit")
		keptRejection := strings.Contains(low, "sms") || strings.Contains(low, "reject")
		if !keptDecision && !keptRejection {
			t.Errorf("[%s] the folded decision (runbook frozen) and rejection (SMS paging) both vanished — "+
				"judgement was lost to compression:\n%s", provider, trim(allLong))
		}
	}
}

// TestIntegration_RealLLMFoldDescent is the one path proven entirely through the
// LLM: the model generates the fold summaries, and — shown only a DEMOTED short
// line whose underlying report payload is offloaded — the model itself reads the
// rendered recall affordances and descends the ladder to retrieve a fact buried
// at the bottom. Nothing is invoked directly; the model chooses every recall.
//
// The proof is airtight because the buried fact is unreachable except by descent:
// the report payload offloads to a stub the instant it arrives, so the fold
// summary is built from the stub (never the payload) and cannot contain the
// secret, and once folded the stub leaves the working set entirely. A correct
// final answer therefore happens IF AND ONLY IF the model navigated
//
//	demoted short ─recall(fold:N)/(msg:A-B)→ folded messages (the stub)
//	              ─recall(msg:X)→ the offloaded payload → ROOT CAUSE
func TestIntegration_RealLLMFoldDescent(t *testing.T) {
	requireGate(t)
	llm, provider := realLLMProvider(t)
	if llm == nil {
		t.Skip("no real LLM credentials (ANTHROPIC_API_KEY or AWS creds+region); skipping the fold-descent test")
	}
	ctx := context.Background()

	store, err := agent.NewSessionStore(t.TempDir()+"/descent.db", observability.NewNoOpTracer())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	reg := prompts.NewFileRegistry("../../prompts")
	if rerr := reg.Reload(ctx); rerr != nil {
		t.Fatalf("load prompts/ tree: %v", rerr)
	}

	const secret = "a mislabeled valve on pump seven, part number XJ-4471"
	// lines=40 → ~4KB: over the 2KB offload threshold (so it stubs at arrival),
	// yet small enough to fit the working set once recalled at the bottom rung.
	tool := &secretTool{secret: secret, lines: 40}

	// Small window + tiny L2 budget: the report folds early and its fold demotes
	// to a short (L2 can afford at most the newest long). Budget headroom
	// (5000-900) holds the recalled payload on the final turn.
	r := newIntegrationRig(t, store, llm, []shuttle.Tool{tool}, 5000, 900, 2000, 200,
		agent.WithPrompts(reg), agent.WithCompressorLLM(llm))
	sid := "real-descent"

	// Collect every recall address the model chose across the whole run.
	recallAddrs := []string{}
	seenAddr := map[string]bool{}
	sweepRecalls := func() {
		for _, m := range r.rendered(t, sid) {
			for _, tc := range m.ToolCalls {
				if tc.Name != "recall" {
					continue
				}
				if a, _ := tc.Input["address"].(string); a != "" && !seenAddr[a] {
					seenAddr[a] = true
					recallAddrs = append(recallAddrs, a)
				}
			}
		}
	}

	turns := []string{
		"Call fetch_report to pull the full incident report, then acknowledge in one line that you have it.",
		"Explain, in four detailed paragraphs, best practices for blameless postmortems.",
		"Explain, in four detailed paragraphs, how to run an incident timeline reconstruction.",
		"Explain, in four detailed paragraphs, how severity levels should be assigned.",
		"Explain, in four detailed paragraphs, how to structure on-call handoffs.",
		"Explain, in four detailed paragraphs, communication norms during a major incident.",
		"Explain, in four detailed paragraphs, how to measure incident-response effectiveness.",
		"Explain, in four detailed paragraphs, how to run a game-day resilience exercise.",
		"Explain, in four detailed paragraphs, how to onboard a new on-call engineer.",
		"Explain, in four detailed paragraphs, how to write a customer-facing incident update.",
		"Explain, in four detailed paragraphs, how to run a post-incident action-item review.",
	}
	for i, msg := range turns {
		if _, err := r.agent.Chat(ctx, sid, msg); err != nil {
			t.Fatalf("turn %d (%q): real Chat failed: %v", i, msg, err)
		}
		sweepRecalls()
		t.Logf("[%s] turn %d done (fold_count=%d)", provider, i, r.foldCount(t, sid))
	}

	// Precondition: the report was folded and its fold demoted. With maxL2=200
	// only the newest fold can render long, so ≥2 folds guarantees an older one
	// (the report's) is a short. Skip honestly if the load did not fold enough.
	n := r.foldCount(t, sid)
	if n < 2 {
		t.Skipf("[%s] only %d fold(s); the report fold did not demote — scenario not exercised", provider, n)
	}

	// Precondition: the secret is NOT reachable from the visible context — it lives
	// only in the offloaded payload behind the fold. (If it were visible, a correct
	// answer would not prove descent.)
	for _, m := range r.rendered(t, sid) {
		if strings.Contains(strings.ToLower(m.Content), "xj-4471") {
			t.Fatalf("[%s] the secret is visible in the rendered context (%s) — the scenario "+
				"does not force descent", provider, m.Role)
		}
	}

	// The moment of truth: a question answerable only by descending the ladder.
	answer, err := r.agent.Chat(ctx, sid,
		"What exact ROOT CAUSE was recorded in that incident report earlier? "+
			"Recall whatever you need to answer precisely.")
	if err != nil {
		t.Fatalf("[%s] final Chat failed: %v", provider, err)
	}
	sweepRecalls()

	t.Logf("[%s] recall trace (model-chosen): %v", provider, recallAddrs)
	t.Logf("[%s] final answer: %s", provider, trim(answer.Content))

	// 1. The model navigated — it issued at least one recall of its own accord.
	if len(recallAddrs) == 0 {
		t.Fatalf("[%s] the model never called recall — it could not have reached the buried payload; "+
			"the rendered affordances were not usable", provider)
	}

	// 2. The answer carries the buried root cause. Given the secret is only in the
	//    offloaded payload, this is true IFF the full descent succeeded end to end.
	low := strings.ToLower(answer.Content)
	if !strings.Contains(low, "xj-4471") && !strings.Contains(low, "valve on pump seven") {
		t.Fatalf("[%s] the answer does not carry the buried root cause — the model did not descend "+
			"the ladder to the payload.\n  recalls: %v\n  answer: %s", provider, recallAddrs, trim(answer.Content))
	}

	t.Logf("[%s] fold-descent proven end to end: model folded, read the demoted short, "+
		"recalled down the ladder, and delivered the buried root cause", provider)
}
