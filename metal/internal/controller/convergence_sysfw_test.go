package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

func sysProfile(name string, layer int32, targets ...metalv1alpha1.SystemFirmwareTarget) metalv1alpha1.FirmwareProfile {
	return metalv1alpha1.FirmwareProfile{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: metalv1alpha1.FirmwareProfileSpec{Layer: layer, System: targets}}
}

func TestMergeSystemFirmware(t *testing.T) {
	bios1 := metalv1alpha1.SystemFirmwareTarget{Component: "BIOS", Version: "1", Image: "r/b@sha256:01"}
	bios2 := metalv1alpha1.SystemFirmwareTarget{Component: "BIOS", Version: "2", Image: "r/b@sha256:02"}
	nic := metalv1alpha1.SystemFirmwareTarget{Component: `NIC\..*`, Version: "9", Image: "r/n@sha256:09"}

	got, conflicts := mergeSystemFirmware([]metalv1alpha1.FirmwareProfile{
		sysProfile("site", 0, bios1, nic), sysProfile("rack", 10, bios2),
	})
	assert.Empty(t, conflicts)
	assert.Equal(t, []metalv1alpha1.SystemFirmwareTarget{bios2, nic}, got, "a later layer overrides per component, sorted")

	_, conflicts = mergeSystemFirmware([]metalv1alpha1.FirmwareProfile{sysProfile("a", 5, bios1), sysProfile("b", 5, bios2)})
	require.Len(t, conflicts, 1)
	assert.Contains(t, conflicts[0], "a and b")

	assert.Empty(t, managerProfiles([]metalv1alpha1.FirmwareProfile{sysProfile("a", 0, bios1)}),
		"a system-only profile is not a manager profile")
}

func TestSystemDrift(t *testing.T) {
	inv := []metalv1alpha1.FirmwareInventoryItem{
		{ID: "BMC", Version: "2.0", Updateable: true},
		{ID: "BIOS", Version: "1", Updateable: true},
		{ID: "NIC.Slot.1", Version: "8", Updateable: true},
		{ID: "NIC.Slot.2", Version: "9", Updateable: true},
		{ID: "CPLD", Version: "1", Updateable: false},
	}
	targets := []metalv1alpha1.SystemFirmwareTarget{
		{Component: "BIOS", Version: "1", Image: "i@sha256:01"},
		{Component: `NIC\.Slot\.[0-9]+`, Version: "9", Image: "i@sha256:09"},
	}
	stages, drifted, hold := systemDrift(targets, inv)
	require.Empty(t, hold)
	assert.Equal(t, []systemStage{{Component: `NIC\.Slot\.[0-9]+`, Version: "9", Image: "i@sha256:09", Entries: []string{"NIC.Slot.1"}}}, stages,
		"only the entries off their version are staged")
	assert.Equal(t, []string{"NIC.Slot.1 serves 8, want 9"}, drifted)

	_, _, hold = systemDrift([]metalv1alpha1.SystemFirmwareTarget{{Component: "NIC", Version: "9"}}, inv)
	assert.Contains(t, hold, "matches no inventory entry", "the pattern is anchored: NIC alone matches no NIC.Slot.n")

	_, _, hold = systemDrift([]metalv1alpha1.SystemFirmwareTarget{{Component: "CPLD", Version: "2"}}, inv)
	assert.Contains(t, hold, "not updateable")

	_, _, hold = systemDrift([]metalv1alpha1.SystemFirmwareTarget{{Component: "(", Version: "2"}}, inv)
	assert.Contains(t, hold, "not a valid pattern")
}

func TestDisruptivePlanShapes(t *testing.T) {
	stage := []metalv1alpha1.HostOperationAction{{ApplyBios: &metalv1alpha1.ApplyBiosAction{}}}
	keys := func(as []metalv1alpha1.HostOperationAction) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.Key())
		}
		return out
	}
	assert.Equal(t, []string{"applyBios", "powerCycle"}, keys(disruptivePlan(nil, stage)))
	worker := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "w1"}, Spec: osv1alpha1.TalosNodeSpec{Role: osv1alpha1.RoleWorker}}
	assert.Equal(t, []string{"applyBios", "cordon", "drain", "talosReboot", "waitNodeReady", "uncordon"},
		keys(disruptivePlan(worker, stage)), "a worker has no etcd member to wait for")
	cp := worker.DeepCopy()
	cp.Spec.Role = osv1alpha1.RoleControlPlane
	assert.Equal(t, []string{"applyBios", "waitEtcdHealthy", "cordon", "drain", "talosReboot", "waitNodeReady", "waitEtcdHealthy", "uncordon"},
		keys(disruptivePlan(cp, stage)))

	desired := map[string]any{"QuietBoot": false}
	assert.Equal(t, planHash(desired), disruptiveHash(desired, nil), "without a node the hash is the one plans had before")
	assert.NotEqual(t, disruptiveHash(desired, worker), disruptiveHash(desired, cp), "the role is part of the plan")
	assert.NotEqual(t, disruptiveHash(desired, nil), disruptiveHash(desired, worker))
}

