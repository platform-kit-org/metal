package lab

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reachSpec = `
cluster: { name: lab, talosVersion: v1.14.6, kubernetesVersion: v1.36.2, vip: 10.77.0.10 }
controlPlanes: 1
workers: 0
controlPlane: { memoryMiB: 4096, cpus: 2 }
worker: { memoryMiB: 2048, cpus: 2 }
`

func probesOf(t *testing.T, raw string) []Probe {
	t.Helper()
	s, err := Parse([]byte(raw))
	require.NoError(t, err)
	probes, err := s.Probes()
	require.NoError(t, err)
	return probes
}

func targets(probes []Probe, who string) []string {
	var out []string
	for _, p := range probes {
		if p.Who == who {
			out = append(out, p.String())
		}
	}
	return out
}

// Without patches, the machines use Talos' own defaults.
func TestProbesTalosDefaults(t *testing.T) {
	probes := probesOf(t, reachSpec)
	assert.Equal(t, []string{
		"dns 1.1.1.1:53", "dns 8.8.8.8:53", "ntp time.cloudflare.com:123",
		"https://factory.talos.dev/v2/", "https://ghcr.io/v2/", "https://registry.k8s.io/v2/",
		"tls pkg-containers.githubusercontent.com:443", "tls discovery.talos.dev:443",
	}, targets(probes, FromMachines))
	assert.Contains(t, targets(probes, FromWorkstation), "https://factory.talos.dev/")
	for _, p := range probes {
		assert.Equal(t, p.Kind == ProbeNTP || strings.HasPrefix(p.Target, "discovery."), p.Optional, p.String())
	}
}

// The machines' DNS and NTP servers, the discovery service and the registry
// mirrors come from the patches; the last patch wins.
func TestProbesFollowThePatches(t *testing.T) {
	probes := probesOf(t, reachSpec+`
patches:
  - inline: |
      apiVersion: v1alpha1
      kind: ResolverConfig
      nameservers:
        - address: 1.1.1.1
  - inline: |
      apiVersion: v1alpha1
      kind: ResolverConfig
      nameservers:
        - address: 192.0.2.53
      ---
      apiVersion: v1alpha1
      kind: TimeSyncConfig
      ntp:
        servers: [ntp.example.org]
      ---
      apiVersion: v1alpha1
      kind: DiscoveryServiceConfig
      name: default
      endpoint: https://discovery.example.org:3000/
      ---
      apiVersion: v1alpha1
      kind: RegistryMirrorConfig
      name: ghcr.io
      endpoints:
        - url: https://mirror.example.org/v2/ghcr.io
          overridePath: true
`)
	assert.Equal(t, []string{
		"dns 192.0.2.53:53", "ntp ntp.example.org:123",
		"https://factory.talos.dev/v2/", "https://mirror.example.org/v2/ghcr.io", "https://registry.k8s.io/v2/",
		"tls discovery.example.org:3000",
	}, targets(probes, FromMachines))
}

// Machines with a proxy reach the registries and the discovery service
// through it.
func TestProbesMachineProxy(t *testing.T) {
	probes := probesOf(t, reachSpec+`
patches:
  - inline: |
      apiVersion: v1alpha1
      kind: EnvironmentConfig
      variables:
        https_proxy: http://proxy.example.org:3128
`)
	for _, p := range probes {
		web := p.Kind == ProbeHTTPS || p.Kind == ProbeTLS
		if p.Who == FromMachines && web {
			assert.Equal(t, "http://proxy.example.org:3128", p.Proxy, p.String())
			assert.True(t, strings.HasSuffix(p.String(), " via proxy"), p.String())
		} else {
			assert.Empty(t, p.Proxy, p.String())
		}
	}
}

// A machine without time sync asks no NTP server.
func TestProbesTimeSyncOff(t *testing.T) {
	probes := probesOf(t, reachSpec+`
patches:
  - inline: |
      apiVersion: v1alpha1
      kind: TimeSyncConfig
      enabled: false
`)
	for _, p := range probes {
		assert.NotEqual(t, ProbeNTP, p.Kind, p.String())
	}
}

// The lab's own spec renders its probes: its ResolverConfig and
// TimeSyncConfig patches decode.
func TestProbesOfTheLabSpec(t *testing.T) {
	s, err := Load("../../../lab/lab.yaml")
	require.NoError(t, err)
	probes, err := s.Probes()
	require.NoError(t, err)
	assert.Contains(t, targets(probes, FromMachines), "dns 1.1.1.1:53")
	assert.Contains(t, targets(probes, FromMachines), "ntp time.cloudflare.com:123")
}

// fakeDNS answers every A question with 192.0.2.1 and every other one with
// no record.
func fakeDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			end := 12
			for q[end] != 0 {
				end += int(q[end]) + 1
			}
			question := q[12 : end+5]
			qtype := binary.BigEndian.Uint16(q[end+1:])
			var resp bytes.Buffer
			resp.Write(q[:2])                       // id
			resp.Write([]byte{0x81, 0x80, 0, 1, 0}) // answer, recursion; one question
			if qtype == 1 {
				resp.WriteByte(1)
			} else {
				resp.WriteByte(0)
			}
			resp.Write([]byte{0, 0, 0, 0})
			resp.Write(question)
			if qtype == 1 {
				resp.Write([]byte{0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 1})
			}
			_, _ = pc.WriteTo(resp.Bytes(), addr)
		}
	}()
	return pc.LocalAddr().String()
}

