package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// treeFunc returns what `pk-metal __docs` prints: the command tree as JSON.
// The tests feed it a recorded tree; main runs the binary.
type treeFunc func() ([]byte, error)

// execTree runs the binary with an empty environment apart from PATH, so that
// no default in the output can come from the machine docgen runs on.
func execTree(bin string) treeFunc {
	return func() ([]byte, error) {
		cmd := exec.Command(bin, "__docs")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent"}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("pk-metal __docs: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), nil
	}
}

// treeCommand is one command as `pk-metal __docs` prints it (docCommand in
// cmd/pk-metal/cli.go).
type treeCommand struct {
	Use         string            `json:"use"`
	Short       string            `json:"short"`
	Long        string            `json:"long"`
	Example     string            `json:"example"`
	Group       string            `json:"group"`
	Annotations map[string]string `json:"annotations"`
	Flags       []cliFlag         `json:"flags"`
	Commands    []treeCommand     `json:"commands"`
}

// cliIndex is the list of the commands and the exit status of the program.
type cliIndex struct {
	Commands   []cliSummary `json:"commands"`
	ExitStatus []exitCode   `json:"exitStatus"`
}

type cliSummary struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Changes is true for a command that changes a machine or what the
	// operator does (help lists those apart).
	Changes bool `json:"changes"`
}

// cliCommand is one command of the program and its subcommands.
type cliCommand struct {
	Name     string `json:"name"`
	Summary  string `json:"summary"`
	Synopsis string `json:"synopsis"`
	Changes  bool   `json:"changes"`
	// NeedsCluster is true when the command reads or writes the cluster.
	NeedsCluster bool `json:"needsCluster"`
	// Description and Examples are the rest of the help text. No page shows
	// them; mise run docs:lint:generated checks their words.
	Description string          `json:"description"`
	Examples    []string        `json:"examples"`
	ExitStatus  []exitCode      `json:"exitStatus"`
	Flags       []cliFlag       `json:"flags"`
	Subcommands []cliSubcommand `json:"subcommands"`
}

type cliSubcommand struct {
	Name        string    `json:"name"`
	Summary     string    `json:"summary"`
	Synopsis    string    `json:"synopsis"`
	Description string    `json:"description"`
	Flags       []cliFlag `json:"flags"`
}

type cliFlag struct {
	// Name is without dashes; the site writes it as --name.
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Usage     string `json:"usage"`
	// Persistent is true for a flag that the subcommands inherit.
	Persistent bool `json:"persistent,omitempty"`
}

type exitCode struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

// The tree's names for the group of the commands that change a machine, and
// for a command that talks to the cluster (cmd/pk-metal/cli.go).
const (
	groupChanges      = "changes"
	clusterAnnotation = "pk-metal/needs-cluster"
)

func readCLI(tree treeFunc) (cliIndex, []cliCommand, error) {
	raw, err := tree()
	if err != nil {
		return cliIndex{}, nil, err
	}
	var root treeCommand
	if err := json.Unmarshal(raw, &root); err != nil {
		return cliIndex{}, nil, fmt.Errorf("read the command tree: %w", err)
	}
	index := cliIndex{Commands: []cliSummary{}, ExitStatus: exitStatus(root.Long)}
	var cmds []cliCommand
	for _, t := range root.Commands {
		name, synopsis := splitUse(t.Use)
		c := cliCommand{
			Name: name, Summary: t.Short, Synopsis: synopsis, Changes: t.Group == groupChanges,
			NeedsCluster: t.Annotations[clusterAnnotation] == "true", Description: description(t.Long),
			Examples: examples(t.Example), ExitStatus: exitStatus(t.Long), Flags: t.Flags, Subcommands: []cliSubcommand{},
		}
		var forms []string
		for _, sub := range t.Commands {
			flags := slices.Clone(sub.Flags)
			for _, f := range t.Flags {
				if f.Persistent {
					f.Persistent = false
					flags = append(flags, f)
				}
			}
			slices.SortFunc(flags, func(a, b cliFlag) int { return strings.Compare(a.Name, b.Name) })
			subName, _ := splitUse(sub.Use)
			c.Subcommands = append(c.Subcommands, cliSubcommand{
				Name: subName, Summary: sub.Short, Synopsis: sub.Use, Description: description(sub.Long), Flags: flags,
			})
			forms = append(forms, sub.Use)
		}
		if len(forms) > 0 {
			c.Synopsis = strings.Join(forms, " | ")
		}
		index.Commands = append(index.Commands, cliSummary{Name: name, Summary: t.Short, Changes: c.Changes})
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return index, nil, errors.New("the tree has no commands")
	}
	return index, cmds, nil
}

// splitUse splits a cobra Use line into the command's name and its synopsis.
func splitUse(use string) (name, synopsis string) {
	name, synopsis, _ = strings.Cut(use, " ")
	return name, synopsis
}

// description is a help text without its block "exit status:", which
// exitStatus reads.
func description(long string) string {
	before, block, found := strings.Cut(long, "exit status:\n")
	if !found {
		return strings.TrimSpace(long)
	}
	_, after, _ := strings.Cut(block, "\n\n")
	return strings.TrimSpace(strings.TrimSpace(before) + "\n\n" + strings.TrimSpace(after))
}

// examples returns the lines of a command's examples, without their indent.
func examples(text string) []string {
	lines := []string{}
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

var exitLine = regexp.MustCompile(`^\s+(\d+)\s+(.+)$`)

// exitStatus reads the block "exit status:" of a help text: lines "  2  text",
// and indented lines that continue the text of the last one.
func exitStatus(help string) []exitCode {
	codes := []exitCode{}
	_, block, found := strings.Cut(help, "exit status:\n")
	if !found {
		return codes
	}
	for line := range strings.SplitSeq(block, "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		if m := exitLine.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			codes = append(codes, exitCode{Code: n, Meaning: strings.TrimSpace(m[2])})
		} else if len(codes) > 0 {
			codes[len(codes)-1].Meaning += " " + strings.TrimSpace(line)
		}
	}
	return codes
}
