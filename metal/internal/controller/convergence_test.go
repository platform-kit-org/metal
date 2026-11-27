package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

func biosProfile(name string, layer int32, attrs map[string]apiextensionsv1.JSON) metalv1alpha1.BiosProfile {
	return metalv1alpha1.BiosProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       metalv1alpha1.BiosProfileSpec{Layer: layer, Attributes: attrs},
	}
}

func TestMergeBios(t *testing.T) {
	t.Run("layered override with provenance", func(t *testing.T) {
		attrs, prov, conflicts := mergeBios([]metalv1alpha1.BiosProfile{
			biosProfile("role-worker", 10, map[string]apiextensionsv1.JSON{
				"CSMSupport": jsonOf("Enabled"),
			}),
			biosProfile("baseline-x12", 0, map[string]apiextensionsv1.JSON{
				"CSMSupport": jsonOf("Disabled"),
				"QuietBoot":  jsonOf(true),
			}),
		})
		require.Empty(t, conflicts)
		assert.Equal(t, "Enabled", jsonAny(attrs["CSMSupport"]), "the later layer overrides")
		assert.Equal(t, true, jsonAny(attrs["QuietBoot"]), "disjoint keys union")
		assert.Equal(t, map[string]string{"CSMSupport": "role-worker", "QuietBoot": "baseline-x12"}, prov)
	})

	t.Run("same layer, same value is no conflict", func(t *testing.T) {
		_, prov, conflicts := mergeBios([]metalv1alpha1.BiosProfile{
			biosProfile("b", 0, map[string]apiextensionsv1.JSON{"QuietBoot": jsonOf(true)}),
			biosProfile("a", 0, map[string]apiextensionsv1.JSON{"QuietBoot": jsonOf(true)}),
		})
		assert.Empty(t, conflicts)
		assert.Equal(t, "b", prov["QuietBoot"], "deterministic: the merge-order winner is recorded")
	})

	t.Run("same layer, different value conflicts", func(t *testing.T) {
		_, _, conflicts := mergeBios([]metalv1alpha1.BiosProfile{
			biosProfile("a", 0, map[string]apiextensionsv1.JSON{"BootModeSelect": jsonOf("UEFI")}),
			biosProfile("b", 0, map[string]apiextensionsv1.JSON{"BootModeSelect": jsonOf("Dual")}),
		})
		require.Len(t, conflicts, 1)
		assert.Contains(t, conflicts[0], "BootModeSelect")
		assert.Contains(t, conflicts[0], "a")
		assert.Contains(t, conflicts[0], "b")
	})
}

func TestMergeFirmware(t *testing.T) {
	fw := func(name string, layer int32, version string) metalv1alpha1.FirmwareProfile {
		return metalv1alpha1.FirmwareProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: metalv1alpha1.FirmwareProfileSpec{
				Layer:   layer,
				Manager: &metalv1alpha1.FirmwareTarget{Version: version, Image: "r/f@sha256:aa"},
			},
		}
	}
	t.Run("later layer overrides whole", func(t *testing.T) {
		target, conflict := mergeFirmware([]metalv1alpha1.FirmwareProfile{
			fw("baseline", 0, "01.05.25"), fw("hotfix", 10, "01.07.09"),
		})
		require.Empty(t, conflict)
		assert.Equal(t, "01.07.09", target.Version)
	})
	t.Run("same-layer disagreement conflicts", func(t *testing.T) {
		_, conflict := mergeFirmware([]metalv1alpha1.FirmwareProfile{
			fw("a", 0, "01.05.25"), fw("b", 0, "01.07.09"),
		})
		assert.Contains(t, conflict, "disagree")
	})
}

