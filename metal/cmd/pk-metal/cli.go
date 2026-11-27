package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
)

// command is one verb: what it does in a plain line, how to call it, and
// whether it can change a machine or the cluster.
type command struct {
	name     string
	synopsis string // arguments after "pk-metal <name>"
	summary  string // one plain line: what it does
	changes  bool   // true: it changes a machine or what the operator does
	run      func(args []string)
}

// commands is the verb table: main dispatches on it and help prints it, so
// the two cannot disagree. Its order is the order of the groups of the docs
// index (inspect, control, day 1, day 2, lab, shell).
var commands []command

func init() {
	commands = []command{
		{name: "status", synopsis: "[-o table|json]", run: statusCmd,
			summary: "show machines, BMCs, claims and operations, and why one is stuck"},
		{name: "top", synopsis: "[--fleet url] [--workload-kubeconfig file]", run: topCmd, changes: true,
			summary: "live view of every machine; approve, hold and retry from it"},
		{name: "check", synopsis: "[flags] <bmc-url>", run: checkCmd,
			summary: "test that a BMC answers; list its virtual media and systems"},
		{name: "inventory", synopsis: "[flags] <bmc-url>", run: inventoryCmd,
			summary: "read the hardware inventory of a BMC and print it as YAML"},
		{name: "version", run: versionCmd,
			summary: "print the version of this program"},
		{name: "approve", synopsis: "[-n namespace] [--yes] [<operation> | <namespace>/<operation> ...]", run: approveCmd, changes: true,
			summary: "list waiting operations, or show the plan of one and approve it"},
		{name: "retry", synopsis: "[-n namespace] <operation> | <namespace>/<operation>", run: retryCmd, changes: true,
			summary: "delete a failed or aborted operation, so that it is proposed again"},
		{name: "abort", synopsis: "[-n namespace] --reason <text> <operation> | <namespace>/<operation>", run: abortCmd, changes: true,
			summary: "stop an operation and undo what it left behind (media, cordon)"},
		{name: "hold", synopsis: "[-n namespace] [--reason text] <claim> | <namespace>/<claim>", run: holdCmd, changes: true,
			summary: "stop new operations for a claim; nothing on the machine changes"},
		{name: "unhold", synopsis: "[-n namespace] <claim> | <namespace>/<claim>", run: unholdCmd, changes: true,
			summary: "remove a hold, so that the operator proposes operations again"},
		{name: "rollout", synopsis: "list | show <rollout> | approve [--yes] <rollout> | resume <rollout> | pause <rollout> | abort [--yes] <rollout>", run: rolloutCmd, changes: true,
			summary: "list, show, approve, resume, pause or abort cluster rollouts"},
		{name: "render", synopsis: "--dir <manifests> --secrets <secrets.yaml|-> [flags]", run: renderCmd,
			summary: "write Talos machine configs, talosconfig and kubeconfig from files"},
		{name: "boot", synopsis: "--config <rendered.yaml> --node <name> (--listen addr | --relay addr) [flags] <bmc-url>", run: bootCmd, changes: true,
			summary: "mount a config ISO through a BMC and REBOOT the machine into it"},
		{name: "bootstrap", synopsis: "--dir <manifests> --secrets <secrets.yaml|-> [flags]", run: bootstrapCmd, changes: true,
			summary: "start a new Talos cluster on booted machines, without the operator"},
		{name: "relay", synopsis: "[--front addr] [--tunnel addr] [--advertise host:port] [--secret-file file] [--allow rules]", run: relayCmd,
			summary: "forward boot media between the BMC network and pk-metal boot"},
		{name: "pivot", synopsis: "--to <kubeconfig> [--to-context name] [--yes] [flags]", run: pivotCmd, changes: true,
			summary: "move the operator and its objects to another cluster"},
		{name: "reboot", synopsis: "[--mode default|powercycle] [--drain-timeout d] [--grace-period s] [--delete-emptydir-data] <node>", run: rebootCmd, changes: true,
			summary: "propose a clean reboot of a Talos node: drain, reboot, uncordon"},
		{name: "upgrade", synopsis: "[--wait 2m] talos|kubernetes <cluster> <version>", run: upgradeCmd, changes: true,
			summary: "set the Talos or Kubernetes version of a cluster; show the rollout"},
		{name: "scale", synopsis: "[-n namespace] --control-planes N --workers M [--apply] [--ack-etcd-risk reason] <cluster>", run: scaleCmd, changes: true,
			summary: "show the machines a cluster gains or gives back; --apply does it"},
		{name: "reinstall", synopsis: "[-n namespace] [--ack-etcd-risk reason] <claim> | <node>", run: reinstallCmd, changes: true,
			summary: "ask for a new install of a node on the same machine"},
		{name: "replace", synopsis: "[-n namespace] --host <host> [--ack-etcd-risk reason] <claim> | <node>", run: replaceCmd, changes: true,
			summary: "move a node to another machine, for failed hardware"},
		{name: "wipe", synopsis: "[--talos-version v] <host>", run: wipeCmd, changes: true,
			summary: "ask for a wipe of a released machine (the operator proposes it)"},
		{name: "backup", synopsis: "now <cluster> | list [<cluster>] | get <snapshot> -o <file> | decrypt ...", run: backupCmd, changes: true,
			summary: "take, list, download or decrypt etcd snapshots"},
		{name: "restore", synopsis: "plan <cluster> [--snapshot name] [--propose] | upload <operation> --identity <file>", run: restoreCmd, changes: true,
			summary: "plan the restore of a cluster's etcd; upload a snapshot for it"},
		{name: "sim", synopsis: "serve [flags] | fleet --config <file>", run: simCmd,
			summary: "run simulated BMCs: serve one, or a fleet with a VM behind each"},
		{name: "completion", synopsis: "bash|zsh|fish", run: completionCmd,
			summary: "print the shell completion script for bash, zsh or fish"},
	}
}

