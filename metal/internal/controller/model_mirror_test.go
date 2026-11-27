package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mirrored are the functions whose control flow the protocol model
// (protocol_model_test.go) copies by hand, branch by branch, with a hash of
// their code as of the last time the model was brought in line. Comments
// and layout do not count.
//
// A change to one of them fails TestModelMirrorsTheCode. Then read the
// change, and bring the pass of the model that mirrors it in line, or
// decide that the change does not touch the protocol. Run TestProtocolModel,
// and write the new hash here.
var mirrored = map[string]string{
	"HostClaimReconciler.Reconcile":     "fba19fb46be14563",
	"HostClaimReconciler.finishRelease": "0988f8ac95640f6b",
	"HostClaimReconciler.handedOff":     "fb248396eee96748",
	"HostClaimReconciler.provision":     "0f523fc3a14f7f4b",
	"HostClaimReconciler.release":       "1a276ff648be969f",
	"HostOperationReconciler.Reconcile": "2e6a1caf224249ef",
}

// TestModelMirrorsTheCode fails when a function that the protocol model
// mirrors changed and the model was not looked at. Without it, the model
// goes on proving the protocol of an older version of the code.
func TestModelMirrorsTheCode(t *testing.T) {
	got := codeHashes(t, ".")
	for name, want := range mirrored {
		have, ok := got[name]
		require.Truef(t, ok, "%s is gone: update the model and the list", name)
		assert.Equalf(t, want, have, "%s changed since the protocol model last matched it. "+
			"Bring its mirror in protocol_model_test.go in line, run TestProtocolModel, then set its hash in mirrored to %q", name, have)
	}
}

// codeHashes hashes the code of each method in the package's non-test
// files, by Recv.Method. The files are parsed without comments and printed
// with gofmt, so only a change of the code changes a hash.
func codeHashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			rt := fd.Recv.List[0].Type
			if star, ok := rt.(*ast.StarExpr); ok {
				rt = star.X
			}
			id, ok := rt.(*ast.Ident)
			if !ok {
				continue
			}
			var buf bytes.Buffer
			require.NoError(t, format.Node(&buf, fset, fd))
			// A dropped comment can leave a blank line behind.
			var code []string
			for line := range strings.Lines(buf.String()) {
				if strings.TrimSpace(line) != "" {
					code = append(code, line)
				}
			}
			sum := sha256.Sum256([]byte(strings.Join(code, "")))
			out[id.Name+"."+fd.Name.Name] = hex.EncodeToString(sum[:8])
		}
	}
	return out
}

func TestCodeHashesIgnoreComments(t *testing.T) {
	dir := t.TempDir()
	write := func(src string) map[string]string {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600))
		return codeHashes(t, dir)
	}
	base := write("package a\n\ntype R struct{}\n\nfunc (r *R) F() int {\n\treturn 1\n}\n")
	commented := write("package a\n\ntype R struct{}\n\n// F says.\nfunc (r *R) F() int {\n\t// one\n\treturn   1\n}\n")
	changed := write("package a\n\ntype R struct{}\n\nfunc (r *R) F() int {\n\treturn 2\n}\n")
	assert.Equal(t, base["R.F"], commented["R.F"], "comments and layout do not count")
	assert.NotEqual(t, base["R.F"], changed["R.F"])
}
