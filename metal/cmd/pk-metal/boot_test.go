package main

import (
	"context"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

// TestBootSendsRedfishThroughTheRelay: with --relay, boot sends its Redfish
// calls through the relay too, so a workstation that cannot reach the BMC
// network can boot a machine. A relay whose allow list refuses the BMC stops
// boot at the login; a relay that allows it lets boot finish.
func TestBootSendsRedfishThroughTheRelay(t *testing.T) {
	f, err := sim.LoadFixture("generic") // the machine is off: no --yes
	require.NoError(t, err)
	config := filepath.Join(t.TempDir(), "node.yaml")
	require.NoError(t, os.WriteFile(config, []byte("version: v1alpha1\n"), 0o600))
	secret := "0123456789abcdef0123456789abcdef"
	t.Setenv(relay.SecretEnv, secret)
	t.Setenv("PK_METAL_BMC_PASSWORD", f.Accounts[0].Password)

	boot := func(allowBMC bool) (code int, stderr, bmcHost string) {
		bmcSrv := httptest.NewTLSServer(sim.NewServer(sim.NewState(f)))
		defer bmcSrv.Close()
		u, err := url.Parse(bmcSrv.URL)
		require.NoError(t, err)
		allow := "127.0.0.1:1"
		if allowBMC {
			allow = u.Host
		}
		a, err := relay.ParseAllow(allow)
		require.NoError(t, err)
		tunnel, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		front, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = relay.Serve(ctx, tunnel, front, "", []byte(secret), a) }()
		code, stderr = runCLI(t, "boot", "--config", config, "--node", "n", "--relay", tunnel.Addr().String(),
			"--user", f.Accounts[0].Username, "--insecure", bmcSrv.URL)
		return code, stderr, u.Host
	}

	code, stderr, host := boot(false)
	assert.Equal(t, 1, code, stderr)
	assert.Contains(t, stderr, "target "+host+" is not in the relay's allow list")

	code, stderr, _ = boot(true)
	assert.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "boot: OK")
}
