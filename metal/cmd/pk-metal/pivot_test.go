package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/pivot"
)

func TestConfirmPivot(t *testing.T) {
	plan := &pivot.Plan{Reachable: []string{"the BMCs, over HTTPS, from every node of mgmt: 192.0.2.101:443"}}

	ctx := context.Background()
	var out bytes.Buffer
	asked := func(in io.Reader, interactive, yes bool) prompt {
		return prompt{Yes: yes, Interactive: interactive, In: in, Out: &out}
	}
	require.NoError(t, confirmPivot(ctx, asked(strings.NewReader("y\n"), true, false), plan, "kind", "mgmt"))
	assert.Contains(t, out.String(), "192.0.2.101:443")
	assert.Contains(t, out.String(), "Move? [y/N]")

	out.Reset()
	withStore := *plan
	withStore.Claims = []pivot.ClaimMove{{Volume: "media-store", Name: "pk-metal-media-store", Action: pivot.ClaimEphemeral}}
	require.NoError(t, confirmPivot(ctx, asked(nil, false, true), &withStore, "kind", "mgmt"))
	assert.Contains(t, out.String(), "The volumes the operator mounts, on mgmt:")
	assert.Contains(t, out.String(), "PersistentVolumeClaim pk-metal-media-store is replaced by an emptyDir")

	out.Reset()
	require.ErrorIs(t, confirmPivot(ctx, asked(strings.NewReader("\n"), true, false), plan, "kind", "mgmt"), declined("moved"))

	err := confirmPivot(ctx, asked(nil, false, false), plan, "kind", "mgmt")
	require.ErrorContains(t, err, "--yes", "without a terminal it refuses to ask")

	out.Reset()
	require.NoError(t, confirmPivot(ctx, asked(nil, false, true), plan, "kind", "mgmt"))
	assert.NotContains(t, out.String(), "Move?")

	out.Reset()
	require.NoError(t, confirmPivot(ctx, asked(nil, false, false), &pivot.Plan{Resume: true}, "kind", "mgmt"))
	assert.Contains(t, out.String(), "checking it again", "a resumed move only verifies")

	// Ctrl-C at the question ends it at once, as at every other question.
	// The question read its answer without the context: Ctrl-C did nothing
	// until Enter.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	never, w := io.Pipe()
	defer w.Close()
	require.ErrorIs(t, confirmPivot(cancelled, asked(never, true, false), plan, "kind", "mgmt"), errInterrupted)
}

func TestPivotDoneSaysHowToGoBack(t *testing.T) {
	text := pivotDone(&pivot.Report{Rewritten: 3}, "kind-pk-metal", "admin@mgmt", "mgmt.kubeconfig")
	assert.Contains(t, text, "3 references")
	assert.Contains(t, text, "pk-metal --kubeconfig mgmt.kubeconfig pivot --to <kubeconfig of kind-pk-metal>")
	assert.Contains(t, text, "Do not start it again")
}

func TestRestConfigNamesTheContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- name: mgmt
  cluster: {server: "https://192.0.2.10:6443"}
users:
- name: admin
  user: {token: synthetic}
contexts:
- name: admin@mgmt
  context: {cluster: mgmt, user: admin}
- name: other
  context: {cluster: mgmt, user: admin}
current-context: admin@mgmt
`), 0o600))
	cfg, name, err := namedRestConfig(path, "")
	require.NoError(t, err)
	assert.Equal(t, "admin@mgmt", name)
	assert.Equal(t, "https://192.0.2.10:6443", cfg.Host)
	_, name, err = namedRestConfig(path, "other")
	require.NoError(t, err)
	assert.Equal(t, "other", name)
	_, _, err = namedRestConfig(filepath.Join(t.TempDir(), "missing"), "")
	require.Error(t, err)
	// Without --kubeconfig, the message names no empty path.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	_, _, err = namedRestConfig("", "")
	require.ErrorContains(t, err, "cannot load a kubeconfig: ")
}
