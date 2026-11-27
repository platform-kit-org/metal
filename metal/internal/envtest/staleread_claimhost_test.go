package envtest_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/identity"
)

// TestStaleReadClaimHost runs the claim controller and the Host controller on
// stale caches along the life of one claim: bind, hand-off of a running node,
// delete, release, unbind; then a second claim binds the same Host. It is the
// Go side of spec/claimhost/ClaimHost.tla: the invariants below have the
// names of the TLA+ ones (see metal/spec/README.md).
func TestStaleReadClaimHost(t *testing.T) {
	st := startEnv(t)
	ctx := t.Context()
	cl := st.cl

	w := newStaleWorld(t, st,
		&metalv1alpha1.HostClaim{}, &metalv1alpha1.Host{}, &osv1alpha1.TalosNode{}, &metalv1alpha1.HostOperation{})
	hostR := &controller.HostReconciler{Client: w.mgr.GetClient()}
	claimR := &controller.HostClaimReconciler{
		Client: w.mgr.GetClient(), APIReader: cl,
		OperationsNamespace: "default", Proposer: "pk-metal-test",
	}
	require.NoError(t, hostR.SetupWithManager(w.mgr))
	require.NoError(t, claimR.SetupWithManager(w.mgr))
	// The Host controller lists operations by host through the executor's
	// index; the executor itself does not run here.
	require.NoError(t, (&controller.HostOperationReconciler{}).SetupWithManager(w.mgr))

	// One machine behind one endpoint, already inventoried.
	sys := metalv1alpha1.DiscoveredSystem{ID: "1", SerialNumber: "STALE01", Manufacturer: "Generic"}
	hostName, ok := identity.Name(identity.Normalize("Generic", "", sys))
	require.True(t, ok)
	hostKey := types.NamespacedName{Name: hostName}
	require.NoError(t, cl.Create(ctx, &metalv1alpha1.Asset{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-a"},
		Spec: metalv1alpha1.AssetSpec{CID: "CI-stale-a", Serial: "STALE01", BMCMac: "02:00:00:00:5a:01",
			CredentialsRef: corev1.LocalObjectReference{Name: "bmc-creds"}},
	}))
	ep := &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-a"},
		Spec:       metalv1alpha1.ManagementEndpointSpec{AssetRef: metalv1alpha1.Reference{Name: "stale-a"}, Address: "https://192.0.2.50"},
	}
	require.NoError(t, cl.Create(ctx, ep))
	ep.Status.Vendor = "Generic"
	ep.Status.Systems = []metalv1alpha1.DiscoveredSystem{sys}
	ep.Status.VirtualMedia = []metalv1alpha1.DiscoveredMedia{{ID: "CD1", MediaTypes: []string{"CD"}}}
	for _, c := range []string{metalv1alpha1.ConditionReachable, metalv1alpha1.ConditionInventoried, metalv1alpha1.ConditionVerified} {
		meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{Type: c, Status: metav1.ConditionTrue, Reason: "Test"})
	}
	require.NoError(t, cl.Status().Update(ctx, ep))

	// The Host controller runs on the Host; the claim controller on every
	// claim the API server holds.
	w.target("host controller", hostR, func() []reconcile.Request {
		return []reconcile.Request{{NamespacedName: hostKey}}
	})
	w.target("claim controller", claimR, func() []reconcile.Request {
		claims := &metalv1alpha1.HostClaimList{}
		require.NoError(t, cl.List(ctx, claims))
		var out []reconcile.Request
		for _, c := range claims.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
		}
		return out
	})
	staleClaimHostInvariants(w)

	// --- the path --------------------------------------------------------------

	w.settle(ctx, "host controller", hostR, reconcile.Request{NamespacedName: hostKey})

	runningNode := func(name string) {
		node := &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "stale"}, Role: osv1alpha1.RoleWorker,
				Address: "192.0.2.51", InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "disk-1"}},
		}
		require.NoError(t, cl.Create(ctx, node))
		node.Status.Conditions = []metav1.Condition{
			{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonNodeReady, LastTransitionTime: metav1.Now()},
			{Type: osv1alpha1.ConditionMachineReachable, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonMachineAPI, LastTransitionTime: metav1.Now()},
		}
		require.NoError(t, cl.Status().Update(ctx, node))
	}
	newClaim := func(name, node string) types.NamespacedName {
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "stale-a"},
				OS: &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: node}}},
		}))
		return types.NamespacedName{Namespace: "default", Name: name}
	}

	// A node already runs on the machine: the claim records the hand-off
	// without a boot, and the Host reads Provisioned.
	w.act(ctx, "node stale-n1 checks in", func() { runningNode("stale-n1") })
	var first types.NamespacedName
	w.act(ctx, "a person creates claim stale-c1", func() { first = newClaim("stale-c1", "stale-n1") })
	w.settle(ctx, "claim controller", claimR, reconcile.Request{NamespacedName: first})
	w.settle(ctx, "host controller", hostR, reconcile.Request{NamespacedName: hostKey})
	requireHostPhase(t, ctx, cl, hostName, metalv1alpha1.HostProvisioned)

	// The person deletes the claim. The node runs, so the release proposes
	// a power-off and waits for it.
	w.act(ctx, "a person deletes claim stale-c1", func() {
		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Name: first.Name, Namespace: first.Namespace}}))
	})
	w.settle(ctx, "host controller", hostR, reconcile.Request{NamespacedName: hostKey})
	requireHostPhase(t, ctx, cl, hostName, metalv1alpha1.HostReleasing)
	w.settle(ctx, "claim controller", claimR, reconcile.Request{NamespacedName: first})
	release := releaseOpOf(t, ctx, cl, hostName)

	// The executor runs the approved release: the machine is off, the node
	// stops answering.
	w.act(ctx, "the release powers the machine off", func() {
		release.Spec.Approval.ApprovedBy = "alice"
		require.NoError(t, cl.Update(ctx, release))
		meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{Type: metalv1alpha1.ConditionApproved,
			Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonApproved})
		meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{Type: metalv1alpha1.ConditionExecuted,
			Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonStepsSucceeded})
		require.NoError(t, cl.Status().Update(ctx, release))
	})
	w.act(ctx, "node stale-n1 stops answering", func() {
		node := &osv1alpha1.TalosNode{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "stale-n1"}, node))
		node.Status.Conditions = []metav1.Condition{
			{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "NotReady", LastTransitionTime: metav1.Now()},
			{Type: osv1alpha1.ConditionMachineReachable, Status: metav1.ConditionFalse, Reason: "Unreachable", LastTransitionTime: metav1.Now()},
		}
		require.NoError(t, cl.Status().Update(ctx, node))
	})

	// The release unbinds the Host and lets the claim go. Here the Host
	// controller's Host cache can be behind its claim cache: the race of the
	// lab (Releasing, Provisioned, Available).
	w.settle(ctx, "claim controller", claimR, reconcile.Request{NamespacedName: first})
	require.True(t, claimGone(ctx, cl, first), "the release lets claim stale-c1 go")
	w.settle(ctx, "host controller", hostR, reconcile.Request{NamespacedName: hostKey})
	requireHostPhase(t, ctx, cl, hostName, metalv1alpha1.HostAvailable)

	// A second claim binds the released Host.
	w.act(ctx, "node stale-n2 checks in", func() { runningNode("stale-n2") })
	var second types.NamespacedName
	w.act(ctx, "a person creates claim stale-c2", func() { second = newClaim("stale-c2", "stale-n2") })
	w.settle(ctx, "claim controller", claimR, reconcile.Request{NamespacedName: second})
	w.settle(ctx, "host controller", hostR, reconcile.Request{NamespacedName: hostKey})
	requireHostPhase(t, ctx, cl, hostName, metalv1alpha1.HostProvisioned)

	require.Positive(t, w.explored)
	t.Logf("explored %d stale reconcile passes", w.explored)
}

