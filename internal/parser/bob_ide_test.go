package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeBobIDETask(t *testing.T, root, id, api, ui, metadata string) string {
	t.Helper()
	dir := filepath.Join(root, "tasks", id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for name, content := range map[string]string{
		"api_conversation_history.json": api,
		"ui_messages.json":              ui,
		"task_metadata.json":            metadata,
	} {
		if content != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
		}
	}
	return filepath.Join(dir, "api_conversation_history.json")
}

// This exercises the native provider used by sync, not a parser-only seam.
// Requiring Roo's history_item.json or treating the UI as the whole transcript
// would either hide this source or lose the API-only environment/tool content.
func TestBobIDENativeProviderRetainsAPIAndSupplementalTools(t *testing.T) {
	root := t.TempDir()
	path := writeBobIDETask(t, root, "task-redacted", `[
		{"role":"user","ts":1700000000020,"content":[{"type":"text","text":"<task>Inspect fixture</task>"},{"type":"text","text":"<environment_details>\n# Current Workspace Directory (/redacted/workspace) Files\nhidden context\n</environment_details>"}]},
		{"role":"assistant","ts":1700000000200,"content":[{"type":"text","text":"Inspecting.\n<read_file><args><file><path>fixture.txt</path></file></args></read_file>"}]},
		{"role":"user","ts":1700000000210,"content":[{"type":"text","text":"[read_file for 'fixture.txt'] Result:"},{"type":"text","text":"<files><file path=\"fixture.txt\">retained result</file></files>"},{"type":"text","text":"<environment_details>hidden update</environment_details>"}]},
		{"role":"assistant","ts":1700000000400,"content":[{"type":"text","text":"<attempt_completion><result>Finished.</result></attempt_completion>"}]}
	]`, `[
		{"type":"say","say":"text","ts":1700000000000,"text":"Inspect fixture"},
		{"type":"say","say":"api_req_started","ts":1700000000010,"text":"{\"tokensIn\":10,\"tokensOut\":3,\"cacheReads\":2,\"cacheWrites\":1,\"cost\":0.001,\"apiProtocol\":\"anthropic\"}"},
		{"type":"say","say":"text","ts":1700000000100,"text":"Inspecting.","partial":false},
		{"type":"ask","ask":"tool","ts":1700000000150,"text":"{\"tool\":\"readFile\",\"path\":\"fixture.txt\",\"content\":\"retained result\"}","partial":false},
		{"type":"say","say":"api_req_started","ts":1700000000205,"text":"{\"tokensIn\":12,\"tokensOut\":4,\"cacheReads\":0,\"cacheWrites\":0,\"cost\":0.002}"},
		{"type":"say","say":"completion_result","ts":1700000000300,"text":"Finished.","partial":false}
	]`, `{"files_in_context":[]}`)
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}, Machine: "test-machine"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1, "a Bob IDE task is discoverable without Roo history_item.json")
	fp, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fp})
	require.NoError(t, err)
	require.True(t, out.ResultSetComplete)
	require.Len(t, out.Results, 1)
	result := out.Results[0].Result
	assert.Equal(t, "bob:ide:task-redacted", result.Session.ID)
	assert.Equal(t, "task-redacted", result.Session.SourceSessionID)
	assert.Equal(t, "test-machine", result.Session.Machine)
	assert.Equal(t, "/redacted/workspace", result.Session.Cwd)
	assert.Equal(t, path, result.Session.File.Path)
	assert.Equal(t, "Inspect fixture", result.Session.FirstMessage)
	assert.Equal(t, "Inspect fixture", result.Session.SessionName)
	assert.Equal(t, 1, result.Session.UserMessageCount)
	require.Len(t, result.Messages, 4, "UI mirrors must not duplicate API transcript rows")
	assert.Equal(t, []RoleType{RoleUser, RoleAssistant, RoleUser, RoleAssistant}, []RoleType{
		result.Messages[0].Role, result.Messages[1].Role, result.Messages[2].Role, result.Messages[3].Role,
	})
	assert.Equal(t, "<task>Inspect fixture</task>\n<environment_details>\n# Current Workspace Directory (/redacted/workspace) Files\nhidden context\n</environment_details>", result.Messages[0].Content)
	assert.Equal(t, "Inspecting.\n<read_file><args><file><path>fixture.txt</path></file></args></read_file>", result.Messages[1].Content)
	assert.Equal(t, "[read_file for 'fixture.txt'] Result:\n<files><file path=\"fixture.txt\">retained result</file></files>\n<environment_details>hidden update</environment_details>", result.Messages[2].Content)
	for i, want := range []string{"Inspect fixture", "Inspecting.", "", "Finished."} {
		require.NotNil(t, result.Messages[i].DialogueText)
		assert.Equal(t, want, *result.Messages[i].DialogueText)
	}
	assert.Equal(t, int64(1700000000200), result.Messages[1].Timestamp.UnixMilli())
	require.Len(t, result.Messages[1].ToolCalls, 1)
	call := result.Messages[1].ToolCalls[0]
	assert.Equal(t, "read_file", call.ToolName)
	require.NotEmpty(t, call.ResultEvents)
	assert.Contains(t, call.ResultEvents[len(call.ResultEvents)-1].Content, "retained result")
	require.Len(t, result.UsageEvents, 2)
	assert.Equal(t, 10, result.UsageEvents[0].InputTokens)
	assert.Equal(t, 3, result.UsageEvents[0].OutputTokens)
	assert.Equal(t, 2, result.UsageEvents[0].CacheReadInputTokens)
	assert.Equal(t, 1, result.UsageEvents[0].CacheCreationInputTokens)
	assert.Equal(t, 7, result.Session.TotalOutputTokens)
	assert.Equal(t, 13, result.Session.PeakContextTokens)
}

