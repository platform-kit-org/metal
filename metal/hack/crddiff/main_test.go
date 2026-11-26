package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func crd(spec apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.org"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.org", Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Widget", Plural: "widgets"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": spec,
						"status": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"phase": {Type: "string"}, "managerType": {Type: "string"}}},
					},
				}},
			}},
		},
	}
}

func obj(props map[string]apiextensionsv1.JSONSchemaProps, required ...string) apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{Type: "object", Properties: props, Required: required}
}

func json(s string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(s)} }

func one(old, cur *apiextensionsv1.CustomResourceDefinition) []Finding {
	return Compare(map[string]*apiextensionsv1.CustomResourceDefinition{old.Name: old},
		map[string]*apiextensionsv1.CustomResourceDefinition{cur.Name: cur})
}

func TestNoChangeNoFinding(t *testing.T) {
	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{"size": {Type: "integer"}})
	assert.Empty(t, one(crd(spec), crd(spec)))
}

func TestFindings(t *testing.T) {
	base := map[string]apiextensionsv1.JSONSchemaProps{
		"mode":  {Type: "string", Enum: []apiextensionsv1.JSON{*json(`"A"`), *json(`"B"`)}},
		"name":  {Type: "string", MaxLength: ptr.To[int64](63)},
		"size":  {Type: "integer"},
		"old":   {Type: "string"},
		"limit": {Type: "integer", Default: json(`3`)},
		"items": {Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{
			Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"port": {Type: "integer"}}}}},
	}
	old := crd(obj(base))

	next := map[string]apiextensionsv1.JSONSchemaProps{}
	for k, v := range base {
		next[k] = v
	}
	delete(next, "old")
	next["mode"] = apiextensionsv1.JSONSchemaProps{Type: "string", Enum: []apiextensionsv1.JSON{*json(`"A"`), *json(`"C"`)}}
	next["name"] = apiextensionsv1.JSONSchemaProps{Type: "string", MaxLength: ptr.To[int64](32)}
	next["size"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	next["limit"] = apiextensionsv1.JSONSchemaProps{Type: "integer", Default: json(`5`)}
	next["owner"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	next["zone"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	next["items"] = apiextensionsv1.JSONSchemaProps{Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{
		Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"port": {Type: "integer",
			XValidations: apiextensionsv1.ValidationRules{{Rule: "self > 0"}}}}}}}
	cur := crd(obj(next, "zone"))
	st := cur.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	delete(st.Properties, "managerType")
	cur.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = st

	got := map[string]Severity{}
	for _, f := range one(old, cur) {
		got[f.Path+": "+f.What] = f.Severity
	}
	assert.Equal(t, map[string]Severity{
		`spec.items[].port: new rule: self > 0`:                             Breaking,
		`spec.limit: default 3 becomes 5`:                                   Notice,
		`spec.mode: enum gains "C"`:                                         Added,
		`spec.mode: enum loses "B"`:                                         Breaking,
		`spec.name: maxLength 63 becomes 32`:                                Breaking,
		`spec.old: removed: a manifest that sets it is refused or loses it`: Breaking,
		`spec.owner: new field`:                                             Added,
		`spec.size: type "integer" becomes "string"`:                        Breaking,
		`spec.zone: new required field without a default: each update of a stored object fails until it is set`: Breaking,
		`status.managerType: removed from the status: the API server drops the stored value at the next write`:  Notice,
	}, got)
}

func TestKindsAndVersions(t *testing.T) {
	spec := obj(nil)
	old, cur := crd(spec), crd(spec)
	cur.Spec.Versions[0].Served = false
	cur.Spec.Versions = append(cur.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1beta1", Served: true})
	f := one(old, cur)
	require.Len(t, f, 2)
	assert.Equal(t, Finding{Kind: "Widget/v1alpha1", Severity: Breaking, What: "the version is no longer served"}, f[0])
	assert.Equal(t, Finding{Kind: "Widget/v1beta1", Severity: Added, What: "new version"}, f[1])

	gone := Compare(map[string]*apiextensionsv1.CustomResourceDefinition{old.Name: old}, nil)
	require.Len(t, gone, 1)
	assert.Equal(t, Breaking, gone[0].Severity)
}

// The committed CRDs read, and compare equal to themselves.
func TestReadsTheCommittedCRDs(t *testing.T) {
	dir := filepath.Join("..", "..", "config", "crd")
	crds, err := readDir(dir)
	require.NoError(t, err)
	assert.Len(t, crds, 17)
	assert.Empty(t, Compare(crds, crds))

	empty, err := readDir(filepath.Join(t.TempDir(), "none"))
	require.NoError(t, err)
	var out bytes.Buffer
	report(&out, Compare(empty, crds))
	assert.Contains(t, out.String(), "crddiff: 0 breaking, 0 notice, 17 added")
}

func TestReadDirSkipsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	raw, err := yaml.Marshal(crd(obj(nil)))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "example.org_widgets.yaml"), raw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("resources: []\n"), 0o600))
	crds, err := readDir(dir)
	require.NoError(t, err)
	assert.Len(t, crds, 1)
}
