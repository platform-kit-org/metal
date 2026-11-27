package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// A deleting endpoint fronts two hosts. One of them was just bound to a
// claim, and the cache has not seen that yet. A pass for the other host
// must not take the protection off: the endpoint would go at once, and the
// release of the claim could no longer power the machine off through it.
func TestProtectionReadsTheHostsBeforeAnEndpointGoes(t *testing.T) {
	behind := func(name string, claim *metalv1alpha1.ClaimReference) *metalv1alpha1.Host {
		h := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{claimProtection}}}
		h.Spec.ClaimRef = claim
		h.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "bmc"}}}
		return h
	}
	now := metav1.Now()
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{
		Name: "bmc", Finalizers: []string{claimProtection}, DeletionTimestamp: &now,
	}}
	free := behind("free", nil)
	free.Finalizers = nil
	bound := behind("bound", &metalv1alpha1.ClaimReference{Namespace: "team", Name: "c"})
	unseen := behind("bound", nil)

	store := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ep, free, bound).Build()
	cache := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(free.DeepCopy(), unseen).Build()
	r := &ProtectionReconciler{Client: &staleLists{Client: store, cache: cache}, APIReader: store}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(free)})
	require.NoError(t, err)
	got := &metalv1alpha1.ManagementEndpoint{}
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(ep), got), "the endpoint is still there")
	assert.Contains(t, got.Finalizers, claimProtection)

	// Once the API server says that no host behind it is claimed, it goes.
	bound.Spec.ClaimRef = nil
	require.NoError(t, store.Update(t.Context(), bound))
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(free)})
	require.NoError(t, err)
	assert.Error(t, store.Get(t.Context(), client.ObjectKeyFromObject(ep), got), "the endpoint is gone")
}
