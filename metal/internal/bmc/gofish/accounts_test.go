package gofish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

func connectAs(t *testing.T, url, user, password string) error {
	t.Helper()
	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: url, Username: user, Password: password,
	})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Systems(context.Background())
	return err
}

// A fixed-slot collection whose first vacant slot is reserved and refuses a
// PATCH (iDRAC's slot 1): the account goes into the next vacant slot.
func TestEnsureAccountSkipsAReservedSlot(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	f.Accounts[0].ID = "2"
	f.ReservedAccountSlots = []string{"1"}
	srv := serve(t, f)
	c := connect(t, srv.URL, f)
	require.NoError(t, c.EnsureAccount(context.Background(), "pk-metal", "managed-pass-1"))
	require.NoError(t, connectAs(t, srv.URL, "pk-metal", "managed-pass-1"))
}

// An account that failed logins locked stays locked after a new password on
// some BMCs. EnsureAccount clears the lock with the reset.
func TestEnsureAccountUnlocks(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	f.LockoutThreshold = 3
	st := sim.NewState(f)
	srv := httptest.NewServer(sim.NewServer(st))
	t.Cleanup(srv.Close)
	c := connect(t, srv.URL, f)
	require.NoError(t, c.EnsureAccount(context.Background(), "pk-metal", "old-pass-1"))
	for range 3 {
		require.Error(t, connectAs(t, srv.URL, "pk-metal", "wrong-pass"))
	}
	require.True(t, st.AccountLocked("pk-metal"))
	require.Error(t, connectAs(t, srv.URL, "pk-metal", "old-pass-1"), "a locked account refuses the right password too")

	require.NoError(t, c.EnsureAccount(context.Background(), "pk-metal", "new-pass-1"))
	assert.False(t, st.AccountLocked("pk-metal"))
	require.NoError(t, connectAs(t, srv.URL, "pk-metal", "new-pass-1"))
}

// DisableAccount turns the account off over the wire, on both collection
// shapes: the account logs in no more, and the account that disabled it
// keeps working.
func TestDisableAccountOverHTTP(t *testing.T) {
	for name, slots := range map[string]int{"fixed-slots": 8, "growable": 0} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture("generic")
			require.NoError(t, err)
			f.AccountSlots = slots
			st := sim.NewState(f)
			srv := httptest.NewServer(sim.NewServer(st))
			t.Cleanup(srv.Close)
			admin := f.Accounts[0]
			c := connect(t, srv.URL, f)
			require.NoError(t, c.EnsureAccount(context.Background(), "pk-old", "old-pass-1"))
			require.NoError(t, connectAs(t, srv.URL, "pk-old", "old-pass-1"))

			require.NoError(t, c.DisableAccount(context.Background(), "pk-old"))
			assert.False(t, st.AccountEnabled("pk-old"))
			require.Error(t, connectAs(t, srv.URL, "pk-old", "old-pass-1"), "a disabled account logs in no more")
			require.NoError(t, connectAs(t, srv.URL, admin.Username, admin.Password))
		})
	}
}

// echoingSim serves the generic sim, except that a request that carries
// secret is refused, and the refusal echoes secret in MessageArgs: a login
// with 401, any other request with 400.
func echoingSim(t *testing.T, f *sim.Fixture, secret string) *httptest.Server {
	t.Helper()
	next := sim.NewServer(sim.NewState(f))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(secret)) {
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
			return
		}
		status := http.StatusBadRequest
		if strings.HasSuffix(r.URL.Path, "/Sessions") {
			status = http.StatusUnauthorized
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"@Message.ExtendedInfo": []any{map[string]any{"MessageArgs": []string{secret, "Password"}}},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A BMC that refuses a password and echoes it in the refusal: the error
// names no password, because the caller writes it into a condition.
func TestAccountErrorsDoNotEchoThePassword(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	const pw = "managed-pass-0123456789"
	srv := echoingSim(t, f, pw)
	err = connect(t, srv.URL, f).EnsureAccount(t.Context(), "pk-metal", pw)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), pw, "EnsureAccount")

	_, err = gofishclient.Connect(t.Context(), gofishclient.Config{Endpoint: srv.URL, Username: "pk-metal", Password: pw})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), pw, "Connect")
}
