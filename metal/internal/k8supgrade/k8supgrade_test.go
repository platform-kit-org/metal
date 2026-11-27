package k8supgrade

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func kubeProxy(image string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system", Generation: 1},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "kube-proxy", Image: image, Args: []string{"--keep-me"}},
		}}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 3},
	}
}

func TestSyncKubeProxyMovesOnlyTheImage(t *testing.T) {
	c := fake.NewClientBuilder().WithObjects(kubeProxy("registry.k8s.io/kube-proxy:v1.36.1")).Build()
	_, _, err := SyncKubeProxy(t.Context(), c, "registry.k8s.io/kube-proxy:v1.36.2")
	require.NoError(t, err)
	ds := &appsv1.DaemonSet{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: "kube-system", Name: "kube-proxy"}, ds))
	ctr := ds.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "registry.k8s.io/kube-proxy:v1.36.2", ctr.Image)
	assert.Equal(t, []string{"--keep-me"}, ctr.Args, "nothing else of the DaemonSet changes")
}

func TestSyncKubeProxyWithoutKubeProxy(t *testing.T) {
	done, msg, err := SyncKubeProxy(t.Context(), fake.NewClientBuilder().Build(), "x")
	require.NoError(t, err)
	assert.True(t, done)
	assert.Contains(t, msg, "no kube-proxy")
}

func TestRolledOut(t *testing.T) {
	ds := kubeProxy("x")
	done, _ := rolledOut(ds)
	assert.True(t, done)
	ds.Status.UpdatedNumberScheduled = 1
	done, msg := rolledOut(ds)
	assert.False(t, done)
	assert.Contains(t, msg, "1 of 3")
	ds.Status.ObservedGeneration = 0
	done, _ = rolledOut(ds)
	assert.False(t, done)
}

func TestNodesReady(t *testing.T) {
	node := func(name string, s corev1.ConditionStatus) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: s}}}}
	}
	c := fake.NewClientBuilder().WithObjects(node("a", corev1.ConditionTrue), node("b", corev1.ConditionUnknown)).Build()
	ready, err := NodesReady(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"a": true, "b": false}, ready)
}