func TestBobIDENativeProviderRetainsTasksWithoutMetadata(t *testing.T) {
	root := t.TempDir()
	writeBobIDETask(t, root, "no-metadata", `[{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"Original task"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"redacted"}}]}]`, `[{"type":"say","say":"text","ts":1699999999990,"text":"Original task"}]`, "")
	provider, ok := NewProvider(AgentBob, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	require.Len(t, out.Results[0].Result.Messages, 1)
	assert.Equal(t, "Original task\n[image]", out.Results[0].Result.Messages[0].Content)
	assert.Empty(t, out.Results[0].Result.Session.Cwd, "context-file paths are not workspace metadata")
}

func TestBobIDESourceSetTracksAllCompanions(t *testing.T) {
	root := t.TempDir()
	path := writeBobIDETask(t, root, "fingerprint-task", `[{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"Task"}]}]`, `[]`, "")
	sources := newBobIDESourceSet([]string{root})
	refs, err := sources.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, refs, 1)
	before, err := sources.Fingerprint(t.Context(), refs[0])
	require.NoError(t, err)
	metadataPath := filepath.Join(filepath.Dir(path), "task_metadata.json")
	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"files_in_context":[]}`), 0o600))
	after, err := sources.Fingerprint(t.Context(), refs[0])
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash)
	changed, err := sources.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: metadataPath})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].DisplayPath)
	found, ok, err := sources.FindSource(t.Context(), FindSourceRequest{RawSessionID: "ide:fingerprint-task"})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, path, found.DisplayPath)
	direct := newBobIDESourceSet([]string{filepath.Join(root, "tasks")})
	directRefs, err := direct.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, directRefs, 1)
	assert.Equal(t, path, directRefs[0].DisplayPath)
}

func TestBobIDESourceSetRejectsMalformedAndIncompleteTasks(t *testing.T) {
	for _, fixture := range []struct {
		name string
		api  string
		ui   string
		kind string
	}{
		{"bad-api", `[{`, `[]`, "malformed"},
		{"bad-ui", `[]`, `{`, "malformed"},
		{"missing-api", "", `[{"type":"say","say":"text","ts":1700000000000,"text":"Task"}]`, "incomplete"},
		{"missing-role", `[{"ts":1700000000000,"content":[]}]`, `[]`, "malformed"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := t.TempDir()
			writeBobIDETask(t, root, fixture.name, fixture.api, fixture.ui, "")
			sources := newBobIDESourceSet([]string{root})
			refs, err := sources.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, refs, 1, "incomplete tasks must not become authoritative empty discovery")
			_, err = sources.Parse(t.Context(), ParseRequest{Source: refs[0]})
			var sourceErr *bobIDESourceError
			require.ErrorAs(t, err, &sourceErr)
			assert.Equal(t, fixture.kind, sourceErr.Kind)
		})
	}
}

func TestBobIDEDialogueExcludesProtocolWithoutDiscardingSourceRows(t *testing.T) {
	root := t.TempDir()
	path := writeBobIDETask(t, root, "projection", `[
		{"role":"system","ts":1700000000000,"content":[{"type":"text","text":"System instructions"}]},
		{"role":"user","ts":1700000000010,"content":[{"type":"text","text":"<task>Explain literal [Tool: Read] markers</task>"},{"type":"text","text":"<environment_details>workspace context</environment_details>"}]},
		{"role":"assistant","ts":1700000000020,"content":[{"type":"thinking","thinking":"Private analysis"},{"type":"text","text":"Checking first.\n<execute_command><command>printf '<tag>'</command></execute_command>\nThen I will explain."}]},
		{"role":"user","ts":1700000000030,"content":[{"type":"text","text":"[execute_command] Result:\n<tag>\n<environment_details>context update</environment_details>"}]},
		{"role":"tool","ts":1700000000040,"content":[{"type":"text","text":"Native tool result"}]},
		{"role":"assistant","ts":1700000000050,"content":[{"type":"text","text":"<attempt_completion><result>Literal [Thinking] is ordinary text here.</result></attempt_completion>"}]}
	]`, `[]`, "")
	_, messages, err := parseBobIDESession(t.Context(), filepath.Dir(path), "", "local")
	require.NoError(t, err)
	require.Len(t, messages, 6)
	for i, want := range []string{"", "Explain literal [Tool: Read] markers", "Checking first.\n\nThen I will explain.", "", "", "Literal [Thinking] is ordinary text here."} {
		require.NotNil(t, messages[i].DialogueText)
		assert.Equal(t, want, *messages[i].DialogueText)
	}
	assert.Equal(t, RoleSystem, messages[0].Role)
	assert.Equal(t, RoleTool, messages[4].Role)
	assert.Equal(t, "Native tool result", messages[4].Content)
	assert.Contains(t, messages[2].Content, "<execute_command>")
	assert.Equal(t, "Private analysis", messages[2].ThinkingText)
	assert.True(t, messages[2].HasThinking)
}

func TestBobIDEUIDialogueDistinguishesSupplementalConversationAndDiagnostics(t *testing.T) {
	root := t.TempDir()
	path := writeBobIDETask(t, root, "supplemental", `[]`, `[
		{"type":"say","say":"text","ts":1700000000000,"text":"Task"},
		{"type":"say","say":"reasoning","ts":1700000000010,"text":"Private analysis"},
		{"type":"say","say":"text","ts":1700000000020,"text":"Supplemental answer"},
		{"type":"say","say":"command_output","ts":1700000000030,"text":"Unpaired result"},
		{"type":"say","say":"error","ts":1700000000040,"text":"Diagnostic"},
		{"type":"ask","ask":"followup","ts":1700000000050,"text":"{\"question\":\"Which file?\",\"suggest\":[]}"}
	]`, "")
	_, messages, err := parseBobIDESession(t.Context(), filepath.Dir(path), "", "local")
	require.NoError(t, err)
	require.Len(t, messages, 6)
	for i, want := range []string{"Task", "", "Supplemental answer", "", "", "Which file?"} {
		require.NotNil(t, messages[i].DialogueText)
		assert.Equal(t, want, *messages[i].DialogueText)
	}
	assert.Equal(t, "[Thinking]\nPrivate analysis\n[/Thinking]", messages[1].Content)
	assert.Equal(t, "Unpaired result", messages[3].Content)
	assert.Equal(t, RoleSystem, messages[3].Role)
}

func TestBobIDERequestTokensSurviveUnpricedSupplementalCost(t *testing.T) {
	for _, cost := range []string{`"not-a-price"`, `-1`, `{"amount":1}`, `1e100`} {
		t.Run(cost, func(t *testing.T) {
			root := t.TempDir()
			path := writeBobIDETask(t, root, "usage", `[]`, `[{"type":"say","say":"api_req_started","ts":1700000000000,"text":"{\"tokensIn\":10,\"tokensOut\":3,\"cacheReads\":2,\"cacheWrites\":1,\"cost\":`+strings.ReplaceAll(cost, `"`, `\"`)+`}"}]`, "")
			sess, _, err := parseBobIDESession(t.Context(), filepath.Dir(path), "", "local")
			require.NoError(t, err)
			require.Len(t, sess.UsageEvents, 1)
			assert.Nil(t, sess.UsageEvents[0].Cost)
			assert.Equal(t, 10, sess.UsageEvents[0].InputTokens)
			assert.Equal(t, 3, sess.TotalOutputTokens)
			assert.Equal(t, 13, sess.PeakContextTokens)
		})
	}
}

func TestBobIDESourceIOPreservesCancellation(t *testing.T) {
	root := t.TempDir()
	path := writeBobIDETask(t, root, "canceled", `[]`, `[]`, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := parseBobIDESession(ctx, filepath.Dir(path), "", "local")
	assert.ErrorIs(t, err, context.Canceled)
	_, err = bobIDEFingerprint(ctx, path)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBobIDEUnmirroredCompletionRetainsUserFacingDialogue(t *testing.T) {
	root := t.TempDir()
	path := writeBobIDETask(t, root, "completion", `[]`, `[
		{"type":"say","say":"text","ts":1700000000000,"text":"Task"},
		{"type":"say","say":"completion_result","ts":1700000000010,"text":"Finished the task."}
	]`, "")
	_, messages, err := parseBobIDESession(t.Context(), filepath.Dir(path), "", "local")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, RoleAssistant, messages[1].Role)
	assert.Equal(t, new("Finished the task."), messages[1].DialogueText)
	assert.Equal(t, "Finished the task.", messages[1].Content)
}