// laneRig is a ConvergenceReconciler over a fake API, with host-a behind
// endpoint bmc-a, whose observations the test sets.
func laneRig(t *testing.T, ep *metalv1alpha1.ManagementEndpoint, host *metalv1alpha1.Host, objs ...client.Object) (*ConvergenceReconciler, client.Client) {
	t.Helper()
	ep.Name = "bmc-a"
	host.Name = "host-a"
	host.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: ep.Name}, Reachable: true}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(append(objs, ep, host)...).Build()
	return &ConvergenceReconciler{Client: cl, OperationsNamespace: "pk-metal", Proposer: "operator"}, cl
}

// laneCond records the last condition a lane set.
type laneCond struct{ reason, msg string }

func (c *laneCond) set(_ string, _ metav1.ConditionStatus, reason, msg string) {
	c.reason, c.msg = reason, msg
}

func hostOps(t *testing.T, cl client.Client) []metalv1alpha1.HostOperation {
	t.Helper()
	ops := &metalv1alpha1.HostOperationList{}
	require.NoError(t, cl.List(t.Context(), ops))
	return ops.Items
}

// A plan reboots the machine to apply system firmware. A machine that can
// still hold a released node's install is not powered on for it.
func TestSystemFirmwareHoldsOnADiskNotWiped(t *testing.T) {
	ep := &metalv1alpha1.ManagementEndpoint{Status: metalv1alpha1.ManagementEndpointStatus{
		Firmware:     []metalv1alpha1.FirmwareInventoryItem{{ID: "BIOS", Version: "1", Updateable: true}},
		Capabilities: &metalv1alpha1.EndpointCapabilities{UpdateDialect: string(bmc.DialectMultipartPush)},
	}}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{metalv1alpha1.AnnotationDiskNotWiped: "lab/w-1"}}}
	r, cl := laneRig(t, ep, host)
	var c laneCond

	ok, err := r.systemFirmware(t.Context(), host,
		[]metalv1alpha1.FirmwareProfile{sysProfile("bios", 0, metalv1alpha1.SystemFirmwareTarget{Component: "BIOS", Version: "2", Image: "r/b@sha256:02"})},
		true, c.set)

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, metalv1alpha1.ReasonDiskNotWiped, c.reason, c.msg)
	assert.Empty(t, hostOps(t, cl), "nothing that powers the machine on is proposed")
	assert.Nil(t, host.Status.SystemFirmwareProposed)
}

// A changed plan withdraws the old proposal that nobody approved, also when
// no path can carry the new one.
func TestFirmwareWithdrawsASupersededProposalWithoutTransport(t *testing.T) {
	ep := &metalv1alpha1.ManagementEndpoint{Status: metalv1alpha1.ManagementEndpointStatus{FirmwareVersion: "1"}}
	old := metalv1alpha1.FirmwareTarget{Version: "2", Image: "r/f@sha256:02"}
	stale := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a-firmware-old", Namespace: "pk-metal"},
		Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "host-a"},
			Approval: metalv1alpha1.OperationApproval{Required: ptr.To(true)}},
	}
	host := &metalv1alpha1.Host{Status: metalv1alpha1.HostStatus{FirmwareProposed: &metalv1alpha1.ProposedPlan{
		PlanHash: planHash(old), Operation: "pk-metal/host-a-firmware-old"}}}
	r, cl := laneRig(t, ep, host, stale)
	var c laneCond
	profile := metalv1alpha1.FirmwareProfile{ObjectMeta: metav1.ObjectMeta{Name: "fw"},
		Spec: metalv1alpha1.FirmwareProfileSpec{Manager: &metalv1alpha1.FirmwareTarget{Version: "3", Image: "r/f@sha256:03"}}}

	ok, err := r.firmware(t.Context(), host, []metalv1alpha1.FirmwareProfile{profile}, c.set)

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, metalv1alpha1.ReasonNoUpdateTransport, c.reason, c.msg)
	assert.Empty(t, hostOps(t, cl), "the superseded proposal is withdrawn")
	assert.Nil(t, host.Status.FirmwareProposed, "and forgotten")
}
