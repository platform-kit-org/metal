package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// holderRollout is an approved rollout of cluster c with one worker step,
// created at the given minute.
func holderRollout(name string, minute int, state osv1alpha1.RolloutNodeState) osv1alpha1.TalosRollout {
	ro := approvedRollout(osv1alpha1.RolloutReboot, step("w-"+name, osv1alpha1.RoleWorker))
	ro.Name, ro.UID = name, types.UID("uid-"+name)
	ro.CreationTimestamp = metav1.NewTime(time.Date(2026, 1, 1, 0, minute, 0, 0, time.UTC))
	ro.Status.Steps[0].State = state
	return *ro
}

// A rollout that started holds its cluster: an older rollout approved after
// it waits. Before, the older one went ahead, and two rollouts ran on one
// cluster side by side.
func TestClusterHolderPrefersTheRolloutThatStarted(t *testing.T) {
	const (
		P = osv1alpha1.RolloutNodePending
		R = osv1alpha1.RolloutNodeRunning
	)
	older, newer := holderRollout("older", 1, P), holderRollout("newer", 2, R)
	h := clusterHolder([]osv1alpha1.TalosRollout{older, newer}, "c")
	require.NotNil(t, h)
	assert.Equal(t, "newer", h.Name, "the started rollout holds the cluster")

	newer.Status.Steps[0].State = P
	h = clusterHolder([]osv1alpha1.TalosRollout{newer, older}, "c")
	require.NotNil(t, h)
	assert.Equal(t, "older", h.Name, "of two unstarted rollouts, the older goes first")

	twin := holderRollout("a-twin", 1, P)
	h = clusterHolder([]osv1alpha1.TalosRollout{older, twin}, "c")
	require.NotNil(t, h)
	assert.Equal(t, "a-twin", h.Name, "the name breaks a tie")

	unapproved := holderRollout("unapproved", 0, P)
	unapproved.Spec.Approval = osv1alpha1.RolloutApproval{}
	finished := holderRollout("finished", 0, osv1alpha1.RolloutNodeSucceeded)
	finished.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionComplete, Status: metav1.ConditionTrue}}
	elsewhere := holderRollout("elsewhere", 0, R)
	elsewhere.Spec.ClusterRef.Name = "other"
	h = clusterHolder([]osv1alpha1.TalosRollout{unapproved, finished, elsewhere, older}, "c")
	require.NotNil(t, h)
	assert.Equal(t, "older", h.Name, "unapproved, finished and other clusters' rollouts hold nothing")
	assert.Nil(t, clusterHolder([]osv1alpha1.TalosRollout{unapproved, finished}, "c"))
}

// byCluster gives a fake client the selectable field of the TalosRollout CRD,
// which the API server serves.
func byCluster(b *fake.ClientBuilder) *fake.ClientBuilder {
	return b.WithIndex(&osv1alpha1.TalosRollout{}, rolloutClusterField, func(o client.Object) []string {
		return []string{o.(*osv1alpha1.TalosRollout).Spec.ClusterRef.Name}
	})
}

// The check before a step starts reads the rollouts from the API server. A
// cache that has not yet seen the other rollout's approval would let both
// start.
func TestOtherHolderReadsTheAPIServer(t *testing.T) {
	s := testScheme(t)
	mine := holderRollout("mine", 1, osv1alpha1.RolloutNodePending)
	theirs := holderRollout("theirs", 0, osv1alpha1.RolloutNodeRunning)
	stale := theirs.DeepCopy()
	stale.Spec.Approval = osv1alpha1.RolloutApproval{}
	stale.Status.Steps[0].State = osv1alpha1.RolloutNodePending

	r := &TalosRolloutReconciler{
		Client:    byCluster(fake.NewClientBuilder().WithScheme(s).WithObjects(mine.DeepCopy(), stale)).Build(),
		APIReader: byCluster(fake.NewClientBuilder().WithScheme(s).WithObjects(mine.DeepCopy(), theirs.DeepCopy())).Build(),
	}
	other, err := r.otherHolder(t.Context(), &mine)
	require.NoError(t, err)
	assert.Equal(t, "theirs", other, "the API server shows the other rollout approved and running")

	r.APIReader = nil
	other, err = r.otherHolder(t.Context(), &mine)
	require.NoError(t, err)
	assert.Empty(t, other, "the stale cache alone would have let both run")
}

// The proposer lists the rollouts from the API server. A cache that does
// not yet show the rollout proposed a moment ago would propose a second one
// for the same cluster, here for a node set that drifted further.
func TestProposerReadsRolloutsFromTheAPIServer(t *testing.T) {
	s := testScheme(t)
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2"}}
	cluster.Status.KubernetesVersion = "v1.36.2"
	cluster.Status.BootstrappedAt = &metav1.Time{Time: time.Now()}
	drift := []metav1.Condition{{Type: osv1alpha1.ConditionConfigInSync, Status: metav1.ConditionFalse, Reason: osv1alpha1.ReasonConfigDrift}}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:   osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}},
		Status: osv1alpha1.TalosNodeStatus{Version: "v1.14.6", Conditions: drift, ConfigDigest: "sha256:2"}}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "box-a"},
		Spec: metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: "a"}}}}
	// The rollout proposed for the node's earlier render: in the API
	// server, not yet in the cache.
	proposed := &osv1alpha1.TalosRollout{ObjectMeta: metav1.ObjectMeta{Name: "c-config-earlier"},
		Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}, Kind: osv1alpha1.RolloutConfig}}

	cache := byCluster(fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, node, host).WithStatusSubresource(cluster, node)).Build()
	api := byCluster(fake.NewClientBuilder().WithScheme(s).WithObjects(proposed)).Build()
	r := &RolloutProposer{Client: cache, APIReader: api, Proposer: "system:serviceaccount:pk-metal:pk-metal"}
	_, err := r.Reconcile(t.Context(), requestFor("c"))
	require.NoError(t, err)

	created := &osv1alpha1.TalosRolloutList{}
	require.NoError(t, cache.List(t.Context(), created))
	assert.Empty(t, created.Items, "an unfinished rollout in the API server: nothing new is proposed")

	r.APIReader = nil
	_, err = r.Reconcile(t.Context(), requestFor("c"))
	require.NoError(t, err)
	require.NoError(t, cache.List(t.Context(), created, client.InNamespace("")))
	assert.Len(t, created.Items, 1, "from the stale cache alone, a second rollout would be proposed")
}
