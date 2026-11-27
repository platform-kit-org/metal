package gofish_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/bmctest"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// A service that answers 202 with the Task in the body and no Location
// header, and mounts the medium as that task: the client follows the body's
// Task, so a mount that fails is an error and a mount that succeeds reads
// back as inserted when InsertMedia returns.
func TestConformanceWithTaskInBody(t *testing.T) {
	for _, name := range []string{"huawei-2288h-v5", "generic"} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			f.Protocol.TaskDuration = 300 * time.Millisecond
			f.Protocol.TaskInBody = true
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()
			cfg := gofishclient.Config{Endpoint: srv.URL}
			if len(f.Accounts) > 0 {
				cfg.Username, cfg.Password = f.Accounts[0].Username, f.Accounts[0].Password
			}
			c, err := gofishclient.Connect(context.Background(), cfg)
			require.NoError(t, err)
			defer c.Close()
			bmctest.Run(t, c, f, false)
		})
	}
}

// A service that ends a failed task as Completed, with the failure only in
// TaskStatus Critical: the client reports the failure and its message. Each
// fixture passes the conformance checks with such tasks, too.
func TestTaskCompletedCriticalIsAFailure(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	next := sim.NewServer(sim.NewState(f))
	const monitor = "/redfish/v1/TaskService/Tasks/reset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Actions/ComputerSystem.Reset"):
			w.Header().Set("Location", monitor)
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == monitor:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"@odata.type":"#Task.v1_7_4.Task","@odata.id":"`+monitor+`",`+
				`"TaskState":"Completed","TaskStatus":"Critical",`+
				`"Messages":[{"Message":"the power supply refused the request"}]}`)
		default:
			next.ServeHTTP(w, r)
		}
	}))
	defer srv.Close()
	c, err := gofishclient.Connect(t.Context(), gofishclient.Config{
		Endpoint: srv.URL, Username: f.Accounts[0].Username, Password: f.Accounts[0].Password,
	})
	require.NoError(t, err)
	defer c.Close()

	err = c.SetPower(t.Context(), f.Systems[0].ID, bmc.ActionForceOff)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Completed with status Critical")
	assert.Contains(t, err.Error(), "the power supply refused the request")

	for _, name := range sim.Fixtures() {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			f.Protocol.TaskDuration = 100 * time.Millisecond
			f.Protocol.TaskFailureCompleted = true
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()
			cfg := gofishclient.Config{Endpoint: srv.URL}
			if len(f.Accounts) > 0 {
				cfg.Username, cfg.Password = f.Accounts[0].Username, f.Accounts[0].Password
			}
			c, err := gofishclient.Connect(context.Background(), cfg)
			require.NoError(t, err)
			defer c.Close()
			bmctest.Run(t, c, f, false)
		})
	}
}
