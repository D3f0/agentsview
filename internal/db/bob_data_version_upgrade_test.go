package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// A fork could mark these providers complete at 169 without having applied the
// upstream 168/169 corrections. Version 186 must invalidate all four providers,
// including rows already promoted to 185, without rewriting unaffected history.
func TestBob186UpgradeInvalidatesForkCollisionProviders(t *testing.T) {
	for _, previousVersion := range []int{169, 185} {
		t.Run(strconv.Itoa(previousVersion), func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			for _, agent := range []string{"bob", "antigravity", "antigravity-cli", "deepseek-harness", "gptme"} {
				insertSession(t, d, agent, "fixture", func(session *Session) { session.Agent = agent })
				require.NoError(t, d.SetSessionDataVersion(ctx, agent, previousVersion))
			}
			insertSession(t, d, "unaffected-retry", "fixture", func(session *Session) { session.Agent = "gptme" })
			require.NoError(t, d.SetSessionDataVersion(ctx, "unaffected-retry", previousVersion-1))
			skips := map[string]int64{
				"bob?agent=bob": 1, "agy?agent=antigravity": 1,
				"agy-cli?agent=antigravity-cli": 1, "harness?agent=deepseek-harness": 1,
				"other?agent=gptme": 1,
			}
			require.NoError(t, d.ReplaceSkippedFiles(ctx, skips))
			failures := make(map[string]SourceFailure)
			for key := range skips {
				failures[key] = SourceFailure{MTimeNS: 1}
			}
			require.NoError(t, d.ReplaceSourceFailures(ctx, failures))
			require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", previousVersion))
				return err
			}))
			require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error { return advanceDataVersion(ctx, tx, 186) }))
			for _, agent := range []string{"bob", "antigravity", "antigravity-cli", "deepseek-harness"} {
				assert.Equal(t, previousVersion, d.GetSessionDataVersion(ctx, agent), "%s needs reparsing", agent)
			}
			assert.Equal(t, 186, d.GetSessionDataVersion(ctx, "gptme"))
			assert.Equal(t, previousVersion-1, d.GetSessionDataVersion(ctx, "unaffected-retry"), "older retry markers stay stale")
			gotSkips, err := d.LoadSkippedFiles(ctx)
			require.NoError(t, err)
			assert.Equal(t, map[string]int64{"other?agent=gptme": 1}, gotSkips)
			gotFailures, err := d.LoadSourceFailures(ctx)
			require.NoError(t, err)
			assert.Equal(t, map[string]SourceFailure{"other?agent=gptme": {MTimeNS: 1}}, gotFailures)
			var pending, version int
			require.NoError(t, d.Reader().QueryRow(ctx, "SELECT value FROM stats WHERE key = 'scoped_upgrade_pending'").Scan(&pending))
			assert.Equal(t, 186, pending)
			require.NoError(t, d.Reader().QueryRow(ctx, "PRAGMA user_version").Scan(&version))
			assert.Equal(t, 186, version)
		})
	}
}

func TestBob186ScopePreservesUpstreamCollisionDeclarations(t *testing.T) {
	assert.ElementsMatch(t, []parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI}, ScopedDataVersionProviders(167, 168))
	assert.Equal(t, []parser.AgentType{parser.AgentDeepSeekHarness}, ScopedDataVersionProviders(168, 169))
	assert.ElementsMatch(t, []parser.AgentType{parser.AgentBob, parser.AgentAntigravity, parser.AgentAntigravityCLI, parser.AgentDeepSeekHarness}, ScopedDataVersionProviders(185, 186))
}
