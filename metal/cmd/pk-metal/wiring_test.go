package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
)

// runAsCLI is set in the child process that runCLI starts: the verbs exit
// the process on bad usage, so the test runs them in one of their own.
const runAsCLI = "PK_METAL_TEST_RUN_CLI"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(runAsCLI); ok {
		os.Args = append([]string{"pk-metal"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runCLI runs pk-metal with args in a child process and returns its exit
// code, stdout and stderr.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), runAsCLI+"="+strings.Join(args, " "))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), out.String(), errOut.String()
	}
	require.NoError(t, err)
	return 0, out.String(), errOut.String()
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
		{[]string{"relay", "extra"}, "relay takes no arguments"},
		{[]string{"sim"}, "name a subcommand: serve, fleet"},
		{[]string{"backup"}, "name a subcommand: now, list, get, decrypt"},
		{[]string{"rollout", "lsit"}, `unknown subcommand "lsit": give list, show, approve, resume, pause, abort`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			assert.Equal(t, 1, code, stderr)
			assert.Contains(t, stderr, tc.want)
			assert.Contains(t, stderr, "usage: pk-metal "+tc.args[0])
			assert.Contains(t, stderr, "run: pk-metal help "+tc.args[0])
		})
	}
}

// The help of a verb with subcommands comes from the command table, like the
// help of every other verb, and help <verb> <subcommand> shows the flags of
// the subcommand.
func TestHelpOfSubcommands(t *testing.T) {
	code, stdout, _ := runCLI(t, "sim", "-h")
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout, "usage: pk-metal sim serve [flags] | fleet --config <file>")
	code, stdout, _ = runCLI(t, "help", "backup", "get")
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout, "\n  -o string\n", "the flags of backup get, not of backup")
}

// The flags block names the value of a flag with one word, and shows no
// default that a required flag does not have.
func TestHelpOfFlags(t *testing.T) {
	_, stdout, _ := runCLI(t, "render", "-h")
	assert.Contains(t, stdout, "\n  -secrets file\n")
	_, stdout, _ = runCLI(t, "scale", "-h")
	assert.NotContains(t, stdout, "(default -1)")
	code, _, stderr := runCLI(t, "scale", "lab", "--workers", "1")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "give one cluster, --control-planes and --workers")
}

// pk-metal render writes what the day-0 path consumes: a machine config per
// node that Talos loads, and a kubeconfig and talosconfig that parse.
func TestRenderWritesLoadableConfigs(t *testing.T) {
	in := t.TempDir()
	sample, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "talos.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(in, "talos.yaml"), sample, 0o600))
	secrets := filepath.Join("..", "..", "internal", "talos", "render", "testdata", "secrets.yaml")
	out := filepath.Join(t.TempDir(), "out")

	// No --register: the schematic ids are computed offline.
	renderCmd([]string{"--dir", in, "--secrets", secrets, "--out", out})

	nodes, err := filepath.Glob(filepath.Join(out, "node-*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, nodes)
	for _, path := range nodes {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		cfg, err := configloader.NewFromBytes(raw)
		require.NoError(t, err, path)
		assert.NotEmpty(t, cfg.Machine().Type().String(), path)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "%s holds credentials", path)
	}

	kc, err := clientcmd.LoadFromFile(filepath.Join(out, "kubeconfig"))
	require.NoError(t, err)
	assert.NotEmpty(t, kc.CurrentContext)
	tc, err := os.ReadFile(filepath.Join(out, "talosconfig"))
	require.NoError(t, err)
	assert.Contains(t, string(tc), "context:")
}
