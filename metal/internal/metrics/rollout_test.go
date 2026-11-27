package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// A rollout that a person paused and then aborted, or whose step failed
// while it was paused, reads as aborted or failed: a resume would not help.
func TestRolloutStateEndsBeforeAPause(t *testing.T) {
	for _, reason := range []string{osv1alpha1.ReasonRolloutAborted, osv1alpha1.ReasonRolloutFailed} {
		ro := &osv1alpha1.TalosRollout{Status: osv1alpha1.TalosRolloutStatus{Conditions: []metav1.Condition{
			{Type: osv1alpha1.ConditionPaused, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonPausedByPerson,
				Message: "pk-metal rollout resume r to go on"},
			{Type: osv1alpha1.ConditionProgressing, Status: metav1.ConditionTrue, Reason: reason,
				Message: "waiting for 1 running step(s) to finish, nothing else starts"},
		}}}
		st := RolloutStateOf(ro)
		assert.Equal(t, reason, st.Reason)
		assert.Contains(t, st.Message, "waiting for 1 running step(s)")
		assert.False(t, st.Ended, "a step still runs")
	}
}
