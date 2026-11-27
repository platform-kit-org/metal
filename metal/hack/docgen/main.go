// docgen writes the reference data of the documentation site as JSON: one file
// per CRD with every field and its validation rules, one file per pk-metal
// command with its flags, the RBAC roles and the markers they come from, the
// named controllers, the HostOperation actions, and the phase, condition,
// reason and annotation constants of the API. The site renders its reference
// tables from these files, so a table cannot disagree with the code. Runs as
// part of `mise run generate`.
//
// usage (from metal/): go run ./hack/docgen
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	tmp, err := os.MkdirTemp("", "docgen-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(tmp)
	pkMetal := filepath.Join(tmp, "pk-metal")
	build := exec.Command("go", "build", "-o", pkMetal, "./cmd/pk-metal")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fail(fmt.Errorf("build pk-metal: %w", err))
	}
	cfg := config{
		root:          "..",
		out:           "../docs/src/data/generated",
		crdDir:        "config/crd",
		rbacDir:       "config/rbac",
		controllerDir: "internal/controller",
		generatedRole: "pk-metal-manager",
		apiDirs:       []string{"api/v1alpha1", "api/os/v1alpha1"},
		scanDirs:      []string{"api", "internal", "cmd"},
	}
	if err := run(cfg, execHelp(pkMetal)); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
	os.Exit(1)
}

// config names the inputs (relative to the working directory, metal/) and the
// output directory.
type config struct {
	root          string   // the repository root: source paths in the output are relative to it
	out           string   // docgen owns it: it removes the stale JSON files in it
	crdDir        string   // the generated CRD manifests
	rbacDir       string   // the RBAC manifests
	controllerDir string   // the controller package: its Named(...) controllers
	generatedRole string   // the role that controller-gen writes from the +kubebuilder:rbac markers
	apiDirs       []string // the API packages: actions, phases and conditions
	scanDirs      []string // the trees with +kubebuilder:rbac markers and metal.platform-kit.org/os.platform-kit.org keys
}

// run reads every input first and writes only when all of them parsed, so a
// failure never leaves a half-written data directory behind.
func run(cfg config, help helpFunc) error {
	files := map[string]any{}

	crds, err := readCRDs(cfg)
	if err != nil {
		return err
	}
	for _, c := range crds {
		files[filepath.Join("crds", lowerKind(c.Kind)+".json")] = c
	}

	index, cmds, err := readCLI(help)
	if err != nil {
		return fmt.Errorf("pk-metal help: %w", err)
	}
	files[filepath.Join("cli", "index.json")] = index
	for _, c := range cmds {
		files[filepath.Join("cli", c.Name+".json")] = c
	}

	rbac, err := readRBAC(cfg)
	if err != nil {
		return err
	}
	files["rbac.json"] = rbac

	ctrls, err := readControllers(cfg)
	if err != nil {
		return err
	}
	files["controllers.json"] = ctrls

	src, err := readGoSources(cfg)
	if err != nil {
		return err
	}
	if err := src.actions.fillFromCRD(crds); err != nil {
		return err
	}
	files["actions.json"] = src.actions
	files["phases.json"] = src.phases
	files["conditions.json"] = src.conditions
	files["keys.json"] = src.keys

	return writeAll(cfg.out, files)
}
