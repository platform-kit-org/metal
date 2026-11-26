package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func requested(name string) map[string]string {
	return map[string]string{metalv1alpha1.AnnotationRequestedBy: name}
}

// A rollout names everyone who last changed what it applies: the cluster,
// its nodes and the patch ConfigMaps in the operator's namespace.
func TestClusterRequesters(t *testing.T) {
	patch := func(ns, name string) osv1alpha1.Patch {
		return osv1alpha1.Patch{ConfigMapRef: &osv1alpha1.ConfigMapKeyReference{Namespace: ns, Name: name, Key: "p"}}
	}
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Annotations: requested("alice")},
		Spec: osv1alpha1.TalosClusterSpec{Patches: []osv1alpha1.Patch{patch("pk-metal", "net"), patch("tenant", "far")}}}
	members := []osv1alpha1.TalosNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Annotations: requested("bob")},
			Spec: osv1alpha1.TalosNodeSpec{Patches: []osv1alpha1.Patch{patch("pk-metal", "net"), patch("pk-metal", "gone")}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Annotations: requested("alice")}},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "net", Annotations: requested("carol")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "far", Annotations: requested("mallory")}},
	).Build()
	got, err := clusterRequesters(t.Context(), cl, "pk-metal", cluster, members)
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob", "carol"}, got,
		"sorted, once each; a ConfigMap the operator does not read names nobody")
}

// An unapproved rollout the operator proposed ends when a person outside
// its requestedBy changed what it applies; the proposer proposes it again.
func TestRolloutEndsWhenItsRequestersMove(t *testing.T) {
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Annotations: requested("alice")}}
	ro := &osv1alpha1.TalosRollout{ObjectMeta: metav1.ObjectMeta{Name: "c-config-1"},
		Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}, Kind: osv1alpha1.RolloutConfig,
			ProposedBy: "operator", RequestedBy: []string{"alice"}}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cluster).Build()
	r := &TalosRolloutReconciler{Client: cl, Namespace: "pk-metal", Proposer: "operator"}

	stop, err := r.requestersMoved(t.Context(), ro)
	require.NoError(t, err)
	assert.False(t, stop, "the same requesters: the rollout goes on")

	cluster.Annotations = requested("bob")
	require.NoError(t, cl.Update(t.Context(), cluster))
	stop, err = r.requestersMoved(t.Context(), ro)
	require.NoError(t, err)
	require.True(t, stop)
	c := meta.FindStatusCondition(ro.Status.Conditions, osv1alpha1.ConditionComplete)
	require.NotNil(t, c)
	assert.Equal(t, osv1alpha1.ReasonPlanChanged, c.Reason)
	assert.Contains(t, c.Message, "bob changed what this rollout applies")

	person := ro.DeepCopy()
	person.Status = osv1alpha1.TalosRolloutStatus{}
	person.Spec.ProposedBy = "dave"
	stop, err = r.requestersMoved(t.Context(), person)
	require.NoError(t, err)
	assert.False(t, stop, "a rollout a person created names that person; the check is for the operator's")
}

// An install puts the cluster's config and patches on the machine, not the
// claim's and the node's alone. Before, it named only who last changed the
// claim or the node: under selfApproval: Forbidden, the editor of a patch
// could approve the install that carries the edit, the review the rollout
// of the same patch asks for.
func TestInstallNamesWhoChangedWhatItApplies(t *testing.T) {
	patch := osv1alpha1.Patch{ConfigMapRef: &osv1alpha1.ConfigMapKeyReference{Namespace: "pk-metal", Name: "net", Key: "p"}}
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Annotations: requested("alice")},
		Spec: osv1alpha1.TalosClusterSpec{Patches: []osv1alpha1.Patch{patch}}}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "w-1", Annotations: requested("bob")},
		Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}}}
	other := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "w-2", Annotations: requested("dave")},
		Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}}}
	claim := testClaim("w-1")
	claim.Annotations = requested("bob")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(other,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "net", Annotations: requested("carol")}},
	).Build()
	r := &HostClaimReconciler{Client: cl, CredentialsNamespace: "pk-metal"}

	got, err := r.installRequesters(t.Context(), claim, node, cluster)
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob", "carol"}, got,
		"the cluster, the patch, the node and the claim; not another node of the cluster")
}

// A claim's labels go onto its host, and a profile can select the host by
// them. Before, a BIOS or firmware proposal named only who last changed
// the profiles: under selfApproval: Forbidden, the person who opted the
// machine in by a claim label could approve the change that followed.
func TestConvergenceNamesTheClaimThatChoseTheProfile(t *testing.T) {
	claim := &metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "box", Annotations: requested("alice")},
		Spec: metalv1alpha1.HostClaimSpec{Labels: map[string]metalv1alpha1.LabelValue{"role": "gpu"}}}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "box-1"},
		Spec: metalv1alpha1.HostSpec{ClaimRef: &metalv1alpha1.ClaimReference{Namespace: "team", Name: "box"}}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(claim).Build()
	byRole := metalv1alpha1.BiosProfile{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Annotations: requested("bob")},
		Spec: metalv1alpha1.BiosProfileSpec{Match: metav1.LabelSelector{MatchLabels: map[string]string{"role": "gpu"}}}}
	byVendor := metalv1alpha1.BiosProfile{ObjectMeta: metav1.ObjectMeta{Name: "base", Annotations: requested("carol")},
		Spec: metalv1alpha1.BiosProfileSpec{Match: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "metal.platform-kit.org/vendor", Operator: metav1.LabelSelectorOpExists}}}}}

	got, err := convergenceRequesters(t.Context(), cl, host, []metalv1alpha1.BiosProfile{byRole, byVendor})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob", "carol"}, got, "the claim's label selects a profile")

	got, err = convergenceRequesters(t.Context(), cl, host, []metalv1alpha1.BiosProfile{byVendor})
	require.NoError(t, err)
	assert.Equal(t, []string{"carol"}, got, "a profile on the discovered facts alone: the claim chose nothing")

	fw := metalv1alpha1.FirmwareProfile{ObjectMeta: metav1.ObjectMeta{Name: "fw", Annotations: requested("dave")},
		Spec: metalv1alpha1.FirmwareProfileSpec{Match: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "role", Operator: metav1.LabelSelectorOpIn, Values: []string{"gpu"}}}}}}
	got, err = convergenceRequesters(t.Context(), cl, host, []metalv1alpha1.FirmwareProfile{fw})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "dave"}, got, "an expression on the claim's label counts too")

	host.Spec.ClaimRef.Name = "gone"
	got, err = convergenceRequesters(t.Context(), cl, host, []metalv1alpha1.FirmwareProfile{fw})
	require.NoError(t, err)
	assert.Equal(t, []string{"dave"}, got, "a claim that is gone names nobody")
}
