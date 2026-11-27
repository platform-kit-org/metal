package qemu

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// The driver is the sim's media-taking machine backend.
var (
	_ sim.MediaMachine    = (*Machine)(nil)
	_ sim.BootMachine     = (*Machine)(nil)
	_ sim.ProgressMachine = (*Machine)(nil)
)

func TestArgs(t *testing.T) {
	dir := t.TempDir()
	m := New(Options{
		Name: "vm1", Dir: dir, InstallerISO: "/isos/talos.iso", Arch: ArchAMD64,
		OVMFCode: "/fw/CODE.fd", DiskSerial: "e2e-disk",
		HostIP: "127.42.0.10", ForwardPorts: []int{50000, 6443},
	})
	require.NoError(t, m.SetMedia(context.Background(), []sim.Media{{SlotID: "2", Data: []byte("cfg")}}))

	args, _, err := m.args()
	require.NoError(t, err)
	line := strings.Join(args, " ")

	assert.Contains(t, line, "-machine q35")
	assert.Contains(t, line, "-accel kvm", "kvm is the default accelerator")
	assert.Contains(t, line, "-cpu host")
	assert.Contains(t, line, "-m 4096", "memory defaults")
	assert.Contains(t, line, "virtio-blk-pci,drive=disk0,id=hd0,bootindex=1,serial=e2e-disk",
		"the blank disk leads the boot order and carries the selector serial")
	assert.Contains(t, line, "id=install,format=raw,readonly=on,file=/isos/talos.iso")
	assert.Contains(t, line, "ide-cd,drive=install,id=cdi,bus=ide.0,bootindex=2",
		"the installer trails the disk — a completed install wins every later boot")
	assert.Contains(t, line, "file="+filepath.Join(dir, "media-0.iso"))
	assert.Contains(t, line, "ide-cd,drive=media0,id=cd0,bus=ide.1,bootindex=3",
		"every CD rides its own AHCI port; BMC media trail the installer")
	assert.Contains(t, line,
		"user,id=net0,hostfwd=tcp:127.42.0.10:50000-:50000,hostfwd=tcp:127.42.0.10:6443-:6443")
	assert.Contains(t, line, "-device virtio-net-pci,netdev=net0,id=nic0",
		"the NIC is not a boot device in the machine's own order")
	assert.NotContains(t, line, "id=nic0,", "no MAC, no boot index")

	// The installer withdrawn and the media set cleared: both vanish.
	m.DetachInstaller()
	require.NoError(t, m.SetMedia(context.Background(), nil))
	args, _, err = m.args()
	require.NoError(t, err)
	line = strings.Join(args, " ")
	assert.NotContains(t, line, "install")
	assert.NotContains(t, line, "media-0.iso")
}

func TestArgsTCG(t *testing.T) {
	m := New(Options{Name: "vm1", Dir: t.TempDir(), OVMFCode: "/fw/CODE.fd", Arch: ArchAMD64, Accel: AccelTCG})
	args, _, err := m.args()
	require.NoError(t, err)
	line := strings.Join(args, " ")
	assert.Contains(t, line, "-accel tcg")
	assert.Contains(t, line, "-cpu max ", "no host CPU passthrough without kvm")
}

// An arm64 machine is a virt board: no AHCI, so its CDs share one
// virtio-scsi controller, and the boot order rides on the same bootindex.
func TestArgsARM64(t *testing.T) {
	dir := t.TempDir()
	m := New(Options{
		Name: "vm1", Dir: dir, InstallerISO: "/isos/talos.iso", Arch: ArchARM64, Accel: AccelTCG,
		OVMFCode: "/fw/AAVMF_CODE.fd", DiskSerial: "e2e-disk",
	})
	require.NoError(t, m.SetMedia(context.Background(), []sim.Media{{SlotID: "2", Data: []byte("cfg")}}))

	args, _, err := m.args()
	require.NoError(t, err)
	line := strings.Join(args, " ")
	assert.Contains(t, line, "-machine virt,gic-version=max")
	assert.Contains(t, line, "-cpu max,pauth-impdef=on", "the cheap pointer authentication under tcg")
	assert.Contains(t, line, "-device virtio-scsi-pci,id=scsi0")
	assert.Contains(t, line, "scsi-cd,drive=install,id=cdi,bus=scsi0.0,bootindex=2")
	assert.Contains(t, line, "scsi-cd,drive=media0,id=cd0,bus=scsi0.0,bootindex=3")
	assert.NotContains(t, line, "ide-cd")
	assert.Contains(t, line, "virtio-blk-pci,drive=disk0,id=hd0,bootindex=1,serial=e2e-disk")

	// Without a CD, no controller either.
	m.DetachInstaller()
	require.NoError(t, m.SetMedia(context.Background(), nil))
	args, _, err = m.args()
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(args, " "), "virtio-scsi-pci")

	// Under kvm the guest gets the host CPU.
	m = New(Options{Name: "vm1", Dir: dir, Arch: ArchARM64, OVMFCode: "/fw/AAVMF_CODE.fd"})
	args, _, err = m.args()
	require.NoError(t, err)
	assert.Contains(t, strings.Join(args, " "), "-cpu host")
}

func TestCheckArch(t *testing.T) {
	other := ArchARM64
	if HostArch() == ArchARM64 {
		other = ArchAMD64
	}
	assert.NoError(t, checkArch(other, AccelTCG), "tcg runs any architecture")
	assert.ErrorContains(t, checkArch(other, AccelKVM), "use accel tcg", "kvm runs only the host's architecture")
	assert.ErrorContains(t, checkArch("riscv64", AccelTCG), "not one of [amd64 arm64]")
	if _, ok := archSpecs[HostArch()]; ok {
		assert.NoError(t, checkArch(HostArch(), AccelKVM))
	}
}

