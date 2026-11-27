package sim_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// The UpdateParameters part of a push, as raw JSON.
var (
	preserveParams = json.RawMessage(`{"Targets":["/redfish/v1/Managers/1"],"@Redfish.OperationApplyTime":"Immediate","Oem":{"Supermicro":{"BMC":{"PreserveCfg":true,"PreserveSdr":true,"PreserveSsl":true,"BackupBMC":true}}}}`)
	noPreserve     = json.RawMessage(`{"Targets":["/redfish/v1/Managers/1"],"@Redfish.OperationApplyTime":"Immediate"}`)
	stagedParams   = json.RawMessage(`{"Targets":["/redfish/v1/Managers/1"],"@Redfish.OperationApplyTime":"OnStartUpdateRequest","Oem":{"Supermicro":{"BMC":{"PreserveCfg":true}}}}`)
)

// TestWedgeLadder walks the recorded X12 failure ladder: a staged image wedges
// the BMC in update mode; only a forced manager reset cures it. Both roads into
// the wedge are covered — apply-time OnStartUpdateRequest and a missing
// preserve block.
func TestWedgeLadder(t *testing.T) {
	for name, params := range map[string]json.RawMessage{"staged-apply-time": stagedParams, "missing-preserve": noPreserve} {
		t.Run(name, func(t *testing.T) {
			w, _ := newWire(t, "supermicro-x12", nil)
			const push = "/redfish/v1/UpdateService/upload"
			image := []byte("SIMFW 88.00.00")
			baseReset := w.ok("/redfish/v1/Managers/1")["LastResetTime"]

			resp, _ := w.upload(push, params, image)
			require.Equal(t, http.StatusAccepted, resp.StatusCode, "the wrong invocation is still ACCEPTED — that is the trap")

			m := w.ok("/redfish/v1/Managers/1")
			assert.Equal(t, "01.07.09", m["FirmwareVersion"], "nothing flashed")
			assert.Equal(t, baseReset, m["LastResetTime"], "no BMC reboot")
			staging := w.ok("/redfish/v1/UpdateService/FirmwareInventory/Staging_BMC")
			assert.Equal(t, "88.00.00", staging["Version"], "the image landed in the staging slot")

			// Wedged: a correct upload is now refused.
			resp, _ = w.upload(push, preserveParams, image)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

			// StartUpdate cannot commit it — the recorded X12 truth.
			resp, body := w.post("/redfish/v1/UpdateService/Actions/UpdateService.StartUpdate", map[string]any{})
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, errMessage(body), "malformed JSON")

			// The OEM install answers success — and changes nothing.
			resp, _ = w.post("/redfish/v1/UpdateService/Actions/Oem/SmcUpdateService.Install",
				map[string]any{"Targets": []string{"/redfish/v1/Managers/1"}, "InstallOptions": []string{}})
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "01.07.09", w.ok("/redfish/v1/Managers/1")["FirmwareVersion"],
				"Install answered success without observable effect")

			// GracefulRestart runs the attestation path and is refused mid-update.
			resp, body = w.post("/redfish/v1/Managers/1/Actions/Manager.Reset", map[string]string{"ResetType": "GracefulRestart"})
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, errMessage(body), "FW update in progress")

			// ForceRestart discards the staging slot — the cure.
			resp, _ = w.post("/redfish/v1/Managers/1/Actions/Manager.Reset", map[string]string{"ResetType": "ForceRestart"})
			require.Equal(t, http.StatusNoContent, resp.StatusCode)
			staging = w.ok("/redfish/v1/UpdateService/FirmwareInventory/Staging_BMC")
			assert.Equal(t, "01.07.09", staging["Version"], "staging discarded, mirrors current again")

			// Unwedged: the correct recipe now flashes.
			resp, _ = w.upload(push, preserveParams, image)
			require.Equal(t, http.StatusAccepted, resp.StatusCode)
			assert.Equal(t, "88.00.00", w.ok("/redfish/v1/Managers/1")["FirmwareVersion"])
		})
	}
}

// TestFlashLyingOracle: the injected fault where a correct upload answers
// success and nothing ever happens — what the LastResetTime verify exists for.
func TestFlashLyingOracle(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	st := sim.NewState(f)
	ctx := context.Background()

	st.SetFault(sim.FaultFlashFails, true)
	require.NoError(t, st.UpdateFirmware(ctx, []byte("SIMFW 77.00.00")), "the oracle answers success")
	m, err := st.Manager(ctx)
	require.NoError(t, err)
	assert.Equal(t, "01.07.09", m.FirmwareVersion, "version never advanced")
	assert.Equal(t, f.Manager.LastResetTime.UTC(), m.LastResetTime.UTC(), "no BMC reboot")

	st.SetFault(sim.FaultFlashFails, false)
	require.NoError(t, st.UpdateFirmware(ctx, []byte("SIMFW 77.00.00")))
	m, err = st.Manager(ctx)
	require.NoError(t, err)
	assert.Equal(t, "77.00.00", m.FirmwareVersion, "without the fault the flash is real")
	assert.True(t, m.LastResetTime.After(f.Manager.LastResetTime), "the flash rebooted the BMC")
}

// TestBiosStuck: pending settings that survive every reboot — the alert-and-hold
// input for a BIOS that never converges.
func TestBiosStuck(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	st := sim.NewState(f)
	ctx := context.Background()

	require.NoError(t, st.SetBiosAttributes(ctx, "1", map[string]any{"CSMSupport": "Enabled"}))
	st.SetFault(sim.FaultBiosStuck, true)
	require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
	state, err := st.Bios(ctx, "1")
	require.NoError(t, err)
	assert.Equal(t, "Enabled", state.Pending["CSMSupport"], "pending survived the reboot")
	assert.Equal(t, "Disabled", state.Attributes["CSMSupport"], "nothing applied")

	st.SetFault(sim.FaultBiosStuck, false)
	require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
	state, err = st.Bios(ctx, "1")
	require.NoError(t, err)
	assert.Empty(t, state.Pending)
	assert.Equal(t, "Enabled", state.Attributes["CSMSupport"], "applied once the fault clears")
}

// TestBiosApplyDropsDisallowed: a staged value outside the registry's allowed
// set is dropped at apply time with a message in @Redfish.Settings — staging
// never proves convergence.
func TestBiosApplyDropsDisallowed(t *testing.T) {
	w, _ := newWire(t, "supermicro-x12", nil)

	resp, _ := w.patch("/redfish/v1/Systems/1/Bios/SD",
		map[string]any{"Attributes": map[string]any{"CSMSupport": "Bogus", "QuietBoot": false}})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "a bad VALUE stages fine — validation waits for apply")

	resp, _ = w.post("/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", map[string]string{"ResetType": "ForceRestart"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	bios := w.ok("/redfish/v1/Systems/1/Bios")
	attrs := bios["Attributes"].(map[string]any)
	assert.Equal(t, "Disabled", attrs["CSMSupport"], "the disallowed value was dropped")
	assert.Equal(t, false, attrs["QuietBoot"], "the valid one applied")
	settings := bios["@Redfish.Settings"].(map[string]any)
	msgs, ok := settings["Messages"].([]any)
	require.True(t, ok, "@Redfish.Settings must carry the apply-time messages")
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].(map[string]any)["Message"], "not in the list of acceptable values")

	sd := w.ok("/redfish/v1/Systems/1/Bios/SD")
	_, has := sd["Attributes"]
	assert.False(t, has, "pending cleared — the capture shape omits the key entirely")
}
