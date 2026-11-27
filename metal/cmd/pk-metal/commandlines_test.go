package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-clikit/cobratree/cobratreetest"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// repo is the root of the repository, seen from this package.
const repo = "../../.."

// commandLinesToday is the number of pk-metal command lines in each place
// that TestCommandLinesMatchTheTree found when it was written. Fewer means
// that it no longer finds some of them.
var commandLinesToday = map[string]int{"pages": 267, "scripts": 30, "alerts": 5, "help": 77}

// TestCommandLinesMatchTheTree checks each pk-metal command line that a
// person or a script copies against the command tree: those of the pages
// and the runbooks, of the lab scripts and mise.toml, of the alert rules,
// and the synopsis and the examples in the help. An unknown command or flag
// fails the test.
func TestCommandLinesMatchTheTree(t *testing.T) {
	root := newRoot(noCluster)
	found := map[string]int{}
	check := func(place, name, text string) {
		t.Run(name, func(t *testing.T) { found[place] += cobratreetest.CheckCommandLines(t, root, text) })
	}

	require.NoError(t, filepath.WalkDir(filepath.Join(repo, "docs/src/content"), func(p string, _ fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".mdx") {
			check("pages", rel(p), pageCommands(read(t, p)))
		}
		return err
	}))

	scripts, err := filepath.Glob(filepath.Join(repo, "lab/*.sh"))
	require.NoError(t, err)
	for _, p := range append(scripts, filepath.Join(repo, "mise.toml")) {
		check("scripts", rel(p), scriptCommands(read(t, p)))
	}

	for name, text := range alertTexts(t, read(t, filepath.Join(repo, "metal/config/prometheus/rules.yaml"))) {
		check("alerts", "rules.yaml/"+name, proseCommands(text))
	}

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.HasParent() {
			check("help", c.CommandPath()+" use", synopsis(c.Parent().CommandPath()+" "+c.Use))
		}
		if c.Example != "" {
			check("help", c.CommandPath()+" example", c.Example)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	t.Logf("checked pk-metal command lines: %v", found)
	for place, n := range commandLinesToday {
		assert.GreaterOrEqual(t, found[place], n, "the test finds fewer command lines in the %s than it did", place)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func rel(path string) string {
	r, err := filepath.Rel(repo, path)
	if err != nil {
		return path
	}
	return r
}

var (
	fence    = regexp.MustCompile("^\\s*```(\\S*)")
	codeSpan = regexp.MustCompile("`(pk-metal [^`]*)`")
	// angled matches a placeholder such as <file>, which a shell would read
	// as a redirection.
	angled = regexp.MustCompile(`<([^<>\s]+)>`)
	// optional matches what a synopsis writes around optional words and
	// choices: [--node name], (<snapshot> | --in <file.age>).
	optional = regexp.MustCompile(`[\[\]()]| \| `)
)

// placeholders writes each <placeholder> of a command line in capitals, the
// form that a check reads as a placeholder and not as a redirection.
func placeholders(line string) string {
	return angled.ReplaceAllStringFunc(line, func(m string) string { return strings.ToUpper(m[1 : len(m)-1]) })
}

// synopsis writes a synopsis as a command line, so that the check looks at
// each flag that it names, the optional ones too.
func synopsis(line string) string {
	return optional.ReplaceAllString(placeholders(line), " ")
}

// pageCommands returns the command lines of a page, each on the line of the
// page where it is, so that a failure names the right line: the lines of
// its sh and powershell code blocks, the "$ " lines of its console blocks and of its
// Terminal sessions, and its code spans that start with "pk-metal ". The
// output in a block, and the text, are no command lines.
func pageCommands(page string) string {
	lines := strings.Split(page, "\n")
	out := make([]string, len(lines))
	block := "" // the language of the code block the line is in, "" outside
	inBlock, session, continued := false, false, false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch m := fence.FindStringSubmatch(line); {
		case m != nil:
			inBlock, block = !inBlock, m[1]
			continue
		case !inBlock && strings.HasPrefix(trimmed, "text={`"):
			session = true
			continue
		case session && strings.HasPrefix(trimmed, "`}"):
			session = false
			continue
		}
		switch {
		case inBlock && (block == "sh" || block == "powershell"):
			out[i] = placeholders(line)
		case inBlock && block == "console", session:
			// A command line starts with the prompt and goes on while its
			// lines end with a backslash.
			if strings.HasPrefix(trimmed, "$ ") || continued {
				out[i] = placeholders(trimmed)
			}
			continued = (strings.HasPrefix(trimmed, "$ ") || continued) && strings.HasSuffix(trimmed, `\`)
		case !inBlock:
			var spans []string
			for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
				spans = append(spans, synopsis(m[1]))
			}
			out[i] = strings.Join(spans, " ; ")
		}
	}
	return strings.Join(out, "\n")
}

var (
	// binary matches the ways in which the scripts name the program.
	binary = regexp.MustCompile(`"\$root/metal/bin/pk-metal"|"\$cli"|"\$binary"|metal/bin/pk-metal`)
	// prefix matches what goes in front of the program in a command line of
	// a script: variables for it, and the words of the shell that run it.
	prefix = regexp.MustCompile(`(?m)(^|\s|\$\()(?:(?:[A-Z_]+=\S+|exec|if|until|!|setsid|nohup)\s+)+pk-metal `)
	// heredoc matches the start of a here-document, whose lines are data.
	heredoc = regexp.MustCompile(`<<-?\s*'?"?(\w+)`)
	// text matches the start of a string of more than one line that a
	// variable gets, such as a help text: its lines are data too.
	text = regexp.MustCompile(`^\s*\w+="[^"]*$`)
)

// scriptCommands returns the text of a script with its calls of the program
// written as "pk-metal ...", and its here-documents and the strings of more
// than one line that it keeps in variables blank.
func scriptCommands(script string) string {
	lines := strings.Split(script, "\n")
	end := ""
	for i, line := range lines {
		switch {
		case end == `"`:
			if strings.Contains(line, `"`) {
				end = ""
			}
			lines[i] = ""
		case end != "":
			if strings.TrimSpace(line) == end {
				end = ""
			}
			lines[i] = ""
		default:
			if m := heredoc.FindStringSubmatch(line); m != nil {
				end = m[1]
			} else if text.MatchString(line) {
				end = `"`
			}
			line = binary.ReplaceAllString(line, "pk-metal")
			lines[i] = prefix.ReplaceAllString(line, "${1}pk-metal ")
		}
	}
	return strings.Join(lines, "\n")
}

// alertTexts returns the summary and the description of each alert rule, by
// the alert's name.
func alertTexts(t *testing.T, rules string) map[string]string {
	t.Helper()
	var doc struct {
		Spec struct {
			Groups []struct {
				Rules []struct {
					Alert       string            `json:"alert"`
					Annotations map[string]string `json:"annotations"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(rules), &doc))
	texts := map[string]string{}
	for _, g := range doc.Spec.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				texts[r.Alert] = r.Annotations["summary"] + "\n" + r.Annotations["description"]
			}
		}
	}
	require.NotEmpty(t, texts)
	return texts
}

// sentence matches where a sentence of an alert's text, or a command line
// in it, can start.
var sentence = regexp.MustCompile(`(\. |: |; |, then )`)

// proseCommands puts each sentence of a text on a line of its own, so that
// a command line that starts a sentence starts a line.
func proseCommands(text string) string {
	return sentence.ReplaceAllString(text, "\n")
}
