package main

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

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

// TestSimListenPresentsTheServedCertificate: `sim serve --tls` presents the
// certificate of the simulated BMC. The Certificate resources show it, and a
// replaced identity reaches the wire.
func TestSimListenPresentsTheServedCertificate(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	st := sim.NewState(f)
	ln, err := simListen("127.0.0.1:0", st, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = http.Serve(ln, sim.NewServer(st)) }()
	presented := func() *x509.Certificate {
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		require.NoError(t, err)
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0]
	}

	require.NotNil(t, st.ServedLeaf(), "the simulated BMC knows its certificate")
	assert.Equal(t, st.ServedLeaf().Raw, presented().Raw)

	next, err := asset.EphemeralCert()
	require.NoError(t, err)
	st.SetServedCertificate(next)
	assert.Equal(t, st.ServedLeaf().Raw, presented().Raw, "a new identity reaches the wire")
}