// fakeNTP answers as a stratum 2 server, or with reply when it is not nil.
func fakeNTP(t *testing.T, reply []byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 48)
		for {
			_, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := reply
			if resp == nil {
				resp = make([]byte, 48)
				resp[0], resp[1] = 0x24, 2
				binary.BigEndian.PutUint32(resp[40:], 0xe9000000)
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

// silentUDP takes packets and never answers, as a firewall that drops them.
func silentUDP(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	return pc.LocalAddr().String()
}

func TestCheck(t *testing.T) {
	trusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer trusted.Close()
	probeRoots = trusted.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	defer func() { probeRoots = nil }()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := closed.Addr().String()
	require.NoError(t, closed.Close())
	badNTP := make([]byte, 48)
	badNTP[0] = 0x24 // a server reply, but stratum 0

	rs := Check(context.Background(), []Probe{
		{Kind: ProbeDNS, Target: fakeDNS(t), Who: FromMachines},
		{Kind: ProbeNTP, Target: fakeNTP(t, nil), Who: FromMachines},
		{Kind: ProbeHTTPS, Target: trusted.URL + "/v2/", Who: FromMachines},
		{Kind: ProbeTLS, Target: strings.TrimPrefix(trusted.URL, "https://"), Who: FromMachines},
		{Kind: ProbeDNS, Target: silentUDP(t), Who: FromMachines},
		{Kind: ProbeNTP, Target: silentUDP(t), Who: FromMachines, Optional: true},
		{Kind: ProbeNTP, Target: fakeNTP(t, badNTP), Who: FromMachines},
		{Kind: ProbeTLS, Target: refused, Who: FromMachines},
	}, 500*time.Millisecond)

	assert.Equal(t, "factory.talos.dev has 1 address(es)", rs[0].Note)
	assert.Equal(t, "stratum 2", rs[1].Note)
	assert.Equal(t, "HTTP 401", rs[2].Note)
	assert.Equal(t, "TLS handshake", rs[3].Note)
	for _, r := range rs[:4] {
		assert.NoError(t, r.Err, r.String())
	}
	require.Error(t, rs[4].Err)
	require.Error(t, rs[5].Err)
	assert.Contains(t, rs[5].Err.Error(), "no answer in 500ms")
	assert.ErrorContains(t, rs[6].Err, "stratum 0")
	assert.ErrorContains(t, rs[7].Err, "connection refused")

	var out strings.Builder
	misses := FormatReach(&out, rs)
	assert.Equal(t, 3, misses, "the optional NTP probe warns")
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.Len(t, lines, 12, "a table of 9 lines and one fix for DNS, NTP and the registries")
	assert.Contains(t, lines[9], "fix: give the machines DNS servers")
	assert.True(t, strings.HasPrefix(lines[0], "      who"), lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "ok    machines"), lines[1])
	assert.True(t, strings.HasPrefix(lines[6], "warn  machines"), lines[6])
	assert.True(t, strings.HasPrefix(lines[5], "MISS  machines"), lines[5])
}

// Only the workstation's own requests go through its proxy; the machines'
// go straight out through the NAT.
func TestCheckHTTPSProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer direct.Close()
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	workstationProxy = http.ProxyURL(proxyURL)
	defer func() { workstationProxy = http.ProxyFromEnvironment }()

	ctx := context.Background()
	note, err := check(ctx, Probe{Kind: ProbeHTTPS, Target: direct.URL + "/", Who: FromWorkstation})
	require.NoError(t, err)
	assert.Equal(t, "HTTP 200", note, "through the workstation's proxy")
	note, err = check(ctx, Probe{Kind: ProbeHTTPS, Target: direct.URL + "/", Who: FromMachines})
	require.NoError(t, err)
	assert.Equal(t, "HTTP 418", note, "straight")
	note, err = check(ctx, Probe{Kind: ProbeHTTPS, Target: direct.URL + "/", Who: FromMachines, Proxy: proxy.URL})
	require.NoError(t, err)
	assert.Equal(t, "HTTP 200", note, "through the machines' own proxy")
}

// A certificate the workstation does not trust is something in the way,
// such as a captive portal.
func TestCheckUntrustedCertificate(t *testing.T) {
	portal := httptest.NewTLSServer(http.NotFoundHandler())
	defer portal.Close()
	rs := Check(context.Background(), []Probe{
		{Kind: ProbeHTTPS, Target: portal.URL + "/", Who: FromMachines},
		{Kind: ProbeTLS, Target: strings.TrimPrefix(portal.URL, "https://"), Who: FromMachines},
	}, time.Second)
	for _, r := range rs {
		assert.ErrorContains(t, r.Err, "captive portal", r.String())
	}
}
