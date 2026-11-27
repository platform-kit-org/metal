package v1alpha1

import (
	"reflect"
	"strings"
	"time"
)

// actionKeys is the JSON key of each HostOperationAction field, by field
// index. Read once: the union's shape is fixed at compile time.
var actionKeys = func() []string {
	t := reflect.TypeFor[HostOperationAction]()
	keys := make([]string, t.NumField())
	for i := range t.NumField() {
		keys[i], _, _ = strings.Cut(t.Field(i).Tag.Get("json"), ",")
	}
	return keys
}()

// Key is the union key of the action that is set (powerCycle, talosReboot,
// …): the step name the executor records, and what the CLI shows. It is
// derived from the field's JSON name, so a new action needs no second list
// to stay in step with. An action with no key set is "unknown".
func (a HostOperationAction) Key() string {
	v := reflect.ValueOf(a)
	for i := range v.NumField() {
		if !v.Field(i).IsNil() {
			return actionKeys[i]
		}
	}
	return "unknown"
}

// NodeName is the TalosNode that the set action names in its nodeRef, or ""
// for an action without one (the BMC actions). Like Key, it reads the union
// through reflection, so a new action with a nodeRef needs no second list.
func (a HostOperationAction) NodeName() string {
	v := reflect.ValueOf(a)
	for i := range v.NumField() {
		if f := v.Field(i); !f.IsNil() {
			if ref := f.Elem().FieldByName("NodeRef"); ref.IsValid() {
				return ref.Interface().(Reference).Name
			}
			return ""
		}
	}
	return ""
}

// The budgets of the steps that wait. A default is the +kubebuilder:default
// of the action's timeoutSeconds, for an action that the API server did not
// store (TestBudgetDefaultsAreTheSchemaDefaults).
const (
	// DefaultCheckInTimeout bounds awaitCheckIn.
	DefaultCheckInTimeout     = 30 * time.Minute
	defaultDrainTimeout       = 15 * time.Minute
	defaultNodeReadyTimeout   = 30 * time.Minute
	defaultEtcdHealthyTimeout = 15 * time.Minute
	defaultEtcdRecoverTimeout = 2 * time.Hour
	// talosRestartBudget bounds a reboot, a reset, a shutdown, or a config
	// apply, from the first look to the node running again.
	talosRestartBudget = 20 * time.Minute
	// talosUpgradeBudget adds the image pull and the install to a restart.
	talosUpgradeBudget = 45 * time.Minute
)

// Budget is how long the step of the set action may wait before it fails:
// its timeoutSeconds or the default of that field, or the fixed budget of a
// Talos step that restarts the node. ok is false for the other actions:
// their budget comes from the flags of the manager.
func (a HostOperationAction) Budget() (d time.Duration, ok bool) {
	secs := func(s int32, def time.Duration) (time.Duration, bool) {
		if s > 0 {
			return time.Duration(s) * time.Second, true
		}
		return def, true
	}
	switch {
	case a.AwaitCheckIn != nil:
		return secs(a.AwaitCheckIn.TimeoutSeconds, DefaultCheckInTimeout)
	case a.EtcdRecover != nil:
		return secs(a.EtcdRecover.TimeoutSeconds, defaultEtcdRecoverTimeout)
	case a.Drain != nil:
		return secs(a.Drain.TimeoutSeconds, defaultDrainTimeout)
	case a.WaitNodeReady != nil:
		return secs(a.WaitNodeReady.TimeoutSeconds, defaultNodeReadyTimeout)
	case a.WaitEtcdHealthy != nil:
		return secs(a.WaitEtcdHealthy.TimeoutSeconds, defaultEtcdHealthyTimeout)
	case a.TalosUpgrade != nil:
		return talosUpgradeBudget, true
	case a.TalosReboot != nil, a.TalosShutdown != nil, a.TalosReset != nil, a.TalosApplyConfig != nil:
		return talosRestartBudget, true
	}
	return 0, false
}

// ActionKeys lists every action key the API defines, in field order.
func ActionKeys() []string { return append([]string(nil), actionKeys...) }
