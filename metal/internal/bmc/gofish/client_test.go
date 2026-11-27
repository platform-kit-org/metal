package gofish_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stmcginnis/gofish/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/bmctest"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// serve starts an HTTP server with the sim of f. The test closes it.
func serve(t *testing.T, f *sim.Fixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
	t.Cleanup(srv.Close)
	return srv
}

// connect opens a session to url, after edit changes the config. A fixture
// that seeds accounts enforces them; its first account is the factory
// administrator, and the session logs in with it. The test closes the
// session, before the server.
func connect(t *testing.T, url string, f *sim.Fixture, edit ...func(*gofishclient.Config)) bmc.Client {
	t.Helper()
	cfg := gofishclient.Config{Endpoint: url}
	if len(f.Accounts) > 0 {
		cfg.Username, cfg.Password = f.Accounts[0].Username, f.Accounts[0].Password
	}
	for _, e := range edit {
		e(&cfg)
	}
	c, err := gofishclient.Connect(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readOnly is the edit for a read-only session.
func readOnly(cfg *gofishclient.Config) { cfg.ReadOnly = true }

// TestConformanceOverHTTP is the core seam proof: the PRODUCTION client (gofish + raw OEM
// paths) passes the same bmctest suite as the in-process sim client, against the sim's
// HTTP tree — both sides of the seam exercised, for every personality: the
// two vendor shapes and the generic one that only follows the specification.
func TestConformanceOverHTTP(t *testing.T) {
	for _, name := range sim.Fixtures() {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			bmctest.Run(t, connect(t, serve(t, f).URL, f), f, false)
		})
	}
}

// TestBiosDialectsOverHTTP runs the conformance suite over the generic tree
// played in two vendors' BIOS staging dialects, as in-test mutations like the
// accounts variant: iDRAC 9, which applies staged values only when the PATCH
// asked for OnReset, and iLO 5, whose pending object serves every attribute.
// Its bios-pending-applied and bios-read checks fail against a client that
// sends no apply time or reads the whole iLO set as pending.
func TestBiosDialectsOverHTTP(t *testing.T) {
	for name, dialect := range map[string]func(*sim.Fixture){
		"idrac-apply-time-required": func(f *sim.Fixture) {
			f.Bios.ApplyTimeRequired = true
			f.Bios.SupportedApplyTimes = []string{"OnReset", "AtMaintenanceWindowStart", "InMaintenanceWindowOnReset"}
		},
		"ilo-pending-full-set": func(f *sim.Fixture) { f.Bios.PendingFullSet = true },
	} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture("generic")
			require.NoError(t, err)
			dialect(f)
			bmctest.Run(t, connect(t, serve(t, f).URL, f), f, false)
		})
	}
}

// TestConformanceOverHTTPWithAccounts runs the suite against an
// enforcement-enabled tree — the production client authenticating with the
// factory account the fixture seeds, the account lane active.
func TestConformanceOverHTTPWithAccounts(t *testing.T) {
	for _, name := range []string{"huawei-2288h-v5", "supermicro-x12"} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			f.Accounts = []sim.FixtureAccount{
				{ID: "2", Username: "admin", Password: "factory", Role: "Administrator", Enabled: true},
			}
			bmctest.Run(t, connect(t, serve(t, f).URL, f), f, false)
		})
	}
}

// TestAccountRotationOverHTTP pins the credential lifecycle across the wire:
// a rotated password kills the old credential's session on the very next
// call, the new one works, and a login with the old one is refused.
func TestAccountRotationOverHTTP(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	f.Accounts = []sim.FixtureAccount{
		{ID: "2", Username: "admin", Password: "factory", Role: "Administrator", Enabled: true},
	}
	srv := serve(t, f)
	ctx := context.Background()

	login := func(user, pass string) (bmc.Client, error) {
		return gofishclient.Connect(ctx, gofishclient.Config{Endpoint: srv.URL, Username: user, Password: pass})
	}

	factory, err := login("admin", "factory")
	require.NoError(t, err)
	defer factory.Close()
	require.NoError(t, factory.EnsureAccount(ctx, "pk-metal", "InitialPass0"))

	managed, err := login("pk-metal", "InitialPass0")
	require.NoError(t, err)
	_, err = managed.Manager(ctx)
	require.NoError(t, err, "the managed account works")

	require.NoError(t, factory.EnsureAccount(ctx, "pk-metal", "RotatedPass1"))
	_, err = managed.Manager(ctx)
	require.Error(t, err, "the old credential dies with the rotation")
	managed.Close()

	managed, err = login("pk-metal", "RotatedPass1")
	require.NoError(t, err)
	defer managed.Close()
	_, err = managed.Manager(ctx)
	require.NoError(t, err, "the new credential works")

	// The session login is the oracle: the old password opens no session.
	_, err = login("pk-metal", "InitialPass0")
	require.Error(t, err, "the old password logs in no more")
	assert.True(t, gofishclient.LoginRefused(err), "%v", err)
}

