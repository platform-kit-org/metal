package envtest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// The cluster, the kind and the budgets are part of the approved plan: the
// schema keeps them as they were created, defaulted or not.
func TestRolloutScopeAndBudgetsAreFrozen(t *testing.T) {
	st := startEnv(t)
	for field, change := range map[string]func(*osv1alpha1.TalosRolloutSpec){
		"clusterRef":            func(s *osv1alpha1.TalosRolloutSpec) { s.ClusterRef.Name = "d" },
		"kind":                  func(s *osv1alpha1.TalosRolloutSpec) { s.Kind = osv1alpha1.RolloutConfig },
		"maxUnavailableWorkers": func(s *osv1alpha1.TalosRolloutSpec) { s.MaxUnavailableWorkers = 2 },
		"drainTimeoutSeconds":   func(s *osv1alpha1.TalosRolloutSpec) { s.DrainTimeoutSeconds = 60 },
	} {
		t.Run(field, func(t *testing.T) {
			ctx := t.Context()
			ro := &osv1alpha1.TalosRollout{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "frozen-"},
				Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "c"},
					Kind: osv1alpha1.RolloutReboot, ProposedBy: "alice"},
			}
			require.NoError(t, st.cl.Create(ctx, ro))
			assert.Equal(t, int32(1), ro.Spec.MaxUnavailableWorkers, "the schema default")
			assert.Equal(t, int32(900), ro.Spec.DrainTimeoutSeconds, "the schema default")

			change(&ro.Spec)
			err := st.cl.Update(ctx, ro)
			require.True(t, apierrors.IsInvalid(err), "a changed %s is refused: %v", field, err)
			assert.Contains(t, err.Error(), field+" is frozen")
		})
	}
}
