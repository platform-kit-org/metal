package lab

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/constants"
	yaml "go.yaml.in/yaml/v4"

	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The protocols a Probe speaks.
const (
	// ProbeDNS asks a DNS server, over UDP port 53, for the address of
	// ProbeName.
	ProbeDNS = "dns"
	// ProbeNTP asks an NTP server, over UDP port 123, for the time.
	ProbeNTP = "ntp"
	// ProbeHTTPS sends a GET and accepts any HTTP answer: a registry answers
	// 401 to a client without a token, and that proves the path is open. It
	// follows redirects, so a registry that sends its clients to a mirror
	// (registry.k8s.io) is checked together with that mirror.
	ProbeHTTPS = "https"
	// ProbeTLS opens a TLS connection and checks the certificate, for a
	// host that answers no plain GET (a gRPC service, a CDN).
	ProbeTLS = "tls"
)

// Who reaches an endpoint, and so how the probe reaches it.
const (
	// FromWorkstation is a request of the workstation itself: the operator
	// in kind, Docker, go. It goes through the workstation's proxy, if one
	// is set.
	FromWorkstation = "workstation"
	// FromMachines is a request of the Talos machines. They go through the
	// workstation's NAT, without its proxy and with their own DNS servers.
	FromMachines = "machines"
)

// ProbeName is the name a DNS probe resolves.
const ProbeName = render.PublicFactory

// Probe is one endpoint the lab needs from outside the workstation.
type Probe struct {
	Kind   string // ProbeDNS, ProbeNTP, ProbeHTTPS or ProbeTLS
	Target string // host:port, or the URL for ProbeHTTPS
	Who    string // FromWorkstation or FromMachines
	For    string // what needs it, a few words
	// Optional is true for an endpoint the lab works without, slower or
	// with less: doctor warns and does not fail.
	Optional bool
	// Proxy is the proxy URL the machines use for HTTPS (an
	// EnvironmentConfig patch sets https_proxy); empty is none.
	Proxy string
}

func (p Probe) String() string {
	s := p.Kind + " " + p.Target
	if p.Kind == ProbeHTTPS {
		s = p.Target
	}
	if p.Proxy != "" {
		s += " via proxy"
	}
	return s
}

// Probes are the endpoints outside the workstation that the lab needs: what
// the workstation downloads, and what the Talos machines reach through its
// NAT. The machines' DNS and NTP servers, the discovery service and the
// registry mirrors come from the spec's patches, else from Talos' defaults;
// the registries are the ones the Talos and Kubernetes images come from.
func (s *Spec) Probes() ([]Probe, error) {
	t, err := s.talosNetwork()
	if err != nil {
		return nil, err
	}
	probes := []Probe{
		{ProbeHTTPS, "https://" + render.PublicFactory + "/", FromWorkstation, "boot media, schematics", false, ""},
		{ProbeHTTPS, "https://registry-1.docker.io/v2/", FromWorkstation, "kind node, registry, image build", false, ""},
		{ProbeTLS, "production.cloudflare.docker.com:443", FromWorkstation, "Docker Hub image layers", false, ""},
		{ProbeHTTPS, "https://gcr.io/v2/", FromWorkstation, "base of the operator image", false, ""},
		{ProbeHTTPS, "https://proxy.golang.org/", FromWorkstation, "Go modules of the image build", false, ""},
	}
	for _, ns := range t.nameservers {
		probes = append(probes, Probe{ProbeDNS, net.JoinHostPort(ns, "53"), FromMachines, "every name the machines look up", false, ""})
	}
	for _, ts := range t.timeServers {
		probes = append(probes, Probe{ProbeNTP, net.JoinHostPort(ts, "123"), FromMachines, "the clock (boot waits, then goes on)", true, ""})
	}
	registries := []struct{ host, what string }{
		{render.PublicFactory, "installer image"},
		{imageHost(constants.KubeletImage), "kubelet, flannel"},
		{imageHost(constants.KubernetesAPIServerImage), "control plane, etcd, CoreDNS"},
	}
	for _, r := range registries {
		mirrors := t.mirrors[r.host]
		if len(mirrors) == 0 {
			probes = append(probes, Probe{ProbeHTTPS, "https://" + r.host + "/v2/", FromMachines, r.what, false, ""})
			continue
		}
		for _, m := range mirrors {
			probes = append(probes, Probe{ProbeHTTPS, m, FromMachines, r.what + " (mirror of " + r.host + ")", false, ""})
		}
	}
	if len(t.mirrors[imageHost(constants.KubeletImage)]) == 0 {
		// ghcr.io sends its clients to this CDN for the image layers.
		probes = append(probes, Probe{ProbeTLS, "pkg-containers.githubusercontent.com:443", FromMachines, "ghcr.io image layers", false, ""})
	}
	if t.discovery != "" {
		probes = append(probes, Probe{ProbeTLS, t.discovery, FromMachines, "cluster discovery (talosctl get members)", true, ""})
	}
	for i := range probes {
		if probes[i].Who == FromMachines && (probes[i].Kind == ProbeHTTPS || probes[i].Kind == ProbeTLS) {
			probes[i].Proxy = t.proxy
		}
	}
	return probes, nil
}

