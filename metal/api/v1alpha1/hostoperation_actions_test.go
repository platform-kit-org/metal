package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// Every union member is an optional pointer with its own JSON key, which is
// what Key relies on; and Key names the member that is set.
func TestActionKey(t *testing.T) {
	typ := reflect.TypeFor[HostOperationAction]()
	seen := map[string]bool{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		require.Equal(t, reflect.Pointer, f.Type.Kind(), "%s is a pointer", f.Name)
		key := ActionKeys()[i]
		require.NotEmpty(t, key, "%s has a json name", f.Name)
		require.False(t, seen[key], "%s is unique", key)
		seen[key] = true

		var a HostOperationAction
		reflect.ValueOf(&a).Elem().Field(i).Set(reflect.New(f.Type.Elem()))
		assert.Equal(t, key, a.Key())
	}
	assert.Equal(t, "unknown", HostOperationAction{}.Key())
	assert.Equal(t, "talosReboot", HostOperationAction{TalosReboot: &TalosRebootAction{}}.Key())
}

// NodeName reads the nodeRef of every action that has one. Only the BMC
// actions name no node.
func TestActionNodeName(t *testing.T) {
	typ := reflect.TypeFor[HostOperationAction]()
	var none []string
	for i := range typ.NumField() {
		var a HostOperationAction
		member := reflect.New(typ.Field(i).Type.Elem())
		if ref := member.Elem().FieldByName("NodeRef"); ref.IsValid() {
			ref.Set(reflect.ValueOf(Reference{Name: "cp-1"}))
		}
		reflect.ValueOf(&a).Elem().Field(i).Set(member)
		switch a.NodeName() {
		case "cp-1":
		case "":
			none = append(none, a.Key())
		default:
			t.Errorf("%s names node %q", a.Key(), a.NodeName())
		}
	}
	assert.ElementsMatch(t, []string{"powerCycle", "powerOff", "setBootOverride", "insertMedia", "ejectMedia",
		"applyBios", "flashFirmware", "stageFirmware"}, none)
	assert.Empty(t, HostOperationAction{}.NodeName())
	assert.Equal(t, "cp-2", HostOperationAction{EtcdRecover: &EtcdRecoverAction{NodeRef: Reference{Name: "cp-2"}}}.NodeName())
}

// An action that the API server did not store (a plan that pk-metal shows
// before it proposes) gets the budget that the schema default gives a stored
// one.
func TestBudgetDefaultsAreTheSchemaDefaults(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "metal.platform-kit.org_hostoperations.yaml"))
	require.NoError(t, err)
	crd := &apiextensionsv1.CustomResourceDefinition{}
	require.NoError(t, yaml.Unmarshal(raw, crd))
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["actions"].Items.Schema.Properties
	typ := reflect.TypeFor[HostOperationAction]()
	var checked []string
	for i := range typ.NumField() {
		key := ActionKeys()[i]
		timeout, ok := schema[key].Properties["timeoutSeconds"]
		if !ok || timeout.Default == nil {
			continue
		}
		var def int64
		require.NoError(t, json.Unmarshal(timeout.Default.Raw, &def))
		var a HostOperationAction
		reflect.ValueOf(&a).Elem().Field(i).Set(reflect.New(typ.Field(i).Type.Elem()))
		got, ok := a.Budget()
		assert.True(t, ok, key)
		assert.Equal(t, time.Duration(def)*time.Second, got, key)
		checked = append(checked, key)
	}
	assert.ElementsMatch(t, []string{"awaitCheckIn", "drain", "waitNodeReady", "waitEtcdHealthy", "etcdRecover"}, checked)

	got, ok := HostOperationAction{Drain: &DrainAction{TimeoutSeconds: 60}}.Budget()
	assert.True(t, ok)
	assert.Equal(t, time.Minute, got, "a timeout the action names wins")
	_, ok = HostOperationAction{PowerCycle: &PowerCycleAction{}}.Budget()
	assert.False(t, ok, "the manager's flags budget the BMC actions")
}