// staleClaimHostInvariants are the invariants of spec/claimhost/ClaimHost.tla,
// judged on the writes of one stale pass.
func staleClaimHostInvariants(w *staleWorld) {
	bound := []metalv1alpha1.HostPhase{metalv1alpha1.HostAssigned, metalv1alpha1.HostProvisioning,
		metalv1alpha1.HostProvisioned}
	refUID := func(h *metalv1alpha1.Host) types.UID {
		if h.Spec.ClaimRef == nil {
			return ""
		}
		return h.Spec.ClaimRef.UID
	}

	// ReleasedHostNeverProvisioned: no write puts a phase that says a claim
	// holds the Host, derived from a bind the API server already ended.
	w.invariant("ReleasedHostNeverProvisioned", func(ctx context.Context, p *stalePass) string {
		for i, wr := range p.writes {
			h, ok := wr.obj.(*metalv1alpha1.Host)
			if !ok || wr.verb != "status apply" || !slices.Contains(bound, h.Status.Phase) {
				continue
			}
			read := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: h.Name}}
			live := read.DeepCopy()
			if !p.read(read) || !p.current(ctx, live, i) {
				continue
			}
			if refUID(read) != refUID(live) {
				return fmt.Sprintf("wrote phase %s from claimRef %q, but the API server holds claimRef %q",
					h.Status.Phase, refUID(read), refUID(live))
			}
		}
		return ""
	})

	// BoundClaimHoldsHost: a claim that reports Bound holds the Host.
	w.invariant("BoundClaimHoldsHost", func(ctx context.Context, p *stalePass) string {
		for i, wr := range p.writes {
			c, ok := wr.obj.(*metalv1alpha1.HostClaim)
			if !ok || wr.verb != "status update" || !c.DeletionTimestamp.IsZero() ||
				!meta.IsStatusConditionTrue(c.Status.Conditions, metalv1alpha1.ConditionBound) || c.Status.HostRef == nil {
				continue
			}
			h := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: c.Status.HostRef.Name}}
			if !p.current(ctx, h, i) {
				return fmt.Sprintf("claim %s reports Bound to host %s, which does not exist", c.Name, h.Name)
			}
			if refUID(h) != c.UID {
				return fmt.Sprintf("claim %s reports Bound to host %s, which holds claimRef %q", c.Name, h.Name, refUID(h))
			}
		}
		return ""
	})

	// FinalizerLast: the finalizer goes only after no Host carries the claim.
	w.invariant("FinalizerLast", func(ctx context.Context, p *stalePass) string {
		for i, wr := range p.writes {
			c, ok := wr.obj.(*metalv1alpha1.HostClaim)
			if !ok || wr.verb != "update" || c.DeletionTimestamp.IsZero() || len(c.Finalizers) > 0 {
				continue
			}
			hosts := &metalv1alpha1.HostList{}
			if err := p.w.live.List(ctx, hosts); err != nil {
				return err.Error()
			}
			for _, h := range hosts.Items {
				if p.current(ctx, &h, i) && refUID(&h) == c.UID {
					return fmt.Sprintf("the finalizer of claim %s goes while host %s carries it", c.Name, h.Name)
				}
			}
		}
		return ""
	})

	// PowerOffBeforeUnbind: a Host whose release decided on a power-off is
	// unbound only after a release operation of the claim succeeded.
	w.invariant("PowerOffBeforeUnbind", func(ctx context.Context, p *stalePass) string {
		for i, wr := range p.writes {
			h, ok := wr.obj.(*metalv1alpha1.Host)
			if !ok || wr.verb != "update" || h.Spec.ClaimRef != nil {
				continue
			}
			before := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: h.Name}}
			if !p.current(ctx, before, i) || before.Spec.ClaimRef == nil ||
				before.Annotations[releasePowerOffAnnotation] != string(before.Spec.ClaimRef.UID) {
				continue
			}
			ops := &metalv1alpha1.HostOperationList{}
			if err := p.w.live.List(ctx, ops); err != nil {
				return err.Error()
			}
			if !slices.ContainsFunc(ops.Items, func(op metalv1alpha1.HostOperation) bool {
				return op.Spec.HostRef.Name == h.Name && strings.HasPrefix(op.Name, "release-") &&
					meta.IsStatusConditionTrue(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
			}) {
				return fmt.Sprintf("host %s unbound from claim %s with no succeeded release operation", h.Name, before.Spec.ClaimRef.Name)
			}
		}
		return ""
	})

	// NoSteal: no claim takes the Host from a claim that still exists.
	w.invariant("NoSteal", func(ctx context.Context, p *stalePass) string {
		for i, wr := range p.writes {
			h, ok := wr.obj.(*metalv1alpha1.Host)
			if !ok || wr.verb != "update" || h.Spec.ClaimRef == nil {
				continue
			}
			before := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: h.Name}}
			if !p.current(ctx, before, i) || before.Spec.ClaimRef == nil || before.Spec.ClaimRef.UID == h.Spec.ClaimRef.UID {
				continue
			}
			holder := &metalv1alpha1.HostClaim{}
			ref := before.Spec.ClaimRef
			if err := p.w.live.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, holder); err == nil &&
				holder.UID == ref.UID {
				return fmt.Sprintf("host %s moved from live claim %s to %s", h.Name, ref.Name, h.Spec.ClaimRef.Name)
			}
		}
		return ""
	})
}