// imageHost is the registry of an image reference.
func imageHost(ref string) string {
	host, _, _ := strings.Cut(ref, "/")
	return host
}

// talosNetwork is what the spec's patches say about the machines' way out.
type talosNetwork struct {
	nameservers []string
	timeServers []string
	discovery   string              // host:port; empty when it is off
	proxy       string              // https_proxy of an EnvironmentConfig
	mirrors     map[string][]string // registry host -> endpoint URLs
}

// talosNetwork reads the ResolverConfig, TimeSyncConfig,
// DiscoveryServiceConfig, RegistryMirrorConfig and EnvironmentConfig
// documents of the patches.
// A value no patch sets is Talos' default. The last patch wins, as when
// Talos merges them.
func (s *Spec) talosNetwork() (talosNetwork, error) {
	t := talosNetwork{mirrors: map[string][]string{}}
	discovery := constants.DefaultDiscoveryServiceEndpoint
	var nameservers, timeServers []string
	for i, p := range s.Patches {
		dec := yaml.NewDecoder(strings.NewReader(p.Inline))
		for {
			var doc struct {
				Kind        string `yaml:"kind"`
				Name        string `yaml:"name"`
				Nameservers []struct {
					Address string `yaml:"address"`
				} `yaml:"nameservers"`
				Enabled *bool `yaml:"enabled"`
				NTP     *struct {
					Servers []string `yaml:"servers"`
				} `yaml:"ntp"`
				Variables map[string]string `yaml:"variables"`
				Endpoint  string            `yaml:"endpoint"`
				Endpoints []struct {
					URL string `yaml:"url"`
				} `yaml:"endpoints"`
			}
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return t, fmt.Errorf("patches[%d]: %w", i, err)
			}
			switch doc.Kind {
			case "ResolverConfig":
				nameservers = nameservers[:0]
				for _, ns := range doc.Nameservers {
					nameservers = append(nameservers, ns.Address)
				}
			case "TimeSyncConfig":
				switch {
				case doc.Enabled != nil && !*doc.Enabled:
					timeServers = []string{}
				case doc.NTP != nil && len(doc.NTP.Servers) > 0:
					timeServers = doc.NTP.Servers
				}
			case "DiscoveryServiceConfig":
				if doc.Endpoint != "" {
					discovery = doc.Endpoint
				}
			case "EnvironmentConfig":
				for _, k := range []string{"https_proxy", "HTTPS_PROXY"} {
					if v, ok := doc.Variables[k]; ok {
						t.proxy = v
					}
				}
			case "RegistryMirrorConfig":
				var urls []string
				for _, e := range doc.Endpoints {
					urls = append(urls, e.URL)
				}
				t.mirrors[doc.Name] = urls
			}
		}
	}
	t.nameservers = nameservers
	if len(nameservers) == 0 {
		t.nameservers = []string{constants.DefaultPrimaryResolver, constants.DefaultSecondaryResolver}
	}
	t.timeServers = timeServers
	if timeServers == nil {
		t.timeServers = []string{constants.DefaultNTPServer}
	}
	u, err := url.Parse(discovery)
	if err != nil || u.Host == "" {
		return t, fmt.Errorf("DiscoveryServiceConfig: endpoint %q is not a URL", discovery)
	}
	t.discovery = u.Host
	if u.Port() == "" {
		t.discovery = net.JoinHostPort(u.Hostname(), "443")
	}
	return t, nil
}

