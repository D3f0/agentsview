package config

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// Ignoring [agents.bob], appending default roots, or omitting the IDE source set
// must fail through the same configured provider that local sync consumes.
func TestBobReleaseAgentTableDiscoversCLIIDEAndSQLiteWithoutPG(t *testing.T) {
	fixture := newConfigFixture(t)
	home := canonicalTempDir(t)
	setTestHome(t, home)
	for _, key := range []string{"BOB_DIR", "AGENTSVIEW_PG_URL", "AGENTSVIEW_PG_SCHEMA", "AGENTSVIEW_PG_MACHINE", "AGENTSVIEW_PG_ALLOW_INSECURE"} {
		t.Setenv(key, "")
	}
	fixture.WriteConfigText(t, `[agents.bob]
dirs = ["~/cli-history", "~/ide-history", "~/sqlite-history"]
`)
	cliRoot := filepath.Join(home, "cli-history")
	ideRoot := filepath.Join(home, "ide-history")
	sqliteRoot := filepath.Join(home, "sqlite-history")
	cliPath := filepath.Join(cliRoot, "tmp", "demo", "chats", "session-configured.json")
	idePath := filepath.Join(ideRoot, "tasks", "configured-ide", "api_conversation_history.json")
	defaultPath := filepath.Join(home, ".bob", "tmp", "demo", "chats", "session-default.json")
	for path, body := range map[string]string{
		cliPath: `{"sessionId":"configured-cli-task","projectHash":"demo","messages":[{"id":"cli-user","type":"user","content":"Configured CLI question","timestamp":"2026-01-01T00:00:01Z"},{"id":"cli-answer","type":"bob-shell","content":"Configured CLI answer","timestamp":"2026-01-01T00:00:03Z"}]}`,
		idePath: `[{"role":"user","ts":1767225601000,"content":[{"type":"text","text":"Configured IDE question"}]},{"role":"assistant","ts":1767225603000,"content":[{"type":"text","text":"Configured IDE answer"}]}]`,
		filepath.Join(filepath.Dir(idePath), "ui_messages.json"): `[]`,
		defaultPath: `{"sessionId":"default-task","messages":[{"type":"user","content":"Default root must not leak into configured imports"}]}`,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	producerPath := filepath.Join(sqliteRoot, "db", "bob.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(producerPath), 0o700))
	producer, err := sql.Open("sqlite3", producerPath)
	require.NoError(t, err)
	_, err = producer.ExecContext(t.Context(), `
		CREATE TABLE tasks (id TEXT PRIMARY KEY, project_id TEXT, directory TEXT,
			title TEXT, env TEXT, costs TEXT, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE messages (id TEXT PRIMARY KEY, task_id TEXT, role TEXT,
			data TEXT, created_at INTEGER);
		INSERT INTO tasks VALUES ('configured-sqlite','demo','/workspace/demo','Configured SQLite',
			'{}','',1767225600000,1767225603000);
		INSERT INTO messages VALUES ('user','configured-sqlite','user',
			'{"role":"user","content":"Configured SQLite question"}',1767225601000);
		INSERT INTO messages VALUES ('assistant','configured-sqlite','assistant',
			'{"role":"assistant","content":"Configured SQLite answer"}',1767225603000);
	`)
	require.NoError(t, err)
	require.NoError(t, producer.Close())

	cfg, err := LoadMinimal()
	require.NoError(t, err)
	assert.Equal(t, []string{cliRoot, ideRoot, sqliteRoot}, cfg.ResolveDirs(parser.AgentBob))
	assert.True(t, cfg.IsUserConfigured(parser.AgentBob))
	pg, err := cfg.ResolvePG()
	require.NoError(t, err)
	assert.Empty(t, pg.URL, "local Bob source loading must not require a PostgreSQL target")
	assert.Empty(t, cfg.PGTargets)

	var provider parser.Provider
	for _, factory := range cfg.LocalProviderFactories() {
		if factory.Definition().Type == parser.AgentBob {
			// These are loaded consumer roots, not reconstructed defaults or a
			// test-owned copy of the TOML values.
			provider = factory.NewProvider(parser.ProviderConfig{
				Roots: cfg.AgentDirs[parser.AgentBob], Machine: "fixture-machine",
			})
			break
		}
	}
	require.NotNil(t, provider)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 3)
	paths := make([]string, 0, len(sources))
	results := make(map[string]parser.ParseResult)
	for _, source := range sources {
		paths = append(paths, source.DisplayPath)
		fingerprint, err := provider.Fingerprint(t.Context(), source)
		require.NoError(t, err)
		outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source, Fingerprint: fingerprint})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		result := outcome.Results[0].Result
		results[result.Session.ID] = result
	}
	assert.ElementsMatch(t, []string{cliPath, idePath, producerPath + "#configured-sqlite"}, paths)
	for _, expected := range []struct{ id, sourceID, path, question, answer string }{
		{"bob:session-configured", "configured-cli-task", cliPath, "Configured CLI question", "Configured CLI answer"},
		{"bob:ide:configured-ide", "configured-ide", idePath, "Configured IDE question", "Configured IDE answer"},
		{"bob:configured-sqlite", "configured-sqlite", producerPath + "#configured-sqlite", "Configured SQLite question", "Configured SQLite answer"},
	} {
		result, ok := results[expected.id]
		require.True(t, ok, "configured source must yield session %s", expected.id)
		assert.Equal(t, expected.sourceID, result.Session.SourceSessionID)
		assert.Equal(t, expected.path, result.Session.File.Path)
		assert.Equal(t, "fixture-machine", result.Session.Machine)
		require.Len(t, result.Messages, 2)
		assert.Equal(t, parser.RoleUser, result.Messages[0].Role)
		assert.Equal(t, expected.question, result.Messages[0].Content)
		assert.Equal(t, parser.RoleAssistant, result.Messages[1].Role)
		assert.Equal(t, expected.answer, result.Messages[1].Content)
	}
}
