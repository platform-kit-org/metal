// Package spec holds the TLA+ specifications of the operator's protocols
// (one directory each, model-checked by hack/tlc.sh) and the tests that keep
// them, and the protocol model of internal/controller, in step with the Go
// code they describe.
package spec

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write the current Go fingerprints into the specifications")

// A specification names the Go code it mirrors in comment lines:
//
//	\* go: controller.HostClaimReconciler.bind 1a2b3c4d5e6f
//	\* go-invariants: internal/envtest/staleread_claimhost_test.go
//
// A "go:" line names a function or method of a package under internal/ and
// the fingerprint of its code (comments do not count). A "go-invariants:"
// line names the Go test whose stale-read invariants must have the names of
// the invariants and properties that the specification's configs check.
var (
	goLine   = regexp.MustCompile(`^\\\* go: ([a-z]+)\.([A-Za-z0-9_.]+)(?: ([0-9a-f]{12}))?$`)
	invLine  = regexp.MustCompile(`^\\\* go-invariants: (\S+)$`)
	goInvRe  = regexp.MustCompile(`\.invariant\("([A-Za-z]+)"`)
	cfgNames = regexp.MustCompile(`^(?:INVARIANTS|PROPERTIES)\s+(.*)$`)
)

// TestSpecSync fails when a Go function that a specification mirrors is gone
// or changed since the specification was last checked against it, and when
// the specification and its Go stale-read test check different invariants.
//
// After a change to a mirrored function: read the operator of the
// specification above the "go:" line, change it if the code now does
// something else, run `mise run spec`, then record the new fingerprints:
//
//	go test ./spec -run TestSpecSync -update
func TestSpecSync(t *testing.T) {
	specs, err := filepath.Glob("*/*.tla")
	if err != nil || len(specs) == 0 {
		t.Fatalf("no specifications found: %v", err)
	}
	funcs := map[string]map[string]string{} // package -> name -> fingerprint
	for _, path := range specs {
		if filepath.Base(path) == "MC.tla" {
			continue
		}
		t.Run(path, func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			changed := false
			sc := bufio.NewScanner(bytes.NewReader(src))
			for sc.Scan() {
				line := sc.Text()
				if m := invLine.FindStringSubmatch(line); m != nil {
					checkInvariantNames(t, filepath.Dir(path), filepath.Join("..", m[1]))
				}
				m := goLine.FindStringSubmatch(line)
				if m == nil {
					out.WriteString(line + "\n")
					continue
				}
				pkg, name, want := m[1], m[2], m[3]
				if funcs[pkg] == nil {
					funcs[pkg] = fingerprints(t, filepath.Join("..", "internal", pkg))
				}
				got, ok := funcs[pkg][name]
				switch {
				case !ok:
					t.Errorf("%s names %s.%s, which internal/%s does not have: update the specification", path, pkg, name, pkg)
				case got != want && *update:
					line = fmt.Sprintf(`\* go: %s.%s %s`, pkg, name, got)
					changed = true
				case got != want:
					t.Errorf("%s.%s changed since %s was last checked against it (fingerprint %s, now %s). "+
						"Read the operator below that line, change it if the code now does something else, run `mise run spec`, "+
						"then: go test ./spec -run TestSpecSync -update", pkg, name, path, want, got)
				}
				out.WriteString(line + "\n")
			}
			if changed {
				if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// fingerprints returns, for each top-level function and method of the
// package in dir ("Name" or "Recv.Name"), a hash of its code without comments
// and blank lines.
func fingerprints(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files in %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				typ := fd.Recv.List[0].Type
				if st, ok := typ.(*ast.StarExpr); ok {
					typ = st.X
				}
				if id, ok := typ.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			var b bytes.Buffer
			if err := printer.Fprint(&b, fset, fd); err != nil {
				t.Fatal(err)
			}
			// The parser drops the comments, but a comment line leaves a blank line.
			h := sha256.New()
			for line := range strings.Lines(b.String()) {
				if strings.TrimSpace(line) != "" {
					h.Write([]byte(line))
				}
			}
			out[name] = hex.EncodeToString(h.Sum(nil))[:12]
		}
	}
	return out
}

// checkInvariantNames compares what the configs of dir check (except TypeOK)
// with the invariants the Go stale-read test registers.
func checkInvariantNames(t *testing.T, dir, goTest string) {
	t.Helper()
	var spec []string
	cfgs, _ := filepath.Glob(filepath.Join(dir, "MC*.cfg"))
	for _, cfg := range cfgs {
		if strings.HasSuffix(cfg, "_cache.cfg") {
			continue
		}
		src, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if m := cfgNames.FindStringSubmatch(line); m != nil {
				for _, n := range strings.Fields(m[1]) {
					if n != "TypeOK" && !slices.Contains(spec, n) {
						spec = append(spec, n)
					}
				}
			}
		}
	}
	src, err := os.ReadFile(goTest)
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, m := range goInvRe.FindAllStringSubmatch(string(src), -1) {
		code = append(code, m[1])
	}
	slices.Sort(spec)
	slices.Sort(code)
	if !slices.Equal(spec, code) {
		t.Errorf("%s checks %v, but %s checks %v: give both the same invariants", dir, spec, goTest, code)
	}
}

// TestFingerprintsIgnoreComments: comment lines, also one inside an aligned
// composite literal, and blank lines do not change a fingerprint.
func TestFingerprintsIgnoreComments(t *testing.T) {
	dir := t.TempDir()
	fingerprint := func(body string) string {
		t.Helper()
		src := "package a\n\ntype R struct{}\n\nfunc (r *R) F() T {\n" + body + "}\n"
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		return fingerprints(t, dir)["R.F"]
	}
	base := fingerprint("\tx := T{\n\t\tA:    1,\n\t\tLong: 2,\n\t}\n\treturn x\n")
	commented := fingerprint("\t// Build it.\n\tx := T{\n\t\tA: 1, // one\n\t\t// The long one.\n\t\tLong: 2,\n\t}\n\n\treturn x\n")
	changed := fingerprint("\tx := T{\n\t\tA:    1,\n\t\tLong: 3,\n\t}\n\treturn x\n")
	if commented != base {
		t.Errorf("a comment changed the fingerprint: %s, without comments %s", commented, base)
	}
	if changed == base {
		t.Errorf("a change of the code kept the fingerprint %s", base)
	}
}
