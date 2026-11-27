package envtest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The checks that read from the API server before an operation or a
// rollout starts ask it for one host's operations and one cluster's
// rollouts. The CRDs declare these selectable fields; without them the API
// server refuses the selector and nothing starts.
func TestAPIServerFiltersByHostAndCluster(t *testing.T) {
	st := startEnv(t)
	cl, ctx := st.cl, t.Context()

	for _, o := range []struct{ name, host string }{{"on-a", "a"}, {"on-b-1", "b"}, {"on-b-2", "b"}} {
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: o.name, Namespace: "default"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef: metalv1alpha1.Reference{Name: o.host}, ProposedBy: "alice",
				Actions: []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
			},
		}))
	}
	ops := &metalv1alpha1.HostOperationList{}
	require.NoError(t, cl.List(ctx, ops, client.MatchingFields{"spec.hostRef.name": "b"}))
	var names []string
	for _, o := range ops.Items {
		names = append(names, o.Name)
	}
	assert.ElementsMatch(t, []string{"on-b-1", "on-b-2"}, names)

	for _, r := range []struct{ name, cluster string }{{"ro-x", "x"}, {"ro-y", "y"}} {
		require.NoError(t, cl.Create(ctx, &osv1alpha1.TalosRollout{
			ObjectMeta: metav1.ObjectMeta{Name: r.name},
			Spec: osv1alpha1.TalosRolloutSpec{
				ClusterRef: osv1alpha1.Reference{Name: r.cluster}, Kind: osv1alpha1.RolloutReboot,
				Nodes: []string{"n"}, ProposedBy: "alice",
			},
		}))
	}
	rollouts := &osv1alpha1.TalosRolloutList{}
	require.NoError(t, cl.List(ctx, rollouts, client.MatchingFields{"spec.clusterRef.name": "y"}))
	require.Len(t, rollouts.Items, 1)
	assert.Equal(t, "ro-y", rollouts.Items[0].Name)

	err := cl.List(ctx, &metalv1alpha1.HostList{}, client.MatchingFields{"spec.claimRef.name": "c"})
	require.Error(t, err, "a field the CRD does not declare is refused")
}
