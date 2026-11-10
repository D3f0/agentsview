package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobSQLiteReportedCostOwnsBillingTokens(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "db", bobDBName)
	require.NoError(t, writeBobSQLiteFixture(path))
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
	require.Len(t, result.UsageEvents, 1)
	assert.Empty(t, result.Messages[1].TokenUsage, "reported task cost must not also price the message tokens")
	assert.Equal(t, 120, result.Messages[1].ContextTokens)
	assert.Equal(t, 40, result.Messages[1].OutputTokens)
	event := result.UsageEvents[0]
	assert.Equal(t, 100, event.InputTokens)
	assert.Equal(t, 40, event.OutputTokens)
	assert.Equal(t, 20, event.CacheReadInputTokens)
}
