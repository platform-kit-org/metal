package main

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/cobratree/cobratreetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func noCluster() (client.Client, error) { return nil, errors.New("no cluster in this test") }

// runTree runs a command line on a new tree, as main does, and returns the
// exit status and what the program wrote to stderr. It runs in the test's
// process: a command line that reaches a command's work can exit it.
func runTree(t *testing.T, args ...string) (int, string) {
	t.Helper()
	root := newRoot(noCluster)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	code := run(root, append([]string{}, args...), &stderr)
	return code, stderr.String()
}

// TestUsageHasNoInternalJargon keeps the top-level help readable for someone
// who has not read the design documents.
func TestUsageHasNoInternalJargon(t *testing.T) {
	root := newRoot(noCluster)
	out, _, err := cobratreetest.Run(t, root, "--help")
	require.NoError(t, err)
	for _, word := range []string{"VAP", "ladder", "grant-serve", "L4", "reverse tunnel", "ADR"} {
		assert.NotContains(t, out, word)
	}
	for _, c := range root.Commands() {
		if c.IsAvailableCommand() {
			assert.Contains(t, out, "  "+c.Name()+" ", "every command is listed")
		}
	}
	assert.Contains(t, out, "REBOOT", "the boot line says it reboots the machine")
	assert.Contains(t, out, "exit status:")
}

// A wrong command line exits 1, says what is wrong, and names the help of
// the command it reached. pflag reads a long flag with one dash as
// shorthand flags: -yes is -y -e -s.
func TestWrongCommandLines(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		help string
	}{
		{[]string{"aprove"}, `unknown command "aprove" for "pk-metal" (did you mean "approve"?)`, "pk-metal"},
		{[]string{"rollout", "aprove", "x"}, `unknown command "aprove" for "pk-metal rollout"`, "pk-metal rollout"},
		{[]string{"approve", "--bogus"}, "unknown flag: --bogus", "pk-metal approve"},
		{[]string{"approve", "-yes", "x"}, "unknown shorthand flag: 'y' in -yes", "pk-metal approve"},
		{[]string{"status", "-o", "yaml"}, `invalid --output value "yaml": want table or json`, "pk-metal status"},
		{[]string{"reboot", "--drain-timeout", "x", "n"}, `invalid --drain-timeout value "x"`, "pk-metal reboot"},
		{[]string{"version", "x"}, `unknown command "x" for "pk-metal version"`, "pk-metal version"},
	} {
		code, stderr := runTree(t, tc.args...)
		assert.Equal(t, 1, code, tc.args)
		assert.Contains(t, stderr, "pk-metal: "+tc.want, tc.args)
		assert.Contains(t, stderr, "run: "+tc.help+" --help", tc.args)
	}
}

// TestSignalContext: Ctrl-C (SIGINT) ends a verb's context instead of the
// process, so that a wait on the cluster or on an answer ends cleanly.
func TestSignalContext(t *testing.T) {
	ctx, stop := signalContext()
	defer stop()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the context")
	}
}
