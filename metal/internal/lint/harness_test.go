package lint_test

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	module = "github.com/dennisklein/platform-kit/metal"
	// fixtures is the prefix of the fixture packages. Only load loads them;
	// each check skips them when it runs on the module.
	fixtures = module + "/internal/lint/testdata/"
)

// finding is one site that breaks a rule, or one marked line of a fixture.
type finding struct {
	Pos  string // file:line, relative to the module root
	What string // the kind of the site, as a "want" marker names it
}

func (f finding) String() string { return f.Pos + " " + f.What }

// at is the finding of kind what at n.
func at(root string, p *packages.Package, n ast.Node, what string) finding {
	pos := p.Fset.Position(n.Pos())
	rel, _ := filepath.Rel(root, pos.Filename)
	return finding{Pos: fmt.Sprintf("%s:%d", rel, pos.Line), What: what}
}

// loadOnce loads the module and the fixtures with types. The checks share
// one load, because each load type-checks the whole module.
var loadOnce = sync.OnceValues(func() ([]*packages.Package, error) {
	cfg := &packages.Config{
		Dir:  "../..",
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}
	return packages.Load(cfg, "./...", fixtures+"uncached", fixtures+"writesites")
})

// load returns the module root and the loaded packages, and fails on a
// package that does not load.
func load(t *testing.T) (root string, pkgs []*packages.Package) {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	pkgs, err = loadOnce()
	require.NoError(t, err)
	for _, p := range pkgs {
		for _, e := range p.Errors {
			require.Failf(t, "load", "%s: %v", p.PkgPath, e)
		}
		// go list reads the sources in a child process, which go test's
		// result cache does not see. Stat them here so that editing any file
		// of the module, or adding one to a package, invalidates a cached pass.
		for _, f := range p.GoFiles {
			_, _ = os.Stat(f)
			_, _ = os.ReadDir(filepath.Dir(f))
		}
	}
	return root, pkgs
}

// pkg is the loaded package with the import path path.
func pkg(pkgs []*packages.Package, path string) *packages.Package {
	return pkgs[slices.IndexFunc(pkgs, func(p *packages.Package) bool { return p.PkgPath == path })]
}

var wantRe = regexp.MustCompile(`// want (\S+)$`)

// wanted reads the "want" markers of a fixture package.
func wanted(t *testing.T, root string, p *packages.Package) []finding {
	var out []finding
	for _, name := range p.GoFiles {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		rel, _ := filepath.Rel(root, name)
		for i, line := range strings.Split(string(data), "\n") {
			if m := wantRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				out = append(out, finding{Pos: fmt.Sprintf("%s:%d", rel, i+1), What: m[1]})
			}
		}
	}
	return out
}

func strs(fs []finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.String())
	}
	slices.Sort(out)
	return out
}

func deref(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}
