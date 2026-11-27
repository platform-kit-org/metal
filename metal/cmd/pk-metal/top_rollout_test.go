package main

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The header shows a rollout that has not succeeded, and one that waits for
// a person counts as needing one, as in pk-metal status.
func TestTopShowsRollouts(t *testing.T) {
	in := labRack()
	canary := statusRollout("upgrade-1", []osv1alpha1.RolloutNodeState{osv1alpha1.RolloutNodeSucceeded, osv1alpha1.RolloutNodePending},
		metav1.Condition{Type: osv1alpha1.ConditionApproved, Status: "True", Reason: osv1alpha1.ReasonPlanApproved},
		metav1.Condition{Type: osv1alpha1.ConditionPaused, Status: "True", Reason: osv1alpha1.ReasonCanaryPause})
	done := statusRollout("old", nil,
		metav1.Condition{Type: osv1alpha1.ConditionComplete, Status: "True", Reason: osv1alpha1.ReasonRolloutSucceeded})
	in.Rollouts = []osv1alpha1.TalosRollout{*canary, *done}

	before := buildTop(labRack()).Attention
	d := buildTop(in)
	require.Len(t, d.Rollouts, 1, "a succeeded rollout is history")
	assert.Equal(t, before+1, d.Attention)

	m, _ := feed(t, newTopModel(nil, &fakeActions{}, nil, 0), tea.WindowSizeMsg{Width: 100, Height: 50}, topDataMsg(in))
	assert.Contains(t, screen(m), "rollout upgrade-1 (Upgrade, cluster lab): ✗ PausedAfterFirstNode  nodes 1/2")
	assert.NotContains(t, screen(m), "rollout old")
}

// A running operation shows the step it is at and what that step waits for.
func TestTopShowsWhyARunningOperationWaits(t *testing.T) {
	in := labRack()
	msg := "step 7/9 awaitCheckIn: BMC unavailable, retrying: connection refused"
	in.Ops[1].Status.Conditions[1].Message = msg
	assert.Equal(t, msg, buildTop(in).Tiles[1].Op.Message)
	m, _ := feed(t, newTopModel(nil, &fakeActions{}, nil, 0), tea.WindowSizeMsg{Width: 100, Height: 50}, topDataMsg(in), key("tab"))
	assert.Contains(t, screen(m), "BMC unavailable, retrying")
}

// An aborted operation shows on its tile, as a failed one does, and r
// retries it: pk-metal retry takes both.
func TestTopShowsAnAbortedOperation(t *testing.T) {
	in := labRack()
	in.Fleet, in.Hosts, in.Ops = nil, in.Hosts[3:], in.Ops[3:]
	in.Ops[0].Status.Phase = metalv1alpha1.OperationAborted
	in.Ops[0].Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
		Reason: metalv1alpha1.ReasonAborted, Message: "aborted by bob"}}
	d := buildTop(in)
	require.Len(t, d.Tiles, 1)
	require.NotNil(t, d.Tiles[0].Op)
	assert.Equal(t, "3/9 insertMedia aborted", d.Tiles[0].Op.Step)
	assert.Equal(t, 1, d.Attention)

	act := &fakeActions{}
	m, _ := feed(t, newTopModel(nil, act, nil, 0), tea.WindowSizeMsg{Width: 100}, topDataMsg(in))
	_, cmd := feed(t, m, key("r"))
	require.NotNil(t, cmd, "r retries an aborted operation")
	cmd()
	assert.Equal(t, []string{"pk-metal/bios-real1-9f8e7d6c"}, act.retried)
}

// hangingActions never answers until its context ends.
type hangingActions struct{ fakeActions }

func (h *hangingActions) Approve(ctx context.Context, _, _ string, _ shownPlan) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (h *hangingActions) Plan(ctx context.Context, _, _ string) (shownPlan, error) {
	<-ctx.Done()
	return shownPlan{}, ctx.Err()
}

// A hung API server must not leave the screen busy for good: the key's call
// ends at its deadline and says that the outcome is not known.
func TestTopActionsHaveADeadline(t *testing.T) {
	act := &hangingActions{}
	m := newTopModel(nil, act, nil, 0)
	m.actTimeout = 50 * time.Millisecond
	m, _ = feed(t, m, tea.WindowSizeMsg{Width: 100}, topDataMsg(labRack()))
	m, _ = feed(t, m, key("tab"), key("tab")) // node-3, whose operation waits
	m, cmd := feed(t, m, key("a"))
	require.NotNil(t, cmd)

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		m, _ = feed(t, m, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("the plan call did not end at its deadline")
	}
	assert.False(t, m.busy, "the screen takes keys again")
	assert.True(t, m.statusBad)
	assert.Contains(t, m.status, "did not answer within")
	assert.Contains(t, m.status, "outcome is not known")
}