// Reach is the outcome of one probe.
type Reach struct {
	Probe
	Err  error         // nil when the endpoint answered
	Took time.Duration // how long the answer took
	Note string        // what answered, for example "HTTP 401"
}

// ProbeTimeout is how long a probe waits for an answer.
const ProbeTimeout = 5 * time.Second

// Check runs the probes in parallel, each for at most timeout, and returns
// their outcomes in the order of probes.
func Check(ctx context.Context, probes []Probe, timeout time.Duration) []Reach {
	out := make([]Reach, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			note, err := check(ctx, p)
			out[i] = Reach{Probe: p, Err: explain(err, timeout), Took: time.Since(start), Note: note}
		})
	}
	wg.Wait()
	return out
}

func check(ctx context.Context, p Probe) (string, error) {
	switch p.Kind {
	case ProbeDNS:
		return checkDNS(ctx, p.Target)
	case ProbeNTP:
		return checkNTP(ctx, p.Target)
	case ProbeHTTPS:
		return checkHTTPS(ctx, p.Target, proxyOf(p))
	case ProbeTLS:
		if p.Proxy != "" {
			// Through a proxy, a TLS connection is a CONNECT; any HTTP
			// answer at the end of it proves the way is open.
			return checkHTTPS(ctx, "https://"+p.Target+"/", proxyOf(p))
		}
		return checkTLS(ctx, p.Target)
	}
	return "", fmt.Errorf("unknown probe kind %q", p.Kind)
}

// proxyOf is how a probe finds its proxy: the workstation's own requests
// use its environment, the machines' the proxy of their patches, if any.
func proxyOf(p Probe) func(*http.Request) (*url.URL, error) {
	if p.Who == FromWorkstation {
		return workstationProxy
	}
	if p.Proxy == "" {
		return nil
	}
	return func(*http.Request) (*url.URL, error) { return url.Parse(p.Proxy) }
}

// checkDNS asks server for the address of ProbeName, as a machine does.
func checkDNS(ctx context.Context, server string) (string, error) {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, server)
	}}
	addrs, err := r.LookupHost(ctx, ProbeName)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s has %d address(es)", ProbeName, len(addrs)), nil
}

// checkNTP sends an SNTP client request (RFC 4330) and waits for a server
// answer.
func checkNTP(ctx context.Context, server string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	req := make([]byte, 48)
	req[0] = 0x23 // leap 0, version 4, mode 3 (client)
	if _, err := conn.Write(req); err != nil {
		return "", err
	}
	resp := make([]byte, 48)
	n, err := conn.Read(resp)
	if err != nil {
		return "", err
	}
	if n < 48 || resp[0]&0x07 != 4 {
		return "", errors.New("the answer is not an NTP server reply")
	}
	if resp[1] == 0 {
		return "", errors.New("the server is not synchronized (stratum 0)")
	}
	if binary.BigEndian.Uint32(resp[40:44]) == 0 {
		return "", errors.New("the reply has no time")
	}
	return fmt.Sprintf("stratum %d", resp[1]), nil
}

// probeRoots are the CAs the probes trust; nil is the system's. Tests set it.
var probeRoots *x509.CertPool

// workstationProxy picks the proxy of the workstation's own requests. Tests
// set it: http.ProxyFromEnvironment reads the environment only once.
var workstationProxy = http.ProxyFromEnvironment

