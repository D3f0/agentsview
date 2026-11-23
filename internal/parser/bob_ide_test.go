package parser

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing API turns or importing both companion files separately breaks this
// transcript contract. UI text deliberately differs from the full API answer.
func TestBobIDETranscriptAndCompanionIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tasks", "ide-task", "api_conversation_history.json")
	uiPath := filepath.Join(root, "tasks", "ide-task", "ui_messages.json")
	writeSourceFile(t, path, `[
 {"role":"user","ts":1000,"content":[{"type":"text","text":"Inspect the code"}]},
 {"role":"assistant","ts":2000,"content":[{"type":"text","text":"Full answer\n<read_file><path>main.go</path></read_file>"}]},
 {"role":"user","ts":3000,"content":[{"type":"text","text":"File result: package main"}]},
 {"role":"assistant","ts":4000,"content":[{"type":"text","text":"Finished"}]}
]`)
	writeSourceFile(t, uiPath, `[
 {"ts":950,"type":"say","say":"text","text":"Inspect the code"},
 {"ts":1100,"type":"say","say":"api_req_started","text":"{\"tokensIn\":100,\"tokensOut\":20,\"cacheReads\":30,\"cacheWrites\":10,\"cost\":0.25}"},
 {"ts":1800,"type":"say","say":"text","text":"Short UI answer"},
 {"ts":1900,"type":"ask","ask":"tool","text":"{\"tool\":\"readFile\",\"path\":\"main.go\",\"content\":\"package main\"}"},
 {"ts":3900,"type":"say","say":"text","text":"Finished"}
]`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, path, sources[0].DisplayPath)
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: uiPath, WatchRoot: root})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sources[0].Key, changed[0].Key)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{FullSessionID: "bob:ide:ide-task"})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sources[0].Key, found.Key)
	fp, err := provider.Fingerprint(t.Context(), found)
	require.NoError(t, err)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: found, Fingerprint: fp})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	r := out.Results[0].Result
	assert.Equal(t, "bob:ide:ide-task", r.Session.ID)
	assert.Equal(t, "ide-task", r.Session.SourceSessionID)
	assert.Equal(t, "Inspect the code", r.Session.FirstMessage)
	assert.Equal(t, time.UnixMilli(1000).UTC(), r.Session.StartedAt)
	assert.Equal(t, time.UnixMilli(4000).UTC(), r.Session.EndedAt)
	require.Len(t, r.Messages, 4)
	assert.Equal(t, "Full answer\n<read_file><path>main.go</path></read_file>", r.Messages[1].Content)
	assert.Equal(t, "File result: package main", r.Messages[2].Content)
	require.Len(t, r.Messages[1].ToolCalls, 1)
	assert.Equal(t, "readFile", r.Messages[1].ToolCalls[0].ToolName)
	require.Len(t, r.Messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "package main", r.Messages[1].ToolCalls[0].ResultEvents[0].Content)
	require.Len(t, r.UsageEvents, 1)
	assert.Equal(t, 100, r.UsageEvents[0].InputTokens)
	assert.Equal(t, 20, r.UsageEvents[0].OutputTokens)
	assert.Equal(t, 30, r.UsageEvents[0].CacheReadInputTokens)
	require.NotNil(t, r.UsageEvents[0].Cost)
	assert.Equal(t, int64(250000), r.UsageEvents[0].Cost.Microdollars)
	assert.Equal(t, 140, r.Session.PeakContextTokens)
	assert.Equal(t, fp.Hash, r.Session.File.Hash)
	writeSourceFile(t, uiPath, `[]`)
	newFP, err := provider.Fingerprint(t.Context(), found)
	require.NoError(t, err)
	assert.NotEqual(t, fp.Hash, newFP.Hash)
}

func TestBobIDENativeToolBlocks(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "tasks", "native", "api_conversation_history.json"), `[
 {"role":"user","ts":1000,"content":"Read a file"},
 {"role":"assistant","ts":2000,"content":[{"type":"text","text":"Reading"},{"type":"tool_use","id":"call-1","name":"read_file","input":{"path":"main.go"}}]},
 {"role":"user","ts":3000,"content":[{"type":"tool_result","tool_use_id":"call-1","content":"package main"}]}
]`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	messages := out.Results[0].Result.Messages
	require.Len(t, messages, 3)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "call-1", messages[1].ToolCalls[0].ToolUseID)
	require.Len(t, messages[2].ToolResults, 1)
	assert.Equal(t, "call-1", messages[2].ToolResults[0].ToolUseID)
	assert.Equal(t, `"package main"`, messages[2].ToolResults[0].ContentRaw)
}

func TestBobIDEExcludesMetadataAndUnconfiguredRoots(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "tasks", "_metadata", "api_conversation_history.json"), `[]`)
	writeSourceFile(t, filepath.Join(root, "tasks", "task", "task_metadata.json"), `{}`)
	writeSourceFile(t, filepath.Join(root, "unrelated", "api_conversation_history.json"), `[]`)
	other := t.TempDir()
	writeSourceFile(t, filepath.Join(other, "tasks", "outside", "api_conversation_history.json"), `[]`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestBobIDEKeepsImageOnlyPrompts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tasks", "image-task", "api_conversation_history.json")
	writeSourceFile(t, path, `[
 {"role":"user","ts":1000,"content":[{"type":"text","text":"Describe this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]},
 {"role":"user","ts":2000,"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}
 ]`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	result := out.Results[0].Result
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "Describe this\n[image]", result.Messages[0].Content)
	assert.Equal(t, "[image]", result.Messages[1].Content)
	assert.Equal(t, 2, result.Session.UserMessageCount)
}
