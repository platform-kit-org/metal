package lint_test

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/tools/go/packages"
)

// guardedOwners are the status types whose Phase and BlockedOn summarise the
// conditions. OperationStep also has a Phase, but that one is the outcome of one
// action and the executor writes it directly.
var guardedOwners = map[string]bool{
	"HostStatus": true, "HostClaimStatus": true, "HostOperationStatus": true,
	"ManagementEndpointStatus": true,
}

// TestPhaseWriteSites keeps "conditions are the truth" true as the code grows.
// Phase and BlockedOn are a summary that internal/phase derives from the
// conditions. Anything else that writes them publishes a summary that can
// disagree with its own conditions, and a reconciler that reads them decides
// on a summary instead of on the observations. The check is typed, so it also
// sees writes through an alias (st := &h.Status; st.Phase = x), through a
// pointer to the field, and in a composite literal. A text search sees none of
// these. Test files are out of scope: they build stale fixtures on purpose.
func TestPhaseWriteSites(t *testing.T) {
	root, pkgs := load(t)

	t.Run("the fixture's marked lines are exactly what the check finds", func(t *testing.T) {
		fixture := pkg(pkgs, fixtures+"writesites")
		assert.Equal(t, strs(wanted(t, root, fixture)), strs(checkPhase(root, fixture, false, true)))
	})

	t.Run("the module writes phase only in internal/phase and reconcilers never read it", func(t *testing.T) {
		var bad []finding
		for _, p := range pkgs {
			if strings.HasPrefix(p.PkgPath, fixtures) {
				continue
			}
			allowWrite := p.PkgPath == module+"/internal/phase"
			forbidRead := strings.HasPrefix(p.PkgPath, module+"/internal/controller")
			bad = append(bad, checkPhase(root, p, allowWrite, forbidRead)...)
		}
		assert.Empty(t, strs(bad), "write Phase and BlockedOn only through internal/phase (Stamp*), "+
			"and decide on conditions, not on the phase")
	})

	t.Run("the guard knows every summary status type", func(t *testing.T) {
		// A new top-level status with a Phase field must join guardedOwners, or
		// the check silently ignores it.
		api := pkg(pkgs, module+"/api/v1alpha1")
		var owners []string
		for _, name := range api.Types.Scope().Names() {
			st, ok := api.Types.Scope().Lookup(name).Type().Underlying().(*types.Struct)
			if !ok || !strings.HasSuffix(name, "Status") {
				continue
			}
			for i := range st.NumFields() {
				if st.Field(i).Name() == "BlockedOn" {
					owners = append(owners, name)
				}
			}
		}
		assert.ElementsMatch(t, slices.Collect(maps.Keys(guardedOwners)), owners)
	})
}

// checkPhase reports every touch of a guarded field in p's non-test files
// that the policy forbids: any write unless allowWrite, and any read if
// forbidRead. A finding is a "write" or a "read".
func checkPhase(root string, p *packages.Package, allowWrite, forbidRead bool) []finding {
	var out []finding
	report := func(n ast.Node, write bool) {
		switch {
		case write && !allowWrite:
			out = append(out, at(root, p, n, "write"))
		case !write && forbidRead:
			out = append(out, at(root, p, n, "read"))
		}
	}
	for _, file := range p.Syntax {
		if strings.HasSuffix(p.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}
		// First pass: the selectors in a written position.
		written := map[*ast.SelectorExpr]bool{}
		mark := func(e ast.Expr) {
			if sel, ok := ast.Unparen(e).(*ast.SelectorExpr); ok {
				written[sel] = true
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					mark(l)
				}
			case *ast.IncDecStmt:
				mark(n.X)
			case *ast.RangeStmt:
				mark(n.Key)
				mark(n.Value)
			case *ast.UnaryExpr:
				// A pointer to the field is a write waiting to happen.
				if n.Op == token.AND {
					mark(n.X)
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if s := p.TypesInfo.Selections[n]; s != nil && s.Kind() == types.FieldVal && guarded(s.Recv(), n.Sel.Name) {
					report(n, written[n])
				}
			case *ast.CompositeLit:
				for _, el := range n.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && guarded(p.TypesInfo.TypeOf(n), key.Name) {
						report(kv, true)
					}
				}
			}
			return true
		})
	}
	return out
}

// guarded: the field named field of type t (or *t) is a summary Phase or
// BlockedOn of an API status type.
func guarded(t types.Type, field string) bool {
	if field != "Phase" && field != "BlockedOn" {
		return false
	}
	named, ok := types.Unalias(deref(t)).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return strings.HasPrefix(named.Obj().Pkg().Path(), module+"/api/") && guardedOwners[named.Obj().Name()]
}
