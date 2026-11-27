package main

import (
	"fmt"
	"slices"
	"strings"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/metrics"
)

// rolloutStatus is one TalosRollout row of -o json.
type rolloutStatus struct {
	Name           string `json:"name"`
	Cluster        string `json:"cluster"`
	Kind           string `json:"kind"`
	NodesDone      int    `json:"nodesDone"`
	NodesTotal     int    `json:"nodesTotal"`
	State          string `json:"state"`
	ApprovedBy     string `json:"approvedBy"`
	Message        string `json:"message"`
	NeedsAttention bool   `json:"needsAttention"`
}

// rolloutNeedsPerson are the states in which a rollout does not go on until
// somebody acts: approve it, resume it after its first node, or read why it
// cannot plan or why it ended. Running a node, waiting for health between
// nodes and a pause a person set are the plan working; the alert
// PkMetalRolloutStalled covers one that waits for too long.
var rolloutNeedsPerson = []string{
	osv1alpha1.ReasonAwaitingApproval, osv1alpha1.ReasonPlanBlocked, osv1alpha1.ReasonPlanChanged,
	osv1alpha1.ReasonCanaryPause, osv1alpha1.ReasonRolloutFailed, osv1alpha1.ReasonRolloutAborted,
}

// rolloutRow is one rollout as status, top and rollout list show it. The
// state is read the same way as the metric pk_metal_rollout_condition, so the
// tables and the alerts agree.
func rolloutRow(ro *osv1alpha1.TalosRollout) rolloutStatus {
	st := metrics.RolloutStateOf(ro)
	return rolloutStatus{Name: ro.Name, Cluster: ro.Spec.ClusterRef.Name, Kind: string(ro.Spec.Kind),
		NodesDone: ro.Status.StepsSucceeded(), NodesTotal: len(ro.Status.Plan), State: st.Reason,
		ApprovedBy: ro.Spec.Approval.ApprovedBy, Message: st.Message,
		NeedsAttention: !st.Succeeded && slices.Contains(rolloutNeedsPerson, st.Reason)}
}

// addRollouts fills the ROLLOUT rows.
func addRollouts(rep *statusReport, rollouts []osv1alpha1.TalosRollout) {
	for i := range rollouts {
		rep.Rollouts = append(rep.Rollouts, rolloutRow(&rollouts[i]))
	}
	slices.SortFunc(rep.Rollouts, func(a, b rolloutStatus) int { return strings.Compare(a.Name, b.Name) })
}

// history: the rollout succeeded, or had nothing to do. It is history, as a
// succeeded operation is; status and top show only the others.
func (r rolloutStatus) history() bool {
	return r.State == osv1alpha1.ReasonRolloutSucceeded || r.State == osv1alpha1.ReasonNothingToDo
}

// rolloutTable is the ROLLOUT table: the rollouts that are not history.
func rolloutTable(rep statusReport) table {
	t := table{header: "ROLLOUT\tCLUSTER\tKIND\tNODES\tSTATE\tAPPROVED"}
	for _, r := range rep.Rollouts {
		if r.history() {
			continue
		}
		t.row(r.Name, orDash(r.Cluster), orDash(r.Kind), fmt.Sprintf("%d/%d", r.NodesDone, r.NodesTotal),
			orDash(r.State), orDash(r.ApprovedBy))
		t.noteIf(r.NeedsAttention, r.Name, r.Message, r.State)
	}
	return t
}
