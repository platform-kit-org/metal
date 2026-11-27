// crddiff compares the CRDs of two directories and says what the change of
// the operator's API does to the objects a cluster already stores and to the
// manifests people apply. It reads each <group>_<plural>.yaml of the two
// directories and walks the schema of each version.
//
// A finding is one of:
//
//	breaking  an object or a manifest that was valid before can fail now:
//	          a kind, version or field that goes, a new required field
//	          without a default, a changed type, an enum value that goes,
//	          a tighter limit, a new CEL rule;
//	notice    valid objects stay valid, but the meaning can change: a new
//	          or changed default, a field of the status that goes;
//	added     a new kind, version or field.
//
// The API is v1alpha1 and may break; the point is that a break is seen,
// written down, and has an upgrade note.
//
// usage: go run ./hack/crddiff [-strict] <old-dir> <new-dir>
//
// With -strict the exit status is 1 if a finding is breaking.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// Severity of a finding.
type Severity string

const (
	Breaking Severity = "breaking"
	Notice   Severity = "notice"
	Added    Severity = "added"
)

// Finding is one change of one kind.
type Finding struct {
	Kind     string // Kind/version, e.g. HostClaim/v1alpha1
	Path     string // field path, e.g. spec.os.nodeRef; empty for the kind itself
	Severity Severity
	What     string
}

func main() {
	strict := flag.Bool("strict", false, "exit 1 if a finding is breaking")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: crddiff [-strict] <old-dir> <new-dir>")
		os.Exit(2)
	}
	old, err := readDir(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "crddiff:", err)
		os.Exit(2)
	}
	cur, err := readDir(flag.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "crddiff:", err)
		os.Exit(2)
	}
	findings := Compare(old, cur)
	report(os.Stdout, findings)
	if *strict && slices.ContainsFunc(findings, func(f Finding) bool { return f.Severity == Breaking }) {
		os.Exit(1)
	}
}

