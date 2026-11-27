package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

func TestVerifiedCondition(t *testing.T) {
	snap := &metalv1alpha1.AssetSnapshot{Serial: "SIMX12B000001", BMCMac: "02:00:5a:3a:cc:18"}
	tests := []struct {
		name    string
		snap    *metalv1alpha1.AssetSnapshot
		mac     string
		anchors []string
		status  metav1.ConditionStatus
		reason  string
	}{
		{name: "board serial anchors (Supermicro: system serial empty)",
			snap: snap, mac: "02:00:5a:3a:cc:18", anchors: []string{"SIMX12B000001"},
			status: metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "system serial anchors (Huawei)",
			snap:    &metalv1alpha1.AssetSnapshot{Serial: "SIMHW2288V5000391"},
			anchors: []string{"SIMHW2288V5000391", "SIMHW2288V5000391"},
			status:  metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "serial mismatch",
			snap: snap, mac: "02:00:5a:3a:cc:18", anchors: []string{"OTHER"},
			status: metav1.ConditionFalse, reason: metalv1alpha1.ReasonSerialMismatch},
		{name: "mac mismatch outranks serial (wrong box behind the address)",
			snap: snap, mac: "02:00:48:0b:74:83", anchors: []string{"SIMX12B000001"},
			status: metav1.ConditionFalse, reason: metalv1alpha1.ReasonMACMismatch},
		{name: "mac compare is case-insensitive",
			snap: snap, mac: "02:00:5A:3A:CC:18", anchors: []string{"SIMX12B000001"},
			status: metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "an asserted mac in dashed upper case matches",
			snap: &metalv1alpha1.AssetSnapshot{Serial: "SIMX12B000001", BMCMac: "02-00-5A-3A-CC-18"},
			mac:  "02:00:5a:3a:cc:18", anchors: []string{"SIMX12B000001"},
			status: metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "no asserted mac skips the mac check",
			snap: &metalv1alpha1.AssetSnapshot{Serial: "SIMX12B000001"}, mac: "02:00:48:0b:74:83",
			anchors: []string{"SIMX12B000001"},
			status:  metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "no anchors cannot verify",
			snap: snap, mac: "02:00:5a:3a:cc:18",
			status: metav1.ConditionUnknown, reason: "NoSerialAnchor"},
		{name: "no snapshot cannot verify",
			status: metav1.ConditionUnknown, reason: "NoAssertion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, _ := verifiedCondition(tt.snap, tt.mac, tt.anchors)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.reason, reason)
		})
	}
}

func TestSerialAnchors(t *testing.T) {
	anchors := serialAnchors(
		bmc.Chassis{BoardSerial: "SIMX12B000001"},
		[]bmc.System{{ID: "1", SerialNumber: ""}, {ID: "2", SerialNumber: "S2"}},
	)
	assert.Equal(t, []string{"SIMX12B000001", "S2"}, anchors, "empty system serials are dropped")
}

func TestEndpointCapabilities(t *testing.T) {
	x12 := endpointCapabilities(bmc.Capabilities{}, []bmc.VirtualMedia{
		{ID: "VirtualMedia1", Dialect: bmc.DialectStandard},
		{ID: "VirtualMedia2", Dialect: bmc.DialectStandard},
	})
	assert.True(t, x12.DualMedia)
	assert.Equal(t, "insert-media", x12.MediaDialects["VirtualMedia1"])

	ibmc := endpointCapabilities(bmc.Capabilities{}, []bmc.VirtualMedia{
		{ID: "CD", Dialect: bmc.DialectVmmControl},
		{ID: "USBStick", Dialect: bmc.DialectNone},
	})
	assert.False(t, ibmc.DualMedia, "a slot with no insert mechanism does not count")
	assert.Equal(t, "vmm-control", ibmc.MediaDialects["CD"])
}

// An endpoint whose media slots do not read is still inventoried: the slots
// only shape boot plans. It offers no slot to boot from.
func TestDiscoverWithUnreadableMedia(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	next := sim.NewServer(sim.NewState(f))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/VirtualMedia/VirtualMedia2") {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cl, err := gofishclient.Connect(t.Context(), gofishclient.Config{Endpoint: srv.URL, ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	d, err := discover(t.Context(), cl)
	require.NoError(t, err, "the endpoint is inventoried")
	assert.Empty(t, discoveredMedia(d.media))
	assert.Empty(t, endpointCapabilities(d.caps, d.media).MediaDialects)
	assert.Equal(t, f.Manager.Firmware, d.mgr.FirmwareVersion)
}