// releasePowerOffAnnotation is where the release records its power-off
// decision on the Host, with the claim's UID as the value.
const releasePowerOffAnnotation = "metal.platform-kit.org/release-power-off"

func requireHostPhase(t *testing.T, ctx context.Context, cl client.Client, name string, want metalv1alpha1.HostPhase) {
	t.Helper()
	h := &metalv1alpha1.Host{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: name}, h))
	require.Equal(t, want, h.Status.Phase, "host %s: conditions %+v", name, h.Status.Conditions)
}

func releaseOpOf(t *testing.T, ctx context.Context, cl client.Client, host string) *metalv1alpha1.HostOperation {
	t.Helper()
	ops := &metalv1alpha1.HostOperationList{}
	require.NoError(t, cl.List(ctx, ops, client.InNamespace("default")))
	for i := range ops.Items {
		if ops.Items[i].Spec.HostRef.Name == host && ops.Items[i].Spec.Reason != "" && slices.ContainsFunc(ops.Items[i].Spec.Actions,
			func(a metalv1alpha1.HostOperationAction) bool { return a.PowerOff != nil }) {
			return &ops.Items[i]
		}
	}
	t.Fatalf("no release operation for host %s", host)
	return nil
}

func claimGone(ctx context.Context, cl client.Client, key types.NamespacedName) bool {
	return apierrors.IsNotFound(cl.Get(ctx, key, &metalv1alpha1.HostClaim{}))
}