func TestValidateBios(t *testing.T) {
	schema := []bmc.BiosAttribute{
		{Name: "BootModeSelect", Type: "Enumeration", AllowedValues: []string{"Legacy", "UEFI", "Dual"}},
		{Name: "QuietBoot", Type: "Boolean"},
		{Name: "CorrectableErrorThreshold", Type: "Integer"},
	}
	t.Run("valid values pass", func(t *testing.T) {
		assert.Empty(t, validateBios(map[string]apiextensionsv1.JSON{
			"BootModeSelect":            jsonOf("UEFI"),
			"QuietBoot":                 jsonOf(false),
			"CorrectableErrorThreshold": jsonOf(512),
		}, schema))
	})
	t.Run("violations are loud and sorted", func(t *testing.T) {
		got := validateBios(map[string]apiextensionsv1.JSON{
			"BootModeSelect": jsonOf("Insecure"),
			"NoSuchKnob":     jsonOf("x"),
			"QuietBoot":      jsonOf("yes"),
		}, schema)
		require.Len(t, got, 3)
		assert.Contains(t, got[0], "BootModeSelect")
		assert.Contains(t, got[0], "allowed values")
		assert.Contains(t, got[1], "NoSuchKnob")
		assert.Contains(t, got[1], "not in the attribute registry")
		assert.Contains(t, got[2], "QuietBoot")
	})
	t.Run("fractional integer refused", func(t *testing.T) {
		got := validateBios(map[string]apiextensionsv1.JSON{
			"CorrectableErrorThreshold": jsonOf(1.5),
		}, schema)
		require.Len(t, got, 1)
	})
	t.Run("an integer float64 cannot hold exactly is refused", func(t *testing.T) {
		// 2^53+1 decodes to 2^53: staging it would write a value nobody
		// approved, and drift between the two would never show.
		got := validateBios(map[string]apiextensionsv1.JSON{
			"CorrectableErrorThreshold": {Raw: []byte("9007199254740993")},
		}, schema)
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "9007199254740993 is beyond")
		assert.Empty(t, validateBios(map[string]apiextensionsv1.JSON{
			"CorrectableErrorThreshold": {Raw: []byte("9007199254740991")},
		}, schema), "2^53-1 is exact")
	})
	t.Run("no registry validates nothing", func(t *testing.T) {
		assert.Empty(t, validateBios(map[string]apiextensionsv1.JSON{"Anything": jsonOf(1)}, nil))
	})
}

func TestPlanHash(t *testing.T) {
	a := planHash(map[string]any{"QuietBoot": true, "BootModeSelect": "UEFI"})
	b := planHash(map[string]any{"BootModeSelect": "UEFI", "QuietBoot": true})
	assert.Equal(t, a, b, "canonical over map order")
	assert.NotEqual(t, a, planHash(map[string]any{"BootModeSelect": "Dual", "QuietBoot": true}))
	assert.Len(t, hash8(a), 8)
}

func TestDriftedKeys(t *testing.T) {
	desired := map[string]any{"A": "x", "B": true, "C": float64(3)}
	assert.Empty(t, driftedKeys(desired,
		map[string]any{"A": "x", "B": true, "C": float64(3), "D": "noise"}, nil))
	assert.Equal(t, []string{"B", "C"},
		driftedKeys(desired, map[string]any{"A": "x", "B": false}, nil),
		"differing and unserved keys, sorted")
	assert.Equal(t, []string{"A"},
		driftedKeys(desired, map[string]any{"A": "x", "B": true, "C": float64(3)},
			map[string]any{"A": "y"}),
		"a contradictory pending value is future drift on a schedule")
	assert.Empty(t,
		driftedKeys(desired, map[string]any{"A": "x", "B": true, "C": float64(3)},
			map[string]any{"A": "x", "E": "unmanaged"}),
		"agreeing or unmanaged pending entries are not drift")
}

// A BIOS read that fails on an open session is not a path that is down.
// Before, the lane reported every observation error as NoReachablePath.
func TestBiosReadErrorIsNoUnreachablePath(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	served := sim.NewServer(sim.NewState(f))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Bios") {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		served.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ep := &metalv1alpha1.ManagementEndpoint{Spec: metalv1alpha1.ManagementEndpointSpec{Address: srv.URL,
		CredentialsRef: &corev1.LocalObjectReference{Name: "bmc-a-factory"}, TLS: &metalv1alpha1.EndpointTLS{InsecureSkipVerify: true}}}
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "bmc-a-factory"},
		Data: map[string][]byte{"username": []byte("admin"), "password": []byte("admin-pass")}}
	host := &metalv1alpha1.Host{}
	r, _ := laneRig(t, ep, host, creds)
	r.CredentialsNamespace = "pk-metal"
	host.Status.ManagedBy[0].SystemID = f.Systems[0].ID
	var c laneCond

	_, err = r.bios(t.Context(), host, []metalv1alpha1.BiosProfile{
		biosProfile("p", 0, map[string]apiextensionsv1.JSON{"QuietBoot": jsonOf(false)}),
	}, true, c.set)

	require.NoError(t, err)
	assert.Equal(t, metalv1alpha1.ReasonBiosReadFailed, c.reason, c.msg)
	assert.Contains(t, c.msg, "500")
}
