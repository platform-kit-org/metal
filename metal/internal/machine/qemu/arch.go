package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
)

// The CPU architectures the driver runs, named as Go and Talos name them.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// The accelerators: kvm runs the guest on the host CPU and needs a guest of
// the host's own architecture; tcg emulates the guest CPU, any architecture,
// slower.
const (
	AccelKVM = "kvm"
	AccelTCG = "tcg"
)

// Arches lists the architectures the driver runs.
func Arches() []string { return []string{ArchAMD64, ArchARM64} }

// HostArch is the architecture of the machine this process runs on.
func HostArch() string { return runtime.GOARCH }

// archSpec is what differs between two architectures on the qemu command
// line. Everything else (QMP, the disk, the NIC, the boot order) is the same.
type archSpec struct {
	// binary is the system emulator on PATH.
	binary string
	// machine is the -machine value. q35 is a PC with AHCI; virt is the
	// generic Arm board with a PL011 UART (ttyAMA0) and no IDE or AHCI.
	machine string
	// tcgCPU is the -cpu value without kvm. pauth-impdef swaps the
	// architected pointer authentication hash for a cheap one, which makes
	// an emulated arm64 guest several times faster.
	tcgCPU string
	// scsiCD attaches the CDs to a virtio-scsi controller instead of AHCI
	// ports: virt has no IDE, and the UEFI firmware boots from virtio-scsi.
	scsiCD bool
	// console is the kernel's name of the first serial port, which the
	// driver records into ConsoleLog.
	console string
	// firmware are the UEFI firmware pairs (code, variables template) worth
	// probing, in preference order.
	firmware [][2]string
}

var archSpecs = map[string]archSpec{
	ArchAMD64: {
		binary:  "qemu-system-x86_64",
		machine: "q35",
		tcgCPU:  "max",
		console: "ttyS0",
		// Fedora and RHEL (edk2-ovmf), Debian and Ubuntu (ovmf), Arch.
		firmware: [][2]string{
			{"/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
			{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
			{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
			{"/usr/share/edk2-ovmf/x64/OVMF_CODE.fd", "/usr/share/edk2-ovmf/x64/OVMF_VARS.fd"},
		},
	},
	ArchARM64: {
		binary:  "qemu-system-aarch64",
		machine: "virt,gic-version=max",
		tcgCPU:  "max,pauth-impdef=on",
		scsiCD:  true,
		console: "ttyAMA0",
		// Debian and Ubuntu (qemu-efi-aarch64), Fedora and RHEL
		// (edk2-aarch64), Arch (edk2-aarch64). The files are padded to the
		// 64 MiB flash size of the virt board.
		firmware: [][2]string{
			{"/usr/share/AAVMF/AAVMF_CODE.fd", "/usr/share/AAVMF/AAVMF_VARS.fd"},
			{"/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw", "/usr/share/edk2/aarch64/vars-template-pflash.raw"},
			{"/usr/share/edk2/aarch64/QEMU_CODE.fd", "/usr/share/edk2/aarch64/QEMU_VARS.fd"},
		},
	},
}

// Console is the kernel's name of the serial port the driver records for a
// machine of arch (console=<name> on the kernel command line); empty for an
// architecture the driver does not run.
func Console(arch string) string { return archSpecs[arch].console }

// checkArch refuses an architecture the driver does not run, and kvm for a
// guest of another architecture than the host's.
func checkArch(arch, accel string) error {
	if _, ok := archSpecs[arch]; !ok {
		return fmt.Errorf("qemu: architecture %q is not one of %v", arch, Arches())
	}
	if accel == AccelKVM && arch != HostArch() {
		return fmt.Errorf("qemu: kvm runs only machines of the host's architecture (%s), not %s; use accel %s", HostArch(), arch, AccelTCG)
	}
	return nil
}

// qemuBinary names the QEMU system emulator for arch: $PK_METAL_QEMU when
// set, else the emulator on PATH, else, for the host's own architecture,
// RHEL's /usr/libexec/qemu-kvm, which RHEL ships off PATH and under another
// name.
func qemuBinary(arch string) string {
	if b := os.Getenv("PK_METAL_QEMU"); b != "" {
		return b
	}
	binary := archSpecs[arch].binary
	if _, err := exec.LookPath(binary); err == nil {
		return binary
	}
	if arch == HostArch() {
		if _, err := os.Stat("/usr/libexec/qemu-kvm"); err == nil {
			return "/usr/libexec/qemu-kvm"
		}
	}
	return binary
}

// firmwarePairs are the firmware locations probed for arch.
func firmwarePairs(arch string) [][2]string { return slices.Clone(archSpecs[arch].firmware) }
