package gofish_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/bmctest"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// The conformance suite passes against a BMC with real timing: late
// answers, a power state that takes a while to change, a slow key
// generation, one connection at a time.
func TestConformanceOnASlowBMC(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	f.Protocol.Latency = 2 * time.Millisecond
	f.Protocol.MaxConnections = 1
	f.Protocol.PowerTransition = 100 * time.Millisecond
	f.Identity.CSRDelay = 100 * time.Millisecond
	bmctest.Run(t, connect(t, serve(t, f).URL, f), f, false)
}

// A request over the sim's connection limit is answered 503 with
// Retry-After.
func TestSimConnectionLimit(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	f.Protocol.Latency = 300 * time.Millisecond
	f.Protocol.MaxConnections = 1
	srv := serve(t, f)

	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			resp, err := http.Get(srv.URL + "/redfish/v1")
			if err == nil {
				codes[i] = resp.StatusCode
				_ = resp.Body.Close()
			}
		})
	}
	wg.Wait()
	assert.Equal(t, []int{http.StatusOK, http.StatusServiceUnavailable}, codes)
}
