package parser

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobSQLiteReportedContextAndUsageOnlyResponse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "db", bobDBName)
	require.NoError(t, writeBobSQLiteFixture(path))
	writer, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, writer.Close()) })
	_, err = writer.ExecContext(t.Context(), `UPDATE tasks SET costs = '{"contextTokens":450,"cost":0}'`)
	require.NoError(t, err)
	_, err = writer.ExecContext(t.Context(), `UPDATE messages SET data = '{"id":"native-response","role":"assistant","content":"","_meta":{"spend":{"input":90,"output":12}}}' WHERE role = 'assistant'`)
	require.NoError(t, err)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	r := out.Results[0].Result
	assert.Equal(t, 450, r.Session.PeakContextTokens)
	require.Len(t, r.Messages, 2)
	assert.True(t, r.Messages[1].IsSystem)
	assert.Equal(t, "native-response", r.Messages[1].SourceUUID)
	assert.Equal(t, 12, r.Messages[1].OutputTokens)
	require.Len(t, r.UsageEvents, 1)
	require.NotNil(t, r.UsageEvents[0].Cost)
	assert.Zero(t, r.UsageEvents[0].Cost.Microdollars)
	assert.Zero(t, r.UsageEvents[0].InputTokens, "task totals must not duplicate message tokens")
}

func TestBobParseHonorsCanceledContext(t *testing.T) {
	root := t.TempDir()
	writeSourceFile(t, filepath.Join(root, "tmp", "project", "chats", "session-cancel.json"), `{"messages":[{"type":"user","content":"Hello"}]}`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = provider.Parse(ctx, ParseRequest{Source: sources[0]})
	assert.ErrorIs(t, err, context.Canceled)
}
