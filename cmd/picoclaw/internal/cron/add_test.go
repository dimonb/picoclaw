package cron

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgcron "github.com/sipeed/picoclaw/pkg/cron"
)

func TestNewAddSubcommand(t *testing.T) {
	fn := func() string { return "" }
	cmd := newAddCommand(fn)

	require.NotNil(t, cmd)

	assert.Equal(t, "add", cmd.Use)
	assert.Equal(t, "Add a new scheduled job", cmd.Short)

	assert.True(t, cmd.HasFlags())

	assert.NotNil(t, cmd.Flags().Lookup("every"))
	assert.NotNil(t, cmd.Flags().Lookup("cron"))
	assert.NotNil(t, cmd.Flags().Lookup("deliver"))
	assert.NotNil(t, cmd.Flags().Lookup("to"))
	assert.NotNil(t, cmd.Flags().Lookup("channel"))

	nameFlag := cmd.Flags().Lookup("name")
	require.NotNil(t, nameFlag)

	messageFlag := cmd.Flags().Lookup("message")
	require.NotNil(t, messageFlag)

	val, found := nameFlag.Annotations[cobra.BashCompOneRequiredFlag]
	require.True(t, found)
	require.NotEmpty(t, val)
	assert.Equal(t, "true", val[0])

	val, found = messageFlag.Annotations[cobra.BashCompOneRequiredFlag]
	require.True(t, found)
	require.NotEmpty(t, val)
	assert.Equal(t, "true", val[0])
}

func TestNewAddCommandEveryAndCronMutuallyExclusive(t *testing.T) {
	cmd := newAddCommand(func() string { return "testing" })

	cmd.SetArgs([]string{
		"--name", "job",
		"--message", "hello",
		"--every", "10",
		"--cron", "0 9 * * *",
	})

	err := cmd.Execute()
	require.Error(t, err)
}

func TestNewAddCommandMapsDeliverFlagToDirectMode(t *testing.T) {
	t.Parallel()

	storePath := filepath.Join(t.TempDir(), "jobs.json")
	cmd := newAddCommand(func() string { return storePath })
	cmd.SetArgs([]string{
		"--name", "job",
		"--message", "hello",
		"--every", "10",
		"--deliver",
		"--channel", "cli",
		"--to", "direct",
	})

	err := cmd.Execute()
	require.NoError(t, err)

	cs := pkgcron.NewCronService(storePath, nil)
	jobs := cs.ListJobs(false)
	require.Len(t, jobs, 1)
	assert.Equal(t, pkgcron.ModeDirect, jobs[0].Payload.Mode)
	assert.Equal(t, "cli", jobs[0].Payload.Channel)
	assert.Equal(t, "direct", jobs[0].Payload.To)
}
