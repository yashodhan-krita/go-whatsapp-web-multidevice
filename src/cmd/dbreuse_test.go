package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDBReuseHelpDoesNotInitializeStorage(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, rc := runVersionHelper(t, dir, []string{
		"WHATSAPP_WEBHOOK_SECRET=synthetic-secret-should-not-print",
		"APP_BASIC_AUTH=synthetic-user:synthetic-pass",
	}, "db-reuse", "--help")

	require.Equal(t, 0, rc, "stderr: %s", stderr)
	assert.Contains(t, stdout, "snapshot")
	assert.Contains(t, stdout, "preflight")
	assert.NotContains(t, stdout+stderr, "synthetic-secret-should-not-print")
	assert.NotContains(t, stdout+stderr, "synthetic-pass")
	assert.NotContains(t, stdout, "map[")
	assert.Empty(t, dirEntries(t, dir))
}
