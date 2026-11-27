package main

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGlobal(t *testing.T) {
	global.kubeconfig, global.context = "", ""
	t.Cleanup(func() { global.kubeconfig, global.context = "", "" })

	rest, err := parseGlobal([]string{"--kubeconfig", "/tmp/kc", "--context", "lab", "status", "-o", "json"})
	require.NoError(t, err)
	assert.Equal(t, []string{"status", "-o", "json"}, rest)
	assert.Equal(t, "/tmp/kc", global.kubeconfig)
	assert.Equal(t, "lab", global.context)

	rest, err = parseGlobal([]string{"--help"})
	require.NoError(t, err)
	assert.Equal(t, []string{"help"}, rest)

	_, err = parseGlobal([]string{"--bogus"})
	assert.ErrorContains(t, err, "pk-metal help")
}

// TestUsageHasNoInternalJargon keeps the top-level help readable for someone
// who has not read the design documents.
func TestUsageHasNoInternalJargon(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	for _, word := range []string{"VAP", "ladder", "grant-serve", "L4", "reverse tunnel", "ADR"} {
		assert.NotContains(t, out.String(), word)
	}
	for _, c := range commands {
		assert.Contains(t, out.String(), "  "+c.name+" ", "every command is listed")
	}
	assert.Contains(t, out.String(), "REBOOT", "the boot line says it reboots the machine")
}

// TestParseFlagsAfterArguments: every verb takes its flags before or after
// its arguments, as the lab and the docs write them. pk-metal scale and
// replace refused "scale lab --control-planes 1": the flags after the
// cluster were taken for arguments.
func TestParseFlagsAfterArguments(t *testing.T) {
	for _, args := range [][]string{
		{"lab", "--control-planes", "1", "--workers", "2", "--apply", "--ack-etcd-risk", "lab scale round"},
		{"--control-planes=1", "lab", "--workers", "2", "--apply", "--ack-etcd-risk", "lab scale round"},
		{"--control-planes", "1", "--workers", "2", "--apply", "--ack-etcd-risk", "lab scale round", "lab"},
	} {
		fs := newFlags("scale")
		cps := fs.Int("control-planes", -1, "")
		workers := fs.Int("workers", -1, "")
		apply := fs.Bool("apply", false, "")
		ack := fs.String("ack-etcd-risk", "", "")
		parseFlags(fs, args)
		assert.Equal(t, []string{"lab"}, fs.Args(), args)
		assert.Equal(t, 1, *cps, args)
		assert.Equal(t, 2, *workers, args)
		assert.True(t, *apply, args)
		assert.Equal(t, "lab scale round", *ack, args)
	}
	fs := newFlags("replace")
	host := fs.String("host", "", "")
	parseFlags(fs, []string{"node-2", "--host", "pklab0005", "--", "--not-a-flag"})
	assert.Equal(t, "pklab0005", *host)
	assert.Equal(t, []string{"node-2", "--not-a-flag"}, fs.Args(), "after -- everything is an argument")
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

// Each verb that drives a machine or a cluster refuses a missing argument,
// says what is missing, and names the help that shows the rest.
func TestVerbsNameWhatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"boot"}, "--config, --node and one BMC URL are required"},
		{[]string{"boot", "--config=c.yaml", "--node=n"}, "--config, --node and one BMC URL are required"},
		{[]string{"bootstrap"}, "--dir and --secrets are required"},
		{[]string{"bootstrap", "--dir=d", "--secrets=s", "--endpoint=192.0.2.1"}, "--endpoint needs --node"},
		{[]string{"render"}, "--dir and --secrets are required"},
		{[]string{"render", "--dir=d", "--secrets=s", "extra"}, "render takes no other arguments"},
		{[]string{"render", "--dir=d", "--secrets=s", "--credentials-ttl=721h"}, "--credentials-ttl must be more than 0 and at most 720h"},
		{[]string{"render", "--dir=d", "--secrets=s", "--credentials-ttl=0s"}, "--credentials-ttl must be more than 0 and at most 720h"},
		{[]string{"relay", "extra"}, "relay takes no arguments"},
		// An argument reaches the verb as it is: with its space, and empty.
		{[]string{"completion", "korn shell"}, `unknown shell "korn shell"`},
		{[]string{"render", "--dir=d", "--secrets=s", ""}, "render takes no other arguments"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, stderr := runCLI(t, tc.args...)
			assert.Equal(t, 1, code, stderr)
			assert.Contains(t, stderr, tc.want)
			assert.Contains(t, stderr, "usage: pk-metal "+tc.args[0])
			assert.Contains(t, stderr, "run: pk-metal help "+tc.args[0])
		})
	}
}