func versionCmd(args []string) {
	fs := newFlags("version")
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		badUsage(fs, "version takes no arguments")
	}
	fmt.Println(version)
}

func lookup(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == completeVerb {
		completeMain(os.Args[2:])
		return
	}
	args, err := parseGlobal(os.Args[1:])
	if err != nil {
		die(err)
	}
	if len(args) == 0 {
		usage(os.Stderr)
		os.Exit(1)
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		if len(args) > 1 {
			c := lookup(args[1])
			if c == nil {
				dief("unknown command %q; run: pk-metal help", args[1])
			}
			// help <verb> <subcommand> prints what <verb> <subcommand> -h prints.
			c.run(append(slices.Clone(args[2:]), "-h"))
			return
		}
		usage(os.Stdout)
		return
	}
	c := lookup(args[0])
	if c == nil {
		dief("unknown command %q; run: pk-metal help", args[0])
	}
	c.run(args[1:])
}

// global holds the flags every cluster verb shares. They may come before the
// verb (pk-metal --context lab status) or after it (pk-metal status --context lab).
var global struct {
	kubeconfig string
	context    string
}

// The texts of the global flags, for their flags and for the top-level help.
const (
	kubeconfigUsage = "kubeconfig file (default: $KUBECONFIG, then ~/.kube/config, then the in-cluster account)"
	contextUsage    = "kubeconfig context to use (default: the current context)"
)

func addGlobalFlags(fs *flag.FlagSet) {
	fs.StringVar(&global.kubeconfig, "kubeconfig", global.kubeconfig, kubeconfigUsage)
	fs.StringVar(&global.context, "context", global.context, contextUsage)
}

// parseGlobal reads the global flags in front of the verb and returns the rest.
func parseGlobal(args []string) ([]string, error) {
	fs := flag.NewFlagSet("pk-metal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return []string{"help"}, nil
		}
		return nil, fmt.Errorf("%v; run: pk-metal help", err)
	}
	return fs.Args(), nil
}

// newFlags returns the flag set for a verb. Its -h output is the verb's
// summary, its synopsis and its flags.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		w := fs.Output()
		verb := strings.Fields(name)[0]
		if c := lookup(verb); c != nil {
			fmt.Fprintf(w, "pk-metal %s: %s\n\n", c.name, c.summary)
			fmt.Fprintf(w, "usage: pk-metal %s %s\n", c.name, c.synopsis)
			if extra := verbHelp[verb]; extra != "" {
				fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(extra))
			}
		}
		n := 0
		fs.VisitAll(func(*flag.Flag) { n++ })
		if n > 0 {
			fmt.Fprintln(w, "\nflags:")
			fs.PrintDefaults()
		}
	}
	return fs
}

