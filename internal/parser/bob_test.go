package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobProviderDiscoverAndParse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "project-hash", "chats", "session-2026-07-28T10-31-6d542bf6.json")
	writeSourceFile(t, path, `{
  "sessionId": "6d542bf6-1111-2222-3333-444444444444",
  "projectHash": "project-hash",
  "startTime": "2026-07-28T10:31:00.000Z",
  "lastUpdated": "2026-07-28T10:32:00.000Z",
  "messages": [
    {"id":"u1","type":"user","content":"Fix the parser","timestamp":"2026-07-28T10:31:01.000Z"},
    {"id":"a1","type":"bob-shell","content":"<thinking>Inspect the format.</thinking>\nI will fix it.","timestamp":"2026-07-28T10:31:02.000Z","model":"premium","thoughts":[{"description":"Inspect the format."}],"tokens":{"input":100,"output":20,"cached":30,"thoughts":5},"toolCalls":[{"id":"tool-1","name":"read_file","args":{"file_path":"internal/parser/bob.go"},"status":"success","timestamp":"2026-07-28T10:31:03.000Z","resultDisplay":"file contents"}]}
  ]
}`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}, Machine: "devbox"})
	require.True(t, ok)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, path, sources[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{RawSessionID: filepath.Base(path[:len(path)-len(filepath.Ext(path))])})
	require.NoError(t, err)
	require.True(t, ok)
	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: found, Fingerprint: fingerprint})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	assert.Equal(t, "bob:session-2026-07-28T10-31-6d542bf6", sess.ID)
	assert.Equal(t, "project-hash", sess.Project)
	assert.Equal(t, "Fix the parser", sess.FirstMessage)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasThinking)
	assert.Equal(t, "Inspect the format.", msgs[1].ThinkingText)
	require.NotNil(t, msgs[1].DialogueText)
	assert.Equal(t, "I will fix it.", *msgs[1].DialogueText)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read_file", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, 100+30, msgs[1].ContextTokens)
	assert.Equal(t, 20, msgs[1].OutputTokens)
	assert.Equal(t, "premium", msgs[1].Model)
	assert.NotEmpty(t, msgs[1].TokenUsage)
	assert.Equal(t, fingerprint.Hash, sess.File.Hash)
}

func TestBobProviderIgnoresNonSessionJSON(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "db", "bob.db"), "not a session")
	writeSourceFile(t, filepath.Join(root, "tmp", "project", "logs.json"), "{}")
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestBobProviderMissingSourceReturnsError(t *testing.T) {
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{t.TempDir()}})
	require.True(t, ok)
	_, err := provider.Parse(context.Background(), ParseRequest{Source: SourceRef{
		Provider:       AgentBob,
		Key:            "/missing/session.json",
		DisplayPath:    "/missing/session.json",
		FingerprintKey: "/missing/session.json",
		Opaque:         JSONLSource{Path: "/missing/session.json"},
	}})
	assert.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