// readDir reads the CRDs of a directory, by their name. A missing directory
// has none: the CRDs of a commit from before the operator had any.
func readDir(dir string) (map[string]*apiextensionsv1.CustomResourceDefinition, error) {
	out := map[string]*apiextensionsv1.CustomResourceDefinition{}
	files, err := filepath.Glob(filepath.Join(dir, "*_*.yaml"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if crd.Kind != "CustomResourceDefinition" {
			continue
		}
		out[crd.Name] = crd
	}
	return out, nil
}

// Compare lists the findings, ordered by kind, path and text.
func Compare(old, cur map[string]*apiextensionsv1.CustomResourceDefinition) []Finding {
	var out []Finding
	for name, o := range old {
		c, ok := cur[name]
		if !ok {
			out = append(out, Finding{Kind: o.Spec.Names.Kind, Severity: Breaking,
				What: "the kind goes: the API server deletes each stored object of it with the CRD"})
			continue
		}
		out = append(out, compareCRD(o, c)...)
	}
	for name, c := range cur {
		if _, ok := old[name]; !ok {
			out = append(out, Finding{Kind: c.Spec.Names.Kind, Severity: Added, What: "new kind"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.What < b.What
	})
	return out
}

func compareCRD(o, c *apiextensionsv1.CustomResourceDefinition) []Finding {
	var out []Finding
	kind := c.Spec.Names.Kind
	if o.Spec.Scope != c.Spec.Scope {
		out = append(out, Finding{Kind: kind, Severity: Breaking, What: fmt.Sprintf("scope %s becomes %s", o.Spec.Scope, c.Spec.Scope)})
	}
	for _, ov := range o.Spec.Versions {
		i := slices.IndexFunc(c.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool { return v.Name == ov.Name })
		kv := kind + "/" + ov.Name
		if i < 0 {
			out = append(out, Finding{Kind: kv, Severity: Breaking, What: "the version goes"})
			continue
		}
		cv := c.Spec.Versions[i]
		if ov.Served && !cv.Served {
			out = append(out, Finding{Kind: kv, Severity: Breaking, What: "the version is no longer served"})
		}
		if ov.Storage != cv.Storage {
			out = append(out, Finding{Kind: kv, Severity: Notice, What: fmt.Sprintf("storage %t becomes %t: stored objects need a migration", ov.Storage, cv.Storage)})
		}
		if ov.Schema == nil || cv.Schema == nil || ov.Schema.OpenAPIV3Schema == nil || cv.Schema.OpenAPIV3Schema == nil {
			continue
		}
		out = append(out, compareSchema(kv, "", ov.Schema.OpenAPIV3Schema, cv.Schema.OpenAPIV3Schema)...)
	}
	for _, cv := range c.Spec.Versions {
		if !slices.ContainsFunc(o.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool { return v.Name == cv.Name }) {
			out = append(out, Finding{Kind: kind + "/" + cv.Name, Severity: Added, What: "new version"})
		}
	}
	return out
}

// inStatus: a field under status, which only the operator writes.
func inStatus(path string) bool { return path == "status" || strings.HasPrefix(path, "status.") }

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func compareSchema(kv, path string, o, c *apiextensionsv1.JSONSchemaProps) []Finding {
	var out []Finding
	add := func(s Severity, format string, args ...any) {
		out = append(out, Finding{Kind: kv, Path: path, Severity: s, What: fmt.Sprintf(format, args...)})
	}
	if o.Type != c.Type {
		add(Breaking, "type %q becomes %q", o.Type, c.Type)
		return out // the children of another type do not compare
	}
	if removed := missing(enumValues(o), enumValues(c)); len(c.Enum) > 0 && len(removed) > 0 {
		add(Breaking, "enum loses %s", strings.Join(removed, ", "))
	}
	if added := missing(enumValues(c), enumValues(o)); len(o.Enum) > 0 && len(added) > 0 {
		add(Added, "enum gains %s", strings.Join(added, ", "))
	}
	if len(o.Enum) == 0 && len(c.Enum) > 0 {
		add(Breaking, "new enum: %s", strings.Join(enumValues(c), ", "))
	}
	if o.Pattern != c.Pattern && c.Pattern != "" {
		add(Breaking, "pattern %q becomes %q", o.Pattern, c.Pattern)
	}
	tighter(add, "maxLength", o.MaxLength, c.MaxLength, true)
	tighter(add, "minLength", o.MinLength, c.MinLength, false)
	tighter(add, "maxItems", o.MaxItems, c.MaxItems, true)
	tighter(add, "minItems", o.MinItems, c.MinItems, false)
	tighter(add, "maxProperties", o.MaxProperties, c.MaxProperties, true)
	tighterFloat(add, "maximum", o.Maximum, c.Maximum, true)
	tighterFloat(add, "minimum", o.Minimum, c.Minimum, false)
	for _, r := range c.XValidations {
		if !slices.ContainsFunc(o.XValidations, func(x apiextensionsv1.ValidationRule) bool { return x.Rule == r.Rule }) {
			add(Breaking, "new rule: %s", r.Rule)
		}
	}
	if string(defaultJSON(o)) != string(defaultJSON(c)) {
		switch {
		case o.Default == nil:
			add(Notice, "new default %s", defaultJSON(c))
		case c.Default == nil:
			add(Notice, "the default %s goes", defaultJSON(o))
		default:
			add(Notice, "default %s becomes %s", defaultJSON(o), defaultJSON(c))
		}
	}

	for name, op := range o.Properties {
		p := join(path, name)
		cp, ok := c.Properties[name]
		if !ok {
			if inStatus(p) {
				out = append(out, Finding{Kind: kv, Path: p, Severity: Notice, What: "removed from the status: the API server drops the stored value at the next write"})
			} else if c.XPreserveUnknownFields == nil || !*c.XPreserveUnknownFields {
				out = append(out, Finding{Kind: kv, Path: p, Severity: Breaking, What: "removed: a manifest that sets it is refused or loses it"})
			}
			continue
		}
		out = append(out, compareSchema(kv, p, &op, &cp)...)
	}
	for name, cp := range c.Properties {
		if _, ok := o.Properties[name]; ok {
			continue
		}
		p := join(path, name)
		if slices.Contains(c.Required, name) && cp.Default == nil && !inStatus(p) {
			out = append(out, Finding{Kind: kv, Path: p, Severity: Breaking, What: "new required field without a default: each update of a stored object fails until it is set"})
			continue
		}
		out = append(out, Finding{Kind: kv, Path: p, Severity: Added, What: "new field"})
	}
	for _, name := range c.Required {
		if _, existed := o.Properties[name]; existed && !slices.Contains(o.Required, name) && !inStatus(join(path, name)) {
			out = append(out, Finding{Kind: kv, Path: join(path, name), Severity: Breaking, What: "becomes required"})
		}
	}
	if o.Items != nil && c.Items != nil && o.Items.Schema != nil && c.Items.Schema != nil {
		out = append(out, compareSchema(kv, path+"[]", o.Items.Schema, c.Items.Schema)...)
	}
	if o.AdditionalProperties != nil && c.AdditionalProperties != nil && o.AdditionalProperties.Schema != nil && c.AdditionalProperties.Schema != nil {
		out = append(out, compareSchema(kv, path+"{}", o.AdditionalProperties.Schema, c.AdditionalProperties.Schema)...)
	}
	return out
}

func enumValues(s *apiextensionsv1.JSONSchemaProps) []string {
	var out []string
	for _, e := range s.Enum {
		out = append(out, string(e.Raw))
	}
	return out
}

// missing lists the values of a that b does not have.
func missing(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

func defaultJSON(s *apiextensionsv1.JSONSchemaProps) []byte {
	if s.Default == nil {
		return nil
	}
	return s.Default.Raw
}

// tighter reports a limit that accepts less than before. upper is true for
// a maximum (smaller is tighter), false for a minimum (larger is tighter).
func tighter(add func(Severity, string, ...any), name string, o, c *int64, upper bool) {
	switch {
	case c == nil:
		return
	case o == nil:
		add(Breaking, "new %s %d", name, *c)
	case upper && *c < *o, !upper && *c > *o:
		add(Breaking, "%s %d becomes %d", name, *o, *c)
	}
}

func tighterFloat(add func(Severity, string, ...any), name string, o, c *float64, upper bool) {
	switch {
	case c == nil:
		return
	case o == nil:
		add(Breaking, "new %s %g", name, *c)
	case upper && *c < *o, !upper && *c > *o:
		add(Breaking, "%s %g becomes %g", name, *o, *c)
	}
}

// report writes the findings as a Markdown list, one section per kind, and
// a line when there is nothing to say.
func report(w io.Writer, findings []Finding) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "crddiff: the CRDs do not change.")
		return
	}
	counts := map[Severity]int{}
	for _, f := range findings {
		counts[f.Severity]++
	}
	fmt.Fprintf(w, "crddiff: %d breaking, %d notice, %d added\n", counts[Breaking], counts[Notice], counts[Added])
	kind := ""
	for _, f := range findings {
		if f.Kind != kind {
			kind = f.Kind
			fmt.Fprintf(w, "\n### %s\n\n", kind)
		}
		path := f.Path
		if path == "" {
			path = "(kind)"
		}
		fmt.Fprintf(w, "- **%s** `%s`: %s\n", f.Severity, path, f.What)
	}
}
