package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func talosNodeObj(name string, role osv1alpha1.NodeRole) *osv1alpha1.TalosNode {
	return &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"}, Role: role, Address: "10.0.0.2"},
	}
}

func hostRunning(name, node string) *metalv1alpha1.Host {
	return &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: node}}},
	}
}

func TestProposeReboot(t *testing.T) {
	ctx := context.Background()
	cl := fakeClient(talosNodeObj("w-1", osv1alpha1.RoleWorker), talosNodeObj("cp-2", osv1alpha1.RoleControlPlane),
		hostRunning("pk0004", "w-1"), hostRunning("pk0002", "cp-2"))
	opts := rebootOptions{User: "alice", DrainTimeout: 15 * time.Minute}

	var out bytes.Buffer
	opts.Node = "w-1"
	op, err := proposeReboot(ctx, cl, opts, &out)
	require.NoError(t, err)
	assert.Equal(t, "pk0004", op.Spec.HostRef.Name)
	assert.Equal(t, "alice", op.Spec.ProposedBy, "admission checks the proposer is the caller")
	assert.True(t, *op.Spec.Approval.Required)
	assert.Equal(t, []string{"cordon", "drain", "talosReboot", "waitNodeReady", "uncordon"}, keys(op.Spec.Actions))
	assert.Equal(t, int32(900), op.Spec.Actions[1].Drain.TimeoutSeconds)
	assert.Contains(t, out.String(), "pk-metal approve "+op.Namespace+"/"+op.Name)

	out.Reset()
	opts.Node, opts.PowerCycle, opts.GracePeriod = "cp-2", true, ptr.To[int32](30)
	op, err = proposeReboot(ctx, cl, opts, &out)
	require.NoError(t, err)
	assert.Equal(t, []string{"cordon", "drain", "talosReboot", "waitNodeReady", "waitEtcdHealthy", "uncordon"}, keys(op.Spec.Actions),
		"a control plane is uncordoned only once etcd is healthy again")
	assert.Equal(t, "PowerCycle", op.Spec.Actions[2].TalosReboot.Mode)
	assert.Equal(t, int32(30), *op.Spec.Actions[1].Drain.GracePeriodSeconds)
	assert.Contains(t, out.String(), "control plane of cluster lab")

	opts.Node = "w-9"
	_, err = proposeReboot(ctx, cl, opts, &out)
	assert.ErrorContains(t, err, "no TalosNode w-9")
	require.NoError(t, cl.Create(ctx, talosNodeObj("w-2", osv1alpha1.RoleWorker)))
	opts.Node = "w-2"
	_, err = proposeReboot(ctx, cl, opts, &out)
	assert.ErrorContains(t, err, "no host runs TalosNode w-2")
}

func keys(actions []metalv1alpha1.HostOperationAction) []string {
	var out []string
	for _, a := range actions {
		out = append(out, a.Key())
	}
	return out
}
