package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// hostile is text that a BMC, a cluster object or the fleet can hold: a
// colour, a write to the clipboard (OSC 52), and a carriage return that
// writes over the start of the line.
const hostile = "\x1b[31mred\x1b]52;c;aGlp\x07clip\rover"

// assertEscaped fails when out holds a character that acts on the terminal,
// or does not show the hostile text, escaped.
func assertEscaped(t *testing.T, out string) {
	t.Helper()
	for _, c := range []string{"\x1b", "\x07", "\r"} {
		assert.NotContains(t, out, c, "the output holds a control character")
	}
	assert.Contains(t, out, termtext.Escape(hostile), "the output shows the text, escaped")
}

// pk-metal check prints what the BMC reports of itself, escaped.
func TestCheckEscapesTheBMCText(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	f.Vendor, f.Manager.Firmware, f.Systems[0].Serial = hostile, hostile, hostile
	srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()
	c, err := gofishclient.Connect(t.Context(), gofishclient.Config{Endpoint: srv.URL,
		Username: f.Accounts[0].Username, Password: f.Accounts[0].Password, ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	var out bytes.Buffer
	require.NoError(t, check(t.Context(), c, &out))
	assertEscaped(t, out.String())
	assert.Contains(t, out.String(), "check:    OK (read-only)")
}

// The error that ends pk-metal is escaped: it can quote a BMC or a cluster.
func TestDieEscapesTheError(t *testing.T) {
	code, stderr := runCLI(t, "--\x1b]52;c;aGlp\x07")
	assert.Equal(t, 1, code)
	assert.NotContains(t, stderr, "\x1b")
	assert.NotContains(t, stderr, "\x07")
	assert.Contains(t, stderr, `pk-metal: flag provided but not defined: -\x1b]52;c;aGlp\x07`)
}

// Each log line is escaped: boot logs the media and system ids and the
// errors of a BMC. The simulator logs its fixture's vendor the same way.
func TestLogEscapesEachLine(t *testing.T) {
	generic, err := os.ReadFile(filepath.Join("..", "..", "internal", "bmc", "sim", "fixtures", "generic.yaml"))
	require.NoError(t, err)
	fixture := filepath.Join(t.TempDir(), "hostile.yaml")
	require.NoError(t, os.WriteFile(fixture,
		bytes.Replace(generic, []byte("vendor: Generic"), []byte("vendor: "+strconv.Quote(hostile)), 1), 0o600))

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), runAsCLI+"=sim serve --listen 127.0.0.1:0 --fixture "+fixture)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stderr).ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, line, "sim: serving generic (")
	assertEscaped(t, line)
}

// The text that a cluster object holds is escaped where pk-metal prints it:
// a condition message, an annotation, a step message, a BMC's serial.
func TestVerbsEscapeTheClusterText(t *testing.T) {
	ctx := context.Background()
	t.Run("status notes", func(t *testing.T) {
		assertEscaped(t, renderReport(statusReport{Hosts: []hostStatus{
			{Name: "h1", BlockedOn: "Stuck", Message: hostile, NeedsAttention: true}}}))
	})
	t.Run("approval plan", func(t *testing.T) {
		op, objs := bootPlan()
		op.Spec.Reason = hostile
		pc, err := loadPlanContext(ctx, fakeClient(objs...), op)
		require.NoError(t, err)
		assertEscaped(t, renderPlan(op, pc))
	})
	t.Run("an operation that cannot be approved", func(t *testing.T) {
		op, _ := bootPlan()
		op.Spec.Approval.ApprovedBy = hostile
		var out bytes.Buffer
		err := approveAll(ctx, fakeClient(op), []types.NamespacedName{
			{Namespace: op.Namespace, Name: op.Name}, {Namespace: op.Namespace, Name: "gone"}},
			approval{User: "alice", Yes: true, Out: &out})
		require.Error(t, err)
		assertEscaped(t, out.String())
	})
	t.Run("rollout", func(t *testing.T) {
		ro := planned()
		ro.Status.Steps = []osv1alpha1.RolloutStepStatus{{State: osv1alpha1.RolloutNodeFailed, Message: hostile}}
		assertEscaped(t, renderRollout(ro))
	})
	t.Run("restore plan", func(t *testing.T) {
		assertEscaped(t, (&restorePlan{Cluster: "dr", Findings: []string{hostile}}).render())
	})
	t.Run("retry", func(t *testing.T) {
		op, objs := bootPlan()
		op.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
			Reason: metalv1alpha1.ReasonStepFailed, Message: hostile}}
		var out bytes.Buffer
		require.NoError(t, retry(ctx, fakeClient(objs...), op.Namespace, op.Name, &out))
		assertEscaped(t, out.String())
	})
	t.Run("abort", func(t *testing.T) {
		op, objs := bootPlan()
		op.Annotations = map[string]string{metalv1alpha1.AnnotationAbort: hostile}
		var out bytes.Buffer
		require.NoError(t, abort(ctx, fakeClient(objs...), op.Namespace, op.Name, "alice", "", &out))
		assertEscaped(t, out.String())
	})
	t.Run("backup", func(t *testing.T) {
		snap := verifiedSnapshot("dr-1", time.Now(), []byte("x"))
		snap.Status.Location = hostile
		var out bytes.Buffer
		require.NoError(t, waitSnapshot(ctx, fakeClient(snap), snap.Name, time.Millisecond, &out))
		assertEscaped(t, out.String())
	})
}

// pk-metal top escapes the text of a machine and of a key's outcome before
// it lays them out. Its own colours stay: only what it styles is not text.
func TestTopEscapesTheText(t *testing.T) {
	in := labRack()
	in.Fleet.Machines[1].Personality = hostile
	in.Consoles["node-2"] = append(in.Consoles["node-2"], hostile)
	m, _ := feed(t, newTopModel(nil, &fakeActions{}, nil, 0),
		tea.WindowSizeMsg{Width: 180, Height: 30}, topDataMsg(in), key("tab"),
		topDoneMsg{err: errors.New(hostile)})
	out := m.render()
	for _, c := range []string{"\x07", "\r", "\x1b]52"} {
		assert.NotContains(t, out, c)
	}
	assert.Equal(t, 3, strings.Count(out, termtext.Escape(hostile)), "the subtitle, the console line and the status")
}
