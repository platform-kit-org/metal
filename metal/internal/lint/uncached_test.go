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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	module         = "github.com/dennisklein/platform-kit/metal"
	clientPkg      = "sigs.k8s.io/controller-runtime/pkg/client"
	cachePkg       = "sigs.k8s.io/controller-runtime/pkg/cache"
	controllerutil = "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// The directives. A Go directive has no space after the slashes, and gofmt
// keeps it at the end of the doc comment.
const (
	// uncachedDirective marks a function that reads the inputs of a decision
	// that a later pass cannot undo: it reads them from the API server.
	uncachedDirective = "//pk:uncached"
	// cachedOKDirective marks a function that removes a finalizer on cached
	// reads, and says why that is safe.
	cachedOKDirective = "//pk:cached-ok"
)

// mustBeUncached are the functions whose cached reads let a decision stand
// on an old world: a second operation or rollout started, a finalizer came
// off, a password or a key was written twice. Each has a stale-cache test;
// this list keeps the directive, and so the check, on them.
var mustBeUncached = []string{
	"EndpointConvergenceReconciler.credentials",
	"EndpointConvergenceReconciler.ensureIdentityInFlight",
	"HostClaimReconciler.bootRanUncached",
	"HostClaimReconciler.etcdChangeInFlight",
	"HostClaimReconciler.liveBoundHosts",
	"HostOperationReconciler.activePredecessorUncached",
	"HostOperationReconciler.recordBootStart",
	"ProtectionReconciler.liveHosts",
	"RolloutProposer.clusterRollouts",
	"TalosNodeMemberReconciler.blocked",
	"TalosRolloutReconciler.liveOperation",
	"TalosRolloutReconciler.otherHolder",
}

// finding is one site that breaks the rule.
type finding struct {
	Pos  string // file:line, relative to the module root
	What string // cached or unmarked
}

func (f finding) String() string { return f.Pos + " " + f.What }

// TestDecisionsReadTheAPIServer keeps the decisions that a later pass cannot
// undo on fresh reads. The informer cache can be late: a pass that reads it
// sees the world of a moment ago. Most decisions are safe on that, because
// the write carries the resourceVersion of what was read, or the next pass
// corrects it. Some are not: they read one kind and then act on another, and
// the act stays. Those read through the API reader.
//
// Two rules:
//
//   - A function marked //pk:uncached calls Get and List only on a
//     client.Reader, never on a client.Client or a cache: those read the
//     cache. A call through a struct that embeds client.Client counts too.
//   - A function that calls controllerutil.RemoveFinalizer is marked
//     //pk:uncached, or //pk:cached-ok with the reason that its cached
//     reads are safe. A finalizer that came off does not come back.
//
// The check sees one function body at a time, closures included. It does not
// follow calls. Test files are out of scope.
func TestDecisionsReadTheAPIServer(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	fixture := module + "/internal/lint/testdata/uncached"
	pkgs := load(t, root, "./...", fixture)

	t.Run("the fixture's marked lines are exactly what the check finds", func(t *testing.T) {
		p := pkgs[slices.IndexFunc(pkgs, func(p *packages.Package) bool { return p.PkgPath == fixture })]
		got, _ := checkUncached(root, p)
		assert.Equal(t, strs(wanted(t, root, p)), strs(got))
	})

	var marked []string
	t.Run("the module reads the inputs of such decisions from the API server", func(t *testing.T) {
		var bad []finding
		for _, p := range pkgs {
			if p.PkgPath == fixture {
				continue
			}
			got, m := checkUncached(root, p)
			bad = append(bad, got...)
			marked = append(marked, m...)
		}
		assert.Empty(t, strs(bad), "read through the API reader (r.reader(), APIReader) in a //pk:uncached function; "+
			"mark a function that removes a finalizer //pk:uncached, or //pk:cached-ok with the reason")
	})

	t.Run("the known decisions stay marked", func(t *testing.T) {
		for _, name := range mustBeUncached {
			assert.Contains(t, marked, name, "a fixed stale-cache race lost its //pk:uncached")
		}
	})
}

