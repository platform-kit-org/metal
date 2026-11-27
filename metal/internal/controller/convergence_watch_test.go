package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Each pass of the convergence reconciler reads the BIOS over the BMC, so an
// event that changes none of its inputs must not start one.
func TestConvergenceWatchesFilterTheirEvents(t *testing.T) {
	host := func(mut func(*metalv1alpha1.Host)) *metalv1alpha1.Host {
		h := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{
			Name: "h1", Generation: 1,
			Labels:      map[string]string{"rack": "a"},
			Annotations: map[string]string{},
		}}
		h.Status.ManagedBy = []metalv1alpha1.ManagementPath{{
			EndpointRef: metalv1alpha1.Reference{Name: "bmc-1"}, SystemID: "1", Reachable: true}}
		if mut != nil {
			mut(h)
		}
		return h
	}
	for _, tc := range []struct {
		name string
		new  func(*metalv1alpha1.Host)
		pass bool
	}{
		{"a condition this reconciler wrote", func(h *metalv1alpha1.Host) {
			h.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionBiosConverged, Status: metav1.ConditionTrue}}
		}, false},
		{"a new spec", func(h *metalv1alpha1.Host) { h.Generation = 2 }, true},
		{"a new label", func(h *metalv1alpha1.Host) { h.Labels["role"] = "worker" }, true},
		{"maintenance", func(h *metalv1alpha1.Host) { h.Annotations[metalv1alpha1.AnnotationMaintenance] = "" }, true},
		{"a path that stops answering", func(h *metalv1alpha1.Host) { h.Status.ManagedBy[0].Reachable = false }, true},
	} {
		got := convergenceHostEvents.Update(event.UpdateEvent{ObjectOld: host(nil), ObjectNew: host(tc.new)})
		assert.Equal(t, tc.pass, got, "Host: %s", tc.name)
	}

	op := func(mut func(*metalv1alpha1.HostOperation)) *metalv1alpha1.HostOperation {
		o := &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{
			Name: "h1-bios-abc", Namespace: "metal", Generation: 1, Annotations: map[string]string{}}}
		o.Status.Conditions = []metav1.Condition{
			{Type: metalv1alpha1.ConditionApproved, Status: metav1.ConditionTrue},
			{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionUnknown, Reason: metalv1alpha1.ReasonExecuting},
		}
		if mut != nil {
			mut(o)
		}
		return o
	}
	for _, tc := range []struct {
		name string
		new  func(*metalv1alpha1.HostOperation)
		pass bool
	}{
		{"a step that the executor flushed", func(o *metalv1alpha1.HostOperation) {
			o.Status.Steps = append(o.Status.Steps, metalv1alpha1.OperationStep{Action: "powerCycle", Phase: "Succeeded"})
			o.Status.Conditions[1].Message = "step 2 of 5"
		}, false},
		{"an approval", func(o *metalv1alpha1.HostOperation) {
			o.Generation = 2
			o.Spec.Approval.ApprovedBy = "alice"
		}, true},
		{"the retry mark", func(o *metalv1alpha1.HostOperation) {
			o.Annotations[metalv1alpha1.AnnotationRetry] = "2026-01-01T00:00:00Z"
		}, true},
		{"the end of the run", func(o *metalv1alpha1.HostOperation) {
			o.Status.Conditions[1].Status = metav1.ConditionFalse
		}, true},
		{"an approval taken back", func(o *metalv1alpha1.HostOperation) {
			o.Status.Conditions[0].Status = metav1.ConditionFalse
		}, true},
	} {
		got := convergenceOperationEvents.Update(event.UpdateEvent{ObjectOld: op(nil), ObjectNew: op(tc.new)})
		assert.Equal(t, tc.pass, got, "HostOperation: %s", tc.name)
	}
	assert.True(t, convergenceOperationEvents.Create(event.CreateEvent{Object: op(nil)}), "a new operation")
	assert.True(t, convergenceOperationEvents.Delete(event.DeleteEvent{Object: op(nil)}), "a deleted operation")
}
