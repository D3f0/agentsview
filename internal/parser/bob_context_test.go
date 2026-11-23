package parser

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBobCLISourceIOPreservesCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "demo", "chats", "session-canceled.json")
	writeSourceFile(t, path, `{"sessionId":"source-task","messages":[{"type":"user","content":"Question"}]}`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := parseBobSession(ctx, path, "local")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBobSQLiteSourceIOPreservesCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "db", bobDBName)
	require.NoError(t, writeBobSQLiteFixture(t.Context(), path))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	src := multiSessionSource{Root: root, Path: bobDBVirtualPath(path, "task-1"), Container: path, MemberID: "task-1"}
	_, err := bobDBParseMember(ctx, src, ParseRequest{})
	assert.ErrorIs(t, err, context.Canceled)
	_, err = bobDBParseContainer(ctx, src, ParseRequest{})
	assert.ErrorIs(t, err, context.Canceled)
	_, err = bobDBFingerprint(ctx, src)
	assert.ErrorIs(t, err, context.Canceled)
	err = bobDBDiscoverEach(ctx, root, func(multiSessionMatch) error {
		t.Error("a canceled discovery must not publish source rows")
		return nil
	})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBobSQLiteStreamingDiscoveryStopsAtConsumerError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "db", bobDBName)
	require.NoError(t, writeBobSQLiteFixture(t.Context(), path))
	stop := errors.New("consumer stopped")
	var discovered []string
	err := bobDBDiscoverEach(t.Context(), root, func(match multiSessionMatch) error {
		discovered = append(discovered, match.Path)
		return stop
	})
	assert.ErrorIs(t, err, stop)
	assert.Equal(t, []string{bobDBVirtualPath(path, "task-1")}, discovered)
}