func TestDefaults(t *testing.T) {
	m := New(Options{Dir: t.TempDir()})
	assert.Equal(t, HostArch(), m.o.Arch, "the host's architecture by default")
	assert.Equal(t, AccelKVM, m.o.Accel)
	assert.Equal(t, "ttyS0", Console(ArchAMD64))
	assert.Equal(t, "ttyAMA0", Console(ArchARM64))
}

func TestFindFirmware(t *testing.T) {
	t.Setenv("PK_METAL_OVMF_CODE", "")
	t.Setenv("PK_METAL_OVMF_VARS", "")
	code, vars, err := findFirmware(ArchARM64, "/fw/CODE.fd", "/fw/VARS.fd")
	require.NoError(t, err)
	assert.Equal(t, [2]string{"/fw/CODE.fd", "/fw/VARS.fd"}, [2]string{code, vars}, "explicit options win")

	t.Setenv("PK_METAL_OVMF_CODE", "/env/CODE.fd")
	t.Setenv("PK_METAL_OVMF_VARS", "/env/VARS.fd")
	code, vars, err = findFirmware(ArchARM64, "", "")
	require.NoError(t, err)
	assert.Equal(t, [2]string{"/env/CODE.fd", "/env/VARS.fd"}, [2]string{code, vars}, "then the environment")

	assert.Equal(t, "/usr/share/AAVMF/AAVMF_CODE.fd", firmwarePairs(ArchARM64)[0][0], "arm64 probes AAVMF")
	assert.Equal(t, "/usr/share/edk2/ovmf/OVMF_CODE.fd", firmwarePairs(ArchAMD64)[0][0], "amd64 probes OVMF")
}

func TestSetMediaReplacesTheSet(t *testing.T) {
	m := New(Options{Dir: t.TempDir()})
	ctx := context.Background()
	require.NoError(t, m.SetMedia(ctx, []sim.Media{{Data: []byte("a")}, {Data: []byte("b")}}))
	require.NoError(t, m.SetMedia(ctx, []sim.Media{{Data: []byte("c")}}))

	files, err := filepath.Glob(filepath.Join(m.o.Dir, "media-*.iso"))
	require.NoError(t, err)
	require.Len(t, files, 1, "a shrinking set removes the stale slots")
	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Equal(t, "c", string(raw))
}

// fakeQMP speaks just enough of the protocol on a unix socket: greeting,
// capabilities, then scripted per-command outcomes.
func fakeQMP(t *testing.T, sock string, fail map[string]string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				fmt.Fprintf(conn, `{"QMP": {"version": {}, "capabilities": []}}`+"\n")
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					req := string(buf[:n])
					replied := false
					for cmd, desc := range fail {
						if strings.Contains(req, cmd) {
							fmt.Fprintf(conn, `{"error": {"class": "GenericError", "desc": %q}}`+"\n", desc)
							replied = true
						}
					}
					if !replied {
						// An asynchronous event first — readReturn must skip it.
						fmt.Fprintf(conn, `{"event": "RESUME", "timestamp": {}}`+"\n")
						fmt.Fprintf(conn, `{"return": {}}`+"\n")
					}
				}
			}()
		}
	}()
}

func TestQMP(t *testing.T) {
	m := New(Options{Dir: t.TempDir()})
	ctx := context.Background()

	t.Run("no-socket", func(t *testing.T) {
		assert.Error(t, m.qmp(ctx, "query-status", nil))
	})

	fakeQMP(t, m.sockPath(), map[string]string{"system_reset": "nope"})

	t.Run("handshake-and-command", func(t *testing.T) {
		assert.NoError(t, m.qmp(ctx, "query-status", nil))
	})
	t.Run("error-reply", func(t *testing.T) {
		err := m.qmp(ctx, "system_reset", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nope")
	})
}

// TestSpawn boots a real qemu process — no disk contents, no KVM (TCG), just
// far enough to prove spawn, QMP control, reset, and teardown against the
// genuine article. Skips where qemu or OVMF are not available.
func TestSpawn(t *testing.T) {
	for _, arch := range Arches() {
		t.Run(arch, func(t *testing.T) { testSpawn(t, arch) })
	}
}

func testSpawn(t *testing.T, arch string) {
	if _, err := exec.LookPath(archSpecs[arch].binary); err != nil {
		t.Skip(archSpecs[arch].binary + " not on PATH")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not on PATH")
	}
	if _, _, err := findFirmware(arch, "", ""); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	m := New(Options{
		Name: "pk-spawn-test", Dir: filepath.Join(t.TempDir(), "vm"),
		Arch: arch, Accel: AccelTCG, MemoryMiB: 256, CPUs: 1, DiskGiB: 1,
	})
	require.NoError(t, m.Ensure(ctx))
	require.NoError(t, m.Start(ctx))
	t.Cleanup(func() { _ = m.Remove(context.Background()) })

	running, err := m.Running(ctx)
	require.NoError(t, err)
	assert.True(t, running)

	assert.NoError(t, m.Start(ctx), "Start on a running VM is a no-op")
	assert.NoError(t, m.Restart(ctx), "reset in place")

	require.NoError(t, m.Stop(ctx))
	running, err = m.Running(ctx)
	require.NoError(t, err)
	assert.False(t, running)
	assert.NoError(t, m.Stop(ctx), "Stop on a stopped VM is a no-op")
}

func TestQEMUBinaryHonoursOverride(t *testing.T) {
	t.Setenv("PK_METAL_QEMU", "/opt/qemu/bin/qemu-kvm")
	if got := qemuBinary(ArchARM64); got != "/opt/qemu/bin/qemu-kvm" {
		t.Fatalf("qemuBinary() = %q, want the PK_METAL_QEMU value", got)
	}
}
