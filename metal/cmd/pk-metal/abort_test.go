package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestAbort(t *testing.T) {
	ctx := context.Background()
	op, objs := bootPlan()
	cl := fakeClient(objs...)
	var out bytes.Buffer

	require.NoError(t, abort(ctx, cl, op.Namespace, op.Name, "alice", "wrong host", &out))
	got := fetch(t, cl, op)
	assert.Equal(t, "alice: wrong host", got.Annotations[metalv1alpha1.AnnotationAbort])
	assert.Contains(t, out.String(), "has not started, so it ends now")

	out.Reset()
	require.NoError(t, abort(ctx, cl, op.Namespace, op.Name, "bob", "also wrong", &out))
	assert.Contains(t, out.String(), "already being aborted (alice: wrong host)")

	got.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionTrue,
		Reason: metalv1alpha1.ReasonStepsSucceeded, LastTransitionTime: metav1.Now()}}
	done := fakeClient(append([]client.Object{got}, objs[1:]...)...)
	assert.ErrorContains(t, abort(ctx, done, op.Namespace, op.Name, "alice", "late", &out), "has finished (Succeeded)")
}

// An abort names who asked and why: admission accepts no other form.
func TestAbortNeedsAReason(t *testing.T) {
	ctx := context.Background()
	op, objs := bootPlan()
	cl := fakeClient(objs...)
	var out bytes.Buffer
	assert.ErrorContains(t, abort(ctx, cl, op.Namespace, op.Name, "alice", "  ", &out), "say why")
	got := fetch(t, cl, op)
	assert.NotContains(t, got.Annotations, metalv1alpha1.AnnotationAbort, "nothing was written")
}
