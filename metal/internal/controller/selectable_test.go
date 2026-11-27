package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The field selectors that the reads from the API server send are
// selectable fields of the committed CRDs. A selector the CRD does not
// declare is refused, and the check that uses it stops each start.
func TestFieldSelectorsAreSelectableFields(t *testing.T) {
	for file, field := range map[string]string{
		"metal.platform-kit.org_hostoperations.yaml": opHostIndex,
		"os.platform-kit.org_talosrollouts.yaml":     rolloutClusterField,
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", file))
		require.NoError(t, err)
		crd := &apiextensionsv1.CustomResourceDefinition{}
		require.NoError(t, yaml.Unmarshal(raw, crd))
		for _, v := range crd.Spec.Versions {
			var paths []string
			for _, f := range v.SelectableFields {
				paths = append(paths, f.JSONPath)
			}
			assert.Contains(t, paths, "."+field, "%s %s", file, v.Name)
		}
	}
}
