package controller

import (
	"context"
	"errors"
	"strconv"

	"github.com/siderolabs/talos/pkg/machinery/role"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// memberIDHex is a member ID the way etcd prints it, and the way the
// etcdRemoveMember action carries it.
func memberIDHex(id uint64) string { return strconv.FormatUint(id, 16) }

// clusterEtcd reads the etcd membership of cluster through its control
// planes, with a talosconfig minted for this reading from the cluster's
// secrets bundle. It returns a report that is not known, and says why, when
// there is nothing to ask with.
//
// Adding and removing control planes is decided on this live reading, never
// on the TalosNode count: a member left behind by a node that was wiped
// without leaving is still a member, and still counts towards the majority
// etcd needs, until something removes it. It is read through the same path
// as the cluster guard's (etcdHealth in talosaccess.go), so the guard that
// holds a reboot and the verdict that holds a removal never see two
// different etcds.
func clusterEtcd(ctx context.Context, c client.Reader, dial talos.Dialer, namespace string,
	cluster *osv1alpha1.TalosCluster) (*etcdReport, string, error) {
	if dial == nil {
		return &etcdReport{}, "this operator has no Talos client configured", nil
	}
	nodes, err := clusterMembers(ctx, c, cluster.Name)
	if err != nil {
		return &etcdReport{}, "", err
	}
	cps := controlPlanes(nodes, "")
	acc, err := newTalosAccessAs(ctx, c, namespace, dial, cluster, role.Reader)
	if err != nil {
		if unavailable := (*bundleUnavailable)(nil); errors.As(err, &unavailable) {
			return &etcdReport{}, unavailable.msg, nil
		}
		return &etcdReport{}, "", err
	}
	rep, err := acc.etcdHealth(ctx, cps, nodes)
	if err != nil {
		// Nobody listed the members: an observation, not a failure.
		return &etcdReport{}, err.Error(), nil
	}
	return rep, "", nil
}

// known: some control plane listed the members.
func (r *etcdReport) known() bool { return r.via != "" }

// answers: the member's own node answered its etcd status with no error and
// a known leader. A learner can answer too.
func (r *etcdReport) answers(id uint64) bool {
	_, sick := r.sick[id]
	return !sick
}

// voting counts the members that vote (not learners); healthyVoting those of
// them that answer.
func (r *etcdReport) voting() (voting, healthyVoting int) {
	for _, m := range r.members {
		if m.IsLearner {
			continue
		}
		voting++
		if r.answers(m.ID) {
			healthyVoting++
		}
	}
	return voting, healthyVoting
}

// learnerNames names the members that do not vote yet, as the member list
// says.
func (r *etcdReport) learnerNames() []string {
	var out []string
	for _, m := range r.members {
		if m.IsLearner {
			out = append(out, m.Hostname)
		}
	}
	return out
}

// unhealthy names the members whose node did not report a healthy etcd.
func (r *etcdReport) unhealthy() []string {
	var out []string
	for _, m := range r.members {
		if !r.answers(m.ID) {
			out = append(out, m.Hostname)
		}
	}
	return out
}