// load loads the patterns with types, and fails on a package that does not
// load.
func load(t *testing.T, root string, patterns ...string) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Dir:  root,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	require.NoError(t, err)
	for _, p := range pkgs {
		for _, e := range p.Errors {
			require.Failf(t, "load", "%s: %v", p.PkgPath, e)
		}
		// go list reads the sources in a child process, which go test's
		// result cache does not see. Stat them here so that editing any file
		// of the module invalidates a cached pass.
		for _, f := range p.GoFiles {
			_, _ = os.Stat(f)
			_, _ = os.ReadDir(filepath.Dir(f))
		}
	}
	return pkgs
}

// checkUncached reports the sites of p's non-test files that break a rule,
// and names each function marked //pk:uncached as Recv.Method.
func checkUncached(root string, p *packages.Package) (out []finding, marked []string) {
	report := func(n ast.Node, what string) {
		pos := p.Fset.Position(n.Pos())
		rel, _ := filepath.Rel(root, pos.Filename)
		out = append(out, finding{Pos: fmt.Sprintf("%s:%d", rel, pos.Line), What: what})
	}
	for _, file := range p.Syntax {
		if strings.HasSuffix(p.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			uncached, cachedOK := directives(fd.Doc)
			if uncached {
				marked = append(marked, funcName(fd))
			}
			removes := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if fn, ok := p.TypesInfo.Uses[sel.Sel].(*types.Func); ok && fn.Pkg() != nil &&
					fn.Pkg().Path() == controllerutil && fn.Name() == "RemoveFinalizer" {
					removes = true
				}
				if uncached && cachedRead(p.TypesInfo.Selections[sel]) {
					report(call, "cached")
				}
				return true
			})
			if removes && !uncached && !cachedOK {
				report(fd.Name, "unmarked")
			}
		}
	}
	return out, marked
}

// directives reads the two directives off a doc comment. A //pk:cached-ok
// without a reason does not count.
func directives(doc *ast.CommentGroup) (uncached, cachedOK bool) {
	if doc == nil {
		return false, false
	}
	for _, c := range doc.List {
		switch {
		case c.Text == uncachedDirective:
			uncached = true
		case strings.HasPrefix(c.Text, cachedOKDirective+" ") && strings.TrimSpace(strings.TrimPrefix(c.Text, cachedOKDirective)) != "":
			cachedOK = true
		}
	}
	return uncached, cachedOK
}

// cachedRead: s selects the Get or List of controller-runtime's client on a
// client.Client or a cache, directly or through an embedded field.
func cachedRead(s *types.Selection) bool {
	if s == nil || s.Kind() != types.MethodVal {
		return false
	}
	m := s.Obj()
	if m.Pkg() == nil || m.Pkg().Path() != clientPkg || (m.Name() != "Get" && m.Name() != "List") {
		return false
	}
	t := s.Recv()
	idx := s.Index()
	for _, i := range idx[:len(idx)-1] {
		st, ok := deref(t).Underlying().(*types.Struct)
		if !ok {
			return false
		}
		t = st.Field(i).Type()
		if caches(t) {
			return true
		}
	}
	return caches(t)
}

// caches: t is controller-runtime's client.Client, client.WithWatch or
// cache.Cache. Their reads go to the informer cache when the manager builds
// them; a client.Reader is what the API reader is.
func caches(t types.Type) bool {
	named, ok := types.Unalias(deref(t)).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	switch named.Obj().Pkg().Path() + "." + named.Obj().Name() {
	case clientPkg + ".Client", clientPkg + ".WithWatch", cachePkg + ".Cache":
		return true
	}
	return false
}

func deref(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// funcName is Recv.Method for a method, the name for a function.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	rt := fd.Recv.List[0].Type
	if star, ok := rt.(*ast.StarExpr); ok {
		rt = star.X
	}
	if id, ok := rt.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

var wantRe = regexp.MustCompile(`// want (cached|unmarked)$`)

// wanted reads the "want" markers of the fixture.
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
