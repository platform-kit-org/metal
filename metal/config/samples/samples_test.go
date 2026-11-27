// Package samples_test keeps each sample in step with the API types: a field
// that the API does not have fails the test.
package samples_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// decode reads the one object of a sample file into out and refuses a field
// that the type does not know.
func decode(t *testing.T, file string, out any) {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.False(t, bytes.Contains(data, []byte("\n---\n")), "%s holds one object", file)
	require.NoError(t, yaml.UnmarshalStrict(data, out), file)
}

// TestEverySampleDecodesStrictly reads each object of each sample file into
// the type of its kind, so the samples that the kind pages show stay valid.
func TestEverySampleDecodesStrictly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, metalv1alpha1.AddToScheme(scheme))
	require.NoError(t, osv1alpha1.AddToScheme(scheme))
	files, err := filepath.Glob("*.yaml")
	require.NoError(t, err)
	for _, file := range files {
		if file == "kustomization.yaml" {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		for i, doc := range bytes.Split(data, []byte("\n---\n")) {
			var tm metav1.TypeMeta
			require.NoError(t, yaml.Unmarshal(doc, &tm), "%s object %d", file, i)
			obj, err := scheme.New(tm.GroupVersionKind())
			require.NoError(t, err, "%s object %d is a kind of the API", file, i)
			require.NoError(t, yaml.UnmarshalStrict(doc, obj), "%s object %d", file, i)
		}
	}
}

func TestClaimSample(t *testing.T) {
	claim := &metalv1alpha1.HostClaim{}
	decode(t, "claim.yaml", claim)
	require.Equal(t, "HostClaim", claim.Kind)
	asset := &metalv1alpha1.Asset{}
	decode(t, "asset.yaml", asset)
	require.Equal(t, asset.Name, claim.Spec.AssetRef.Name, "the claim names the Asset in asset.yaml")
	require.NotNil(t, claim.Spec.OS, "the sample installs a node")
	require.Equal(t, "node-1", claim.Spec.OS.NodeRef.Name, "the node is in talos.yaml")
}

func TestWindowSample(t *testing.T) {
	window := &metalv1alpha1.MaintenanceWindow{}
	decode(t, "window.yaml", window)
	require.Equal(t, "MaintenanceWindow", window.Kind)
	require.NotNil(t, window.Spec.HostSelector)
	require.NotEmpty(t, window.Spec.Weekly)
}

func TestRebootRolloutSample(t *testing.T) {
	rollout := &osv1alpha1.TalosRollout{}
	decode(t, "rollout-reboot.yaml", rollout)
	require.Equal(t, osv1alpha1.RolloutReboot, rollout.Spec.Kind)
	require.Nil(t, rollout.Spec.Target, "a Reboot rollout takes no target")
	require.Empty(t, rollout.Spec.Approval.ApprovedBy, "a rollout cannot be created pre-approved")
	require.NotEmpty(t, rollout.Spec.ProposedBy)
}
