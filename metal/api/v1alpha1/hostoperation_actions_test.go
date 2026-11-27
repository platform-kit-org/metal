package v1alpha1

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
