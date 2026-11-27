package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// runAsCLI is set in a child process of the test binary: the child runs as
// pk-metal, with its own arguments. The verbs exit the process on bad usage,
// so the test runs them in one of their own.
const runAsCLI = "PK_METAL_TEST_RUN_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(runAsCLI) != "" {
		os.Args[0] = "pk-metal"
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runCLI runs pk-metal with args in a child process and returns its exit
// code and stderr.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), runAsCLI+"=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stderr.String()
	}
	require.NoError(t, err)
	return 0, stderr.String()
}

// fakeClient is a fake API server that holds objs.
func fakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).Build()
}

// fakeClientAs is a fake API that answers a SelfSubjectReview with user, as
// the API server answers it for the caller's credential.
func fakeClientAs(user string, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if ssr, ok := obj.(*authenticationv1.SelfSubjectReview); ok {
					ssr.Status.UserInfo.Username = user
					return nil
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
}

// executed is the executor's outcome condition.
func executed(status metav1.ConditionStatus) metav1.Condition {
	reason := metalv1alpha1.ReasonStepFailed
	if status == metav1.ConditionTrue {
		reason = metalv1alpha1.ReasonStepsSucceeded
	}
	return metav1.Condition{Type: metalv1alpha1.ConditionExecuted, Status: status, Reason: reason}
}

func TestLoggableBlanksSecrets(t *testing.T) {
	// The bodies a BMC sees that carry a secret: a session login, the media
	// credentials of InsertMedia, and an image URL with userinfo (the iBMC's
	// only credential mechanism).
	assert.Equal(t, `{"UserName":"admin","Password":"***"}`,
		loggable([]byte(`{"UserName":"admin","Password":"s3cr\"et"}`)))
	assert.Equal(t, `{"Image":"https://assets/node.iso", "UserName":"media", "Password": "***"}`,
		loggable([]byte("{\"Image\":\"https://assets/node.iso\",\n  \"UserName\":\"media\",\n  \"Password\": \"grant\"}")))
	assert.Equal(t, `{"VmmControlType":"Connect","Image":"https://***@assets/node.iso"}`,
		loggable([]byte(`{"VmmControlType":"Connect","Image":"https://media:grant@assets/node.iso"}`)))

	long := loggable([]byte(`{"Attributes":{"X":"` + strings.Repeat("a", 2000) + `"}}`))
	assert.LessOrEqual(t, len(long), 520, "a long body is cut")
}

func parseBMCFlags(t *testing.T, args ...string) bmcFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bf := connectFlags(fs)
	require.NoError(t, fs.Parse(args))
	return bf
}

// --bmc-ca pins the BMC's certificate to the given CA: the session reaches a
// BMC whose certificate chains to it, and the same BMC without the pin is
// refused.
func TestBMCCAPinsTheCertificate(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	srv := httptest.NewTLSServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "bmc-ca.pem")
	require.NoError(t, os.WriteFile(caFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	acct := f.Accounts[0]

	var warn bytes.Buffer
	cfg, err := parseBMCFlags(t, "--user", acct.Username, "--bmc-ca", caFile).
		config(srv.URL, acct.Password, true, &warn)
	require.NoError(t, err)
	assert.Empty(t, warn.String(), "no warning with a pinned CA")
	c, err := gofishclient.Connect(context.Background(), cfg)
	require.NoError(t, err)
	_, err = c.Systems(context.Background())
	require.NoError(t, err)
	require.NoError(t, c.Close())

	cfg, err = parseBMCFlags(t, "--user", acct.Username).config(srv.URL, acct.Password, true, &warn)
	require.NoError(t, err)
	_, err = gofishclient.Connect(context.Background(), cfg)
	require.Error(t, err, "without --bmc-ca the self-signed BMC is refused")
}

func TestBMCFlagsRefuseBadInput(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate\n"), 0o600))

	_, err := parseBMCFlags(t, "--bmc-ca", empty).config("https://bmc", "", true, io.Discard)
	require.ErrorContains(t, err, "holds no PEM certificate")
	_, err = parseBMCFlags(t, "--bmc-ca", empty, "--insecure").config("https://bmc", "", true, io.Discard)
	require.ErrorContains(t, err, "not both")
}

func TestInsecureWarns(t *testing.T) {
	var warn bytes.Buffer
	cfg, err := parseBMCFlags(t, "--insecure").config("https://bmc", "", true, &warn)
	require.NoError(t, err)
	assert.True(t, cfg.Insecure)
	assert.Contains(t, warn.String(), "--bmc-ca")
}