// checkHTTPS sends a GET, through proxy unless it is nil, and accepts any
// HTTP status.
func checkHTTPS(ctx context.Context, target string, proxy func(*http.Request) (*url.URL, error)) (string, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = proxy
	tr.TLSClientConfig = &tls.Config{RootCAs: probeRoots, MinVersion: tls.VersionTLS12}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	note := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if resp.Request.URL.Host != req.URL.Host {
		note += " from " + resp.Request.URL.Host
	}
	return note, nil
}

// checkTLS opens a TLS connection and verifies the certificate.
func checkTLS(ctx context.Context, target string) (string, error) {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return "", err
	}
	d := &tls.Dialer{Config: &tls.Config{ServerName: host, RootCAs: probeRoots, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return "", err
	}
	_ = conn.Close()
	return "TLS handshake", nil
}

// explain turns a network error into the words a person can act on.
func explain(err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &certErr):
		return fmt.Errorf("the certificate is not trusted, so something on the way answers in place of the server (a captive portal or a TLS proxy): %w", err)
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return fmt.Errorf("no answer in %s: a firewall on the way drops the packets", timeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("connection refused: a firewall on the way rejects the port: %w", err)
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return fmt.Errorf("no route: %w", err)
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return fmt.Errorf("the name does not resolve: %w", err)
	}
	return err
}

// FormatReach writes the outcomes as a table, one line per probe, in the
// style of lab:doctor: "ok", "MISS" for a probe the lab needs, "warn" for an
// optional one. It returns the number of MISS lines.
func FormatReach(w io.Writer, rs []Reach) int {
	var b bytes.Buffer
	widths := [3]int{len("who"), len("endpoint"), len("for")}
	for _, r := range rs {
		widths[0] = max(widths[0], len(r.Who))
		widths[1] = max(widths[1], len(r.String()))
		widths[2] = max(widths[2], len(r.For))
	}
	row := func(mark, who, endpoint, what, result string) {
		fmt.Fprintf(&b, "%-6s%-*s  %-*s  %-*s  %s\n", mark, widths[0], who, widths[1], endpoint, widths[2], what, result)
	}
	row("", "who", "endpoint", "for", "result")
	misses := 0
	for _, r := range rs {
		mark, result := "ok", r.Note+", "+r.Took.Round(time.Millisecond).String()
		if r.Err != nil {
			mark, result = "MISS", r.Err.Error()
			if r.Optional {
				mark = "warn"
			} else {
				misses++
			}
		}
		row(mark, r.Who, r.String(), r.For, result)
	}
	var shown []string
	for _, r := range rs {
		if r.Err == nil {
			continue
		}
		fix := fixFor(r.Probe)
		if slices.Contains(shown, fix) {
			continue
		}
		shown = append(shown, fix)
		fmt.Fprintf(&b, "      fix: %s\n", fix)
	}
	_, _ = w.Write(b.Bytes())
	return misses
}

// fixFor says what to do about a probe that got no answer.
func fixFor(p Probe) string {
	switch {
	case p.Who == FromWorkstation:
		return "the workstation needs these endpoints. Behind a proxy, set HTTPS_PROXY and NO_PROXY (https://metal.platform-kit.org/reference/lab/network/#behind-a-proxy); without internet, see https://metal.platform-kit.org/operate/day-1/air-gapped-site/"
	case p.Kind == ProbeDNS:
		return "give the machines DNS servers that this network allows, for example the one in /etc/resolv.conf: change the ResolverConfig patch in lab/lab.yaml"
	case p.Kind == ProbeNTP:
		return "give the machines an NTP server that this network allows: add a TimeSyncConfig patch with ntp.servers to lab/lab.yaml. Without one, each boot waits bootTimeout (2m) for the clock, then continues"
	case p.Optional:
		return "Talos finds the other members of its cluster through the discovery service; Kubernetes and etcd work without it. Point a DiscoveryServiceConfig patch in lab/lab.yaml at a service that this network allows, or accept the warning"
	}
	return "the machines pull their images straight through this workstation's NAT, without its proxy. Use a network that allows these registries, a registry mirror (a RegistryMirrorConfig patch in lab/lab.yaml), or give the machines a proxy (https://metal.platform-kit.org/reference/lab/network/#behind-a-proxy)"
}
