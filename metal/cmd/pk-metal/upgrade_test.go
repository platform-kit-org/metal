package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestUpgrade(t *testing.T) {
	ctx := context.Background()
	cluster := func() *osv1alpha1.TalosCluster {
		return &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab"},
			Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2"}}
	}
	t.Run("sets the spec and shows the proposed plan", func(t *testing.T) {
		cl := fakeClientAs("bob", cluster(), planned())
		var out bytes.Buffer
		require.NoError(t, upgrade(ctx, cl, "talos", "lab", "v1.14.7", time.Second, time.Millisecond, &out))
		c := fetch(t, cl, cluster())
		assert.Equal(t, "v1.14.7", c.Spec.TalosVersion)
		assert.Equal(t, "bob", c.Annotations[metalv1alpha1.AnnotationRequestedBy], "the upgrade names who asked for it")
		assert.Contains(t, out.String(), "lab-talosupgrade-1a2b3c4d")
		assert.Contains(t, out.String(), "pk-metal rollout approve")
	})
	t.Run("refuses a downgrade and a bad version", func(t *testing.T) {
		cl := fakeClient(cluster())
		require.ErrorContains(t, upgrade(ctx, cl, "talos", "lab", "v1.14.5", 0, time.Millisecond, &bytes.Buffer{}), "not an upgrade")
		require.ErrorContains(t, upgrade(ctx, cl, "kubernetes", "lab", "1.36", 0, time.Millisecond, &bytes.Buffer{}), "v1.2.3")
		c := fetch(t, cl, cluster())
		assert.Equal(t, "v1.14.6", c.Spec.TalosVersion)
		assert.Equal(t, "v1.36.2", c.Spec.KubernetesVersion)
	})
	t.Run("times out when nothing is proposed", func(t *testing.T) {
		cl := fakeClientAs("bob", cluster())
		var out bytes.Buffer
		err := upgrade(ctx, cl, "kubernetes", "lab", "v1.36.3", 5*time.Millisecond, time.Millisecond, &out)
		require.ErrorContains(t, err, "no rollout planned")
		assert.Equal(t, 1, strings.Count(out.String(), "waiting up to"), "the wait is said once, so it does not look like a hang")
	})
	t.Run("refuses a skipped Kubernetes minor", func(t *testing.T) {
		cl := fakeClient(cluster())
		err := upgrade(ctx, cl, "kubernetes", "lab", "v1.38.0", 0, time.Millisecond, &bytes.Buffer{})
		require.ErrorContains(t, err, "one minor at a time")
		c := fetch(t, cl, cluster())
		assert.Equal(t, "v1.36.2", c.Spec.KubernetesVersion, "nothing changed")

		// The spec already asks for v1.36, but the API server still runs
		// v1.35: v1.37 would skip a minor on the machines.
		behind := cluster()
		behind.Status.KubernetesVersion = "v1.35.2"
		cl = fakeClient(behind)
		err = upgrade(ctx, cl, "kubernetes", "lab", "v1.37.1", 0, time.Millisecond, &bytes.Buffer{})
		require.ErrorContains(t, err, "one minor at a time")
	})
}