// verbSubcommands are the subcommands of a verb, read from its synopsis
// ("list | show <rollout> | ..."): the first word of each alternative, when
// every alternative starts with a plain word.
func verbSubcommands(c *command) []string {
	alts := strings.Split(c.synopsis, " | ")
	if len(alts) < 2 {
		return nil
	}
	var subs []string
	for _, alt := range alts {
		word, _, _ := strings.Cut(strings.TrimSpace(alt), " ")
		if word == "" || strings.ContainsAny(word, "-[<|") {
			return nil
		}
		subs = append(subs, word)
	}
	return subs
}

// subcommand splits the subcommand off the arguments of a verb that has
// them. -h before it prints the help of the verb; no subcommand, or one that
// verbSubcommands does not list, is a bad call.
func subcommand(verb string, args []string) (sub string, rest []string) {
	fs := newFlags(verb)
	subs := verbSubcommands(lookup(verb))
	switch {
	case len(args) == 0 || strings.HasPrefix(args[0], "-"):
		parseFlags(fs, args)
		badUsage(fs, "name a subcommand: "+strings.Join(subs, ", "))
	case !slices.Contains(subs, args[0]):
		badUsage(fs, fmt.Sprintf("unknown subcommand %q: give %s", args[0], strings.Join(subs, ", ")))
	}
	return args[0], args[1:]
}

// verbHelp is longer help text for the verbs that need it, printed by -h.
var verbHelp = map[string]string{}

// parseFlags parses a verb's flags wherever they stand, before or after its
// arguments (pk-metal scale lab --control-planes 3 as well as pk-metal scale
// --control-planes 3 lab); everything after "--" is an argument. fs.Args()
// then holds the arguments. The standard parser stops at the first argument,
// so the flags after it were taken for arguments and the verb refused the
// call. -h prints the verb's help to stdout and exits 0; a bad flag prints
// the error and the usage to stderr and exits 1.
func parseFlags(fs *flag.FlagSet, args []string) {
	if flagCapture != nil {
		flagCapture(fs) // shell completion reads the flags; it does not return
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "-h" || a == "-help" || a == "--help" {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			os.Exit(0)
		}
	}
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			badUsage(fs, err.Error())
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
	// Leave exactly the arguments in fs.Args(), where every verb reads them.
	// The flags parsed above keep their values.
	_ = fs.Parse(append([]string{"--"}, pos...))
}

// flagGiven reports whether the flag was set on the command line.
func flagGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

// badUsage reports a wrong call of a verb, with its usage line, and exits 1.
func badUsage(fs *flag.FlagSet, msg string) {
	verb := strings.Fields(fs.Name())[0]
	fmt.Fprintf(os.Stderr, "pk-metal: %s\n", msg)
	if c := lookup(verb); c != nil {
		fmt.Fprintf(os.Stderr, "usage: pk-metal %s %s\n", c.name, c.synopsis)
	}
	fmt.Fprintf(os.Stderr, "run: pk-metal help %s\n", verb)
	os.Exit(1)
}

// signalContext is the context of one verb: Ctrl-C or SIGTERM cancels it, so
// that a call that hangs on a slow API server ends, and a question that waits
// for an answer ends with "nothing changed".
func signalContext() (ctx context.Context, stop context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// die prints the error as "pk-metal: <message>" on stderr and exits 1.
func die(err error) {
	fmt.Fprintf(os.Stderr, "pk-metal: %v\n", err)
	os.Exit(1)
}

func dief(format string, args ...any) {
	die(fmt.Errorf(format, args...))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `pk-metal: manage bare-metal machines through their BMCs (Redfish) and the
pk-metal operator that runs in a Kubernetes cluster.

usage: pk-metal [--kubeconfig file] [--context name] <command> [flags] [arguments]

`)
	for _, changes := range []bool{false, true} {
		if changes {
			fmt.Fprintln(w, "\ncommands that change a machine, or what the operator does:")
		} else {
			fmt.Fprintln(w, "commands that change no machine:")
		}
		for _, c := range commands {
			if c.changes == changes {
				fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
			}
		}
	}
	fmt.Fprintf(w, `
help:
  pk-metal help <command>     flags and details of one command (or: pk-metal <command> -h)

global flags (for the commands that talk to the cluster):
  --kubeconfig file           %s
  --context name              %s

exit status:
  0  success
  1  error; the message is on stderr, as "pk-metal: <message>"
  2  pk-metal status only: something is blocked or failed and needs a person
`, kubeconfigUsage, contextUsage)
}
