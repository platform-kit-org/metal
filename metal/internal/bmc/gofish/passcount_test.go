package gofish_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// countingSim serves a fixture and counts the requests per path.
func countingSim(t *testing.T, f *sim.Fixture) (*httptest.Server, func() map[string]int) {
	t.Helper()
	next := sim.NewServer(sim.NewState(f))
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		next.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

// observationPass makes the reads of one ManagementEndpoint pass.
func observationPass(t *testing.T, c bmc.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := c.Manager(ctx)
	require.NoError(t, err)
	_, err = c.Chassis(ctx)
	require.NoError(t, err)
	_, err = c.Capabilities(ctx)
	require.NoError(t, err)
	_, _ = c.Media(ctx)
	_, err = c.Systems(ctx)
	require.NoError(t, err)
	_, err = c.FirmwareInventory(ctx)
	require.NoError(t, err)
}

// One observation pass reads each collection and each service resource
// once: the manager, the chassis and the update service are found once per
// session, not once per call. Over a slow BMC link each request costs up to
// a second or more.
func TestObservationPassReadsEachResourceOnce(t *testing.T) {
	for _, name := range sim.Fixtures() {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			srv, counts := countingSim(t, f)
			observationPass(t, connect(t, srv.URL, f))
			seen := counts()
			total := 0
			for _, v := range seen {
				total += v
			}
			for _, res := range []string{"GET /redfish/v1/Managers", "GET /redfish/v1/Chassis", "GET /redfish/v1/UpdateService"} {
				assert.LessOrEqual(t, seen[res], 1, res)
			}
			// The manager is read when it is found, and fresh for Manager.
			assert.LessOrEqual(t, seen["GET /redfish/v1/Managers/1"], 2, "manager reads")
			for req, n := range seen {
				if strings.Contains(req, "/VirtualMedia/") {
					assert.LessOrEqual(t, n, 1, req)
				}
			}
			// Before the lookups were kept per session, a pass made 46 to 61
			// requests on these fixtures.
			assert.LessOrEqual(t, total, 48, "requests in one pass")
		})
	}
}

// Power reads the system and nothing else: a step polls it while a reset
// settles, twice a second.
func TestPowerReadsOnlyTheSystem(t *testing.T) {
	for _, name := range sim.Fixtures() {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			srv, counts := countingSim(t, f)
			c := connect(t, srv.URL, f)
			before := counts()
			_, err = c.Power(t.Context(), f.Systems[0].ID)
			require.NoError(t, err)
			for req, n := range counts() {
				if n -= before[req]; n > 0 {
					assert.Equal(t, "GET /redfish/v1/Systems/"+f.Systems[0].ID, req, "%d requests", n)
					assert.Equal(t, 1, n, req)
				}
			}
		})
	}
}

// A system that the service keeps at a URI that does not end in its id is
// found in the Systems collection, past a member that fails to read.
func TestSystemAtAnotherURIIsFound(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	next := sim.NewServer(sim.NewState(f))
	id := f.Systems[0].ID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redfish/v1/Systems":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Members":[{"@odata.id":"/redfish/v1/Systems/broken"},{"@odata.id":"/redfish/v1/Systems/self"}]}`)
		case "/redfish/v1/Systems/self":
			r.URL.Path = "/redfish/v1/Systems/" + id
			next.ServeHTTP(w, r)
		case "/redfish/v1/Systems/broken", "/redfish/v1/Systems/" + id:
			http.Error(w, "not here", http.StatusNotFound)
		default:
			next.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	state, err := connect(t, srv.URL, f).Power(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerState(f.Systems[0].Power), state)
}