// TestConformanceBehindDeflate runs the suite against a service whose ETag
// header carries Apache mod_deflate's "-gzip" suffix, on the fixture that
// requires If-Match: every PATCH must send the tag without the suffix.
func TestConformanceBehindDeflate(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	require.True(t, f.Protocol.ETags && f.Protocol.RequireIfMatch)
	f.Protocol.ETagGzipSuffix = true
	ctx := context.Background()
	c := connect(t, serve(t, f).URL, f)
	require.NoError(t, c.SetBootOverride(ctx, f.Systems[0].ID, bmc.BootOverride{Target: "Cd"}),
		"a PATCH behind mod_deflate must not fail its precondition")
	bmctest.Run(t, c, f, false)
}

// TestReadOnlyRefusesMutation is the read-only rail: a read-only client
// refuses every write, whatever the caller asks, and no write reaches the
// BMC. The login and the logout of the session still do. Each method runs
// on a fixture where it gets as far as its write.
func TestReadOnlyRefusesMutation(t *testing.T) {
	const pem = "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n"
	for name, tc := range map[string]struct {
		fixture string
		call    func(context.Context, bmc.Client) error
	}{
		"SetPower": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.SetPower(ctx, "1", bmc.ActionForceOff)
		}},
		"SetBootOverride": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.SetBootOverride(ctx, "1", bmc.BootOverride{Target: "Cd"})
		}},
		"InsertMedia": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.InsertMedia(ctx, "2", "https://x/i.iso", nil)
		}},
		"VmmControl": {"huawei-2288h-v5", func(ctx context.Context, c bmc.Client) error {
			return c.InsertMedia(ctx, "CD", "https://x/i.iso", nil)
		}},
		"EjectMedia": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.EjectMedia(ctx, "2")
		}},
		"UpdateFirmware": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.UpdateFirmware(ctx, []byte("SIMFW 1"))
		}},
		"StageFirmware": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.StageFirmware(ctx, []byte("SIMFW 1"), "BIOS")
		}},
		"ResetManager": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.ResetManager(ctx, bmc.ActionGracefulRestart)
		}},
		"SetBiosAttributes": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.SetBiosAttributes(ctx, "1", map[string]any{"QuietBoot": false})
		}},
		"EnsureAccount": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.EnsureAccount(ctx, "pk-metal", "managed-pass-1")
		}},
		"DisableAccount": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.DisableAccount(ctx, "admin")
		}},
		"SetNTP": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.SetNTP(ctx, bmc.NTPConfig{Enabled: true, Servers: []string{"ntp.example.org"}})
		}},
		"GenerateCSR": {"generic", func(ctx context.Context, c bmc.Client) error {
			_, err := c.GenerateCSR(ctx, bmc.CSRSubject{CommonName: "bmc.example.internal", Country: "XX"})
			return err
		}},
		"ImportServerCertificate": {"generic", func(ctx context.Context, c bmc.Client) error {
			return c.ImportServerCertificate(ctx, []byte(pem))
		}},
		"UploadServerIdentity": {"supermicro-x12", func(ctx context.Context, c bmc.Client) error {
			return c.UploadServerIdentity(ctx, []byte(pem), []byte("key"))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(tc.fixture)
			require.NoError(t, err)
			srv, counts := countingSim(t, f)
			c := connect(t, srv.URL, f, readOnly)
			before := counts()
			require.ErrorIs(t, tc.call(t.Context(), c), gofishclient.ErrReadOnly)
			for req, n := range counts() {
				if n > before[req] {
					assert.True(t, strings.HasPrefix(req, "GET "), "%s reached the BMC", req)
				}
			}
		})
	}
}

// A manager that says it cannot serve the request now is busy; one that
// refuses or does not know the request is not.
func TestBusy(t *testing.T) {
	for code, busy := range map[int]bool{
		http.StatusTooManyRequests: true, http.StatusBadGateway: true,
		http.StatusServiceUnavailable: true, http.StatusGatewayTimeout: true,
		http.StatusBadRequest: false, http.StatusUnauthorized: false, http.StatusNotFound: false,
		http.StatusInternalServerError: false,
	} {
		err := fmt.Errorf("power: %w", &schemas.Error{HTTPReturnedStatusCode: code})
		assert.Equal(t, busy, gofishclient.Busy(err), "%d", code)
	}
	assert.False(t, gofishclient.Busy(errors.New("no status")))
	assert.False(t, gofishclient.Busy(nil))
}
