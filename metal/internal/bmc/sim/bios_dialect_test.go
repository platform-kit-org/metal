package sim_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// TestBiosApplyTimeDialect pins the iDRAC 9 staging shape the sim plays with
// apply_time_required: a PATCH without @Redfish.SettingsApplyTime stages but
// is never applied, one asking for OnReset is applied at the next reset, and
// an apply time the service does not list is refused.
func TestBiosApplyTimeDialect(t *testing.T) {
	ctx := t.Context()
	setup := func(t *testing.T) (*wire, *sim.State) {
		return newWire(t, "supermicro-x12", func(f *sim.Fixture) {
			f.Bios.ApplyTimeRequired = true
			f.Bios.SupportedApplyTimes = []string{"OnReset", "AtMaintenanceWindowStart", "InMaintenanceWindowOnReset"}
		})
	}
	patch := func(w *wire, body map[string]any) int {
		resp, _ := w.patch("/redfish/v1/Systems/1/Bios/SD", body)
		return resp.StatusCode
	}
	attrs := map[string]any{"QuietBoot": false}

	t.Run("without an apply time nothing applies", func(t *testing.T) {
		w, st := setup(t)
		require.Less(t, patch(w, map[string]any{"Attributes": attrs}), 300)
		require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
		state, err := st.Bios(ctx, "1")
		require.NoError(t, err)
		assert.Equal(t, true, state.Attributes["QuietBoot"], "no job, no change")
		assert.Equal(t, false, state.Pending["QuietBoot"], "still pending after the reset")

		// A later PATCH that asks for OnReset schedules the whole pending set.
		require.Less(t, patch(w, map[string]any{"Attributes": map[string]any{"CSMSupport": "Disabled"},
			"@Redfish.SettingsApplyTime": map[string]any{"ApplyTime": "OnReset"}}), 300)
		require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
		state, err = st.Bios(ctx, "1")
		require.NoError(t, err)
		assert.Equal(t, false, state.Attributes["QuietBoot"])
		assert.Empty(t, state.Pending)
	})

	t.Run("OnReset applies at the next reset", func(t *testing.T) {
		w, st := setup(t)
		require.Less(t, patch(w, map[string]any{"Attributes": attrs,
			"@Redfish.SettingsApplyTime": map[string]any{"ApplyTime": "OnReset"}}), 300)
		require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
		state, err := st.Bios(ctx, "1")
		require.NoError(t, err)
		assert.Equal(t, false, state.Attributes["QuietBoot"])
	})

	t.Run("an apply time not offered is refused", func(t *testing.T) {
		w, _ := setup(t)
		assert.Equal(t, http.StatusBadRequest, patch(w, map[string]any{"Attributes": attrs,
			"@Redfish.SettingsApplyTime": map[string]any{"ApplyTime": "Immediate"}}))
	})
}
