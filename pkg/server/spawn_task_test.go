// Copyright © 2026 Teradata Corporation - All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package server

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	loomv1 "github.com/teradata-labs/loom/gen/go/loom/v1"
	"github.com/teradata-labs/loom/pkg/agent"
	llmtypes "github.com/teradata-labs/loom/pkg/llm/types"
	"github.com/teradata-labs/loom/pkg/shuttle"
	"github.com/teradata-labs/loom/pkg/shuttle/builtin"
)

// spawnTestServer builds a server whose registry can instantiate the named
// agent, so SpawnSubAgent runs the real path.
func spawnTestServer(t *testing.T, llm *replyingLLM) *MultiAgentServer {
	t.Helper()

	logger := zaptest.NewLogger(t)
	registry, err := agent.NewRegistry(agent.RegistryConfig{
		ConfigDir:   t.TempDir(),
		DBPath:      ":memory:",
		Logger:      logger,
		LLMProvider: llm,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = registry.Close() })

	registry.RegisterConfig(&loomv1.AgentConfig{
		Name:        "helper",
		Description: "spawnable helper",
	})

	srv := setupBroadcastTestServer(t, map[string]*agent.Agent{}, registry)
	return srv
}

// parentSession creates the parent session a spawn hangs off, which the store
// requires before a child session can reference it.
func parentSession(t *testing.T, srv *MultiAgentServer) string {
	t.Helper()
	id := GenerateSessionID()
	require.NoError(t, srv.sessionStore.SaveSession(context.Background(), &agent.Session{
		ID:      id,
		AgentID: "parent",
	}))
	// A spawn starts background goroutines that outlive the request by design;
	// tear them down with the test so they cannot outlive its logger.
	t.Cleanup(func() { srv.cleanupSpawnedAgentsByParent(id) })
	return id
}

// TestSpawn_WithTaskReturnsTheAnswer proves the spawn contract the response
// struct has always declared: a spawn carrying a task runs it and hands the
// sub-agent's answer back as this call's result, so the parent reads the answer
// where it called and no reply has to find its way home.
func TestSpawn_WithTaskReturnsTheAnswer(t *testing.T) {
	llm := &replyingLLM{}
	srv := spawnTestServer(t, llm)

	resp, err := srv.SpawnSubAgent(context.Background(), &builtin.SpawnSubAgentRequest{
		ParentSessionID: parentSession(t, srv),
		ParentAgentID:   "parent",
		AgentID:         "helper",
		InitialMessage:  "check these queries",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "completed", resp.Status,
		"a spawn carrying a task reports completion, not just creation")
	assert.NotEmpty(t, resp.Output, "the sub-agent's answer is the call's result")
	assert.NotEmpty(t, resp.SubAgentID)
	assert.Greater(t, resp.DurationMs, int64(-1), "the run is timed")
}

// TestSpawn_WithoutTaskKeepsCreateOnlyBehaviour proves the back-compatible
// half: with no task there is nothing to run, so the call creates the agent and
// returns as it always did.
func TestSpawn_WithoutTaskKeepsCreateOnlyBehaviour(t *testing.T) {
	llm := &replyingLLM{}
	srv := spawnTestServer(t, llm)

	resp, err := srv.SpawnSubAgent(context.Background(), &builtin.SpawnSubAgentRequest{
		ParentSessionID: parentSession(t, srv),
		ParentAgentID:   "parent",
		AgentID:         "helper",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "spawned", resp.Status)
	assert.Empty(t, resp.Output, "no task means no answer to report")
}

// TestSpawn_CarriesNoTeamBlock proves a plain spawned sub-agent is told nothing
// about messaging: it receives a task as a call and returns an answer as that
// call's result, so it has no peers to address.
func TestSpawn_CarriesNoTeamBlock(t *testing.T) {
	llm := &replyingLLM{}
	srv := spawnTestServer(t, llm)

	_, err := srv.SpawnSubAgent(context.Background(), &builtin.SpawnSubAgentRequest{
		ParentSessionID: parentSession(t, srv),
		ParentAgentID:   "parent",
		AgentID:         "helper",
		InitialMessage:  "do the thing",
	})
	require.NoError(t, err)

	assert.NotContains(t, llm.prompt(), "WORKFLOW COMMUNICATION",
		"a spawned sub-agent with no subscriptions carries no team block")
}

// failingLLM fails every turn, standing in for a sub-agent whose task blows up.
type failingLLM struct{}

func (failingLLM) Chat(ctx context.Context, messages []llmtypes.Message, tools []shuttle.Tool) (*llmtypes.LLMResponse, error) {
	return nil, errors.New("sub-agent exploded")
}
func (failingLLM) Name() string  { return "failing-llm" }
func (failingLLM) Model() string { return "failing-model" }

// TestSpawn_FailedTaskCleansUp proves the failure path: a task that errors
// surfaces as the spawn call's error, and the half-built sub-agent is torn down
// rather than left tracked — the parent gets a failure, not a phantom child.
func TestSpawn_FailedTaskCleansUp(t *testing.T) {
	logger := zaptest.NewLogger(t)
	registry, err := agent.NewRegistry(agent.RegistryConfig{
		ConfigDir:   t.TempDir(),
		DBPath:      ":memory:",
		Logger:      logger,
		LLMProvider: failingLLM{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = registry.Close() })
	registry.RegisterConfig(&loomv1.AgentConfig{Name: "helper", Description: "spawnable helper"})

	srv := setupBroadcastTestServer(t, map[string]*agent.Agent{}, registry)
	parent := parentSession(t, srv)

	resp, err := srv.SpawnSubAgent(context.Background(), &builtin.SpawnSubAgentRequest{
		ParentSessionID: parent,
		ParentAgentID:   "parent",
		AgentID:         "helper",
		InitialMessage:  "do the thing",
	})

	require.Error(t, err, "a failed task must surface as the spawn call's error")
	assert.Nil(t, resp)
	assert.Zero(t, srv.countSpawnedAgentsByParent(parent),
		"the half-built sub-agent must not stay tracked after a failed task")
}
