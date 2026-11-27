// Package uncached is the fixture of TestDecisionsReadTheAPIServer. Each
// line that ends in a "want" comment is a finding; no other line is.
package uncached

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type reconciler struct {
	client.Client
	APIReader client.Reader
}

func (r *reconciler) reader() client.Reader { return r.APIReader }

// fresh reads the gate of a decision.
//
//pk:uncached
func (r *reconciler) fresh(ctx context.Context) {
	_ = r.reader().List(ctx, &corev1.PodList{})
	_ = r.APIReader.Get(ctx, client.ObjectKey{}, &corev1.Pod{})
	reader := client.Reader(r.Client)
	_ = reader.List(ctx, &corev1.PodList{})
	_ = r.List(ctx, &corev1.PodList{})                       // want cached
	_ = r.Client.Get(ctx, client.ObjectKey{}, &corev1.Pod{}) // want cached
	var c client.Client = r.Client
	func() {
		_ = c.List(ctx, &corev1.PodList{}) // want cached
	}()
	_ = r.Update(ctx, &corev1.Pod{})
}

// unmarked decides nothing that stays.
func (r *reconciler) unmarked(ctx context.Context) {
	_ = r.List(ctx, &corev1.PodList{})
}

func (r *reconciler) release(obj client.Object) { // want unmarked
	controllerutil.RemoveFinalizer(obj, "f")
}

// released says why its inputs are fresh.
//
//pk:cached-ok the update carries the object's resourceVersion
func (r *reconciler) released(obj client.Object) {
	controllerutil.RemoveFinalizer(obj, "f")
}

// releasedFresh reads its gate from the API server.
//
//pk:uncached
func (r *reconciler) releasedFresh(ctx context.Context, obj client.Object) {
	_ = r.reader().List(ctx, &corev1.PodList{})
	controllerutil.RemoveFinalizer(obj, "f")
}

// bare names no reason.
//
//pk:cached-ok
func (r *reconciler) bare(obj client.Object) { // want unmarked
	controllerutil.RemoveFinalizer(obj, "f")
}
