package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The groups of the root's help: the commands that change no machine, and
// the commands that change a machine or what the operator does.
const (
	groupReads   = "reads"
	groupChanges = "changes"
)

// clusterAnnotation marks a command that talks to the Kubernetes API, for the
// reference pages. Every command accepts the global flags.
const clusterAnnotation = "pk-metal/needs-cluster"

func needsCluster() map[string]string { return map[string]string{clusterAnnotation: "true"} }

// newRoot builds the command tree. connect opens the client that the shell
// completion reads the names of objects with. Cobra keeps the values of the
// flags from one Execute to the next, so a test builds a new tree for each
// command line.
func newRoot(connect func() (client.Client, error)) *cobra.Command {
	root := &cobra.Command{
		Use:   "pk-metal",
		Short: "manage bare-metal machines through their BMCs (Redfish) and the pk-metal operator that runs in a Kubernetes cluster",
		Long: `exit status:
  0  success
  1  error; the message is on stderr, as "pk-metal: <message>"
  2  pk-metal status only: something is blocked or failed and needs a person`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.PersistentFlags().StringVar(&global.kubeconfig, "kubeconfig", "",
		"kubeconfig file (default: $KUBECONFIG, then ~/.kube/config, then the in-cluster account)")
	root.PersistentFlags().StringVar(&global.context, "context", "", "kubeconfig context to use (default: the current context)")
	root.AddGroup(
		&cobra.Group{ID: groupReads, Title: "Commands that change no machine:"},
		&cobra.Group{ID: groupChanges, Title: "Commands that change a machine, or what the operator does:"},
	)
	// The help of a command starts with its line in the list of commands.
	root.SetHelpTemplate("{{.Short}}\n\n{{with .Long}}{{. | trimTrailingWhitespaces}}\n\n{{end}}" +
		"{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}")
	cobra.EnableCommandSorting = false
	root.AddCommand(statusCmd(), checkCmd(), inventoryCmd(), renderCmd(), relayCmd(), simCmd(), versionCmd(),
		topCmd(), approveCmd(), retryCmd(), abortCmd(), rebootCmd(), holdCmd(), unholdCmd(), backupCmd(),
		restoreCmd(), wipeCmd(), reinstallCmd(), replaceCmd(), scaleCmd(), rolloutCmd(), upgradeCmd(),
		pivotCmd(), bootCmd(), bootstrapCmd(), docsCmd())
	root.SetCompletionCommandGroupID(groupReads)
	cobratree.Strict(root)
	root.SetFlagErrorFunc(cobratree.FlagError)
	completion(root, connect)
	return root
}

func main() {
	os.Exit(run(newRoot(completionClient), os.Args[1:], os.Stderr))
}

// run executes one command line and returns its exit status. An error is
// written to stderr as "pk-metal: <message>", and a wrong command line also
// names the help of the command.
func run(root *cobra.Command, args []string, stderr io.Writer) int {
	root.SetArgs(args)
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "pk-metal: %v\n", err)
	if cobratree.IsUsage(err) {
		fmt.Fprintf(stderr, "run: %s --help\n", cmd.CommandPath())
	}
	return 1
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "print the version of this program", GroupID: groupReads, Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), version) },
	}
}

// global holds the flags every cluster command shares. They may come before
// the command (pk-metal --context lab status) or after it (pk-metal status
// --context lab).
var global struct {
	kubeconfig string
	context    string
}

// signalContext is the context of one verb that talks to the cluster: Ctrl-C
// or SIGTERM cancels it, so that a call that hangs on a slow API server ends,
// and a question that waits for an answer ends with "nothing changed".
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

// docCommand is one command of the tree as `pk-metal __docs` prints it:
// hack/docgen writes the data of the reference pages from it.
type docCommand struct {
	Path        string            `json:"path"`
	Use         string            `json:"use"`
	Short       string            `json:"short"`
	Long        string            `json:"long,omitempty"`
	Example     string            `json:"example,omitempty"`
	Group       string            `json:"group,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Flags       []docFlag         `json:"flags"`
	Commands    []docCommand      `json:"commands,omitempty"`
}

type docFlag struct {
	Name       string `json:"name"`
	Shorthand  string `json:"shorthand,omitempty"`
	Type       string `json:"type"`
	Default    string `json:"default"`
	Usage      string `json:"usage"`
	Persistent bool   `json:"persistent,omitempty"`
}

// docsCmd prints the command tree as JSON.
func docsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "__docs", Short: "print the command tree as JSON, for the reference pages", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(describe(cmd.Root()))
		},
	}
}

// describe returns c and the commands under it that help lists. The flags
// are those c defines itself, without the help flag.
func describe(c *cobra.Command) docCommand {
	d := docCommand{
		Path: c.CommandPath(), Use: c.Use, Short: c.Short, Long: c.Long, Example: c.Example,
		Group: c.GroupID, Annotations: c.Annotations, Flags: []docFlag{},
	}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		d.Flags = append(d.Flags, docFlag{
			Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: f.DefValue, Usage: f.Usage,
			Persistent: c.PersistentFlags().Lookup(f.Name) != nil,
		})
	})
	for _, sub := range c.Commands() {
		if sub.IsAvailableCommand() {
			d.Commands = append(d.Commands, describe(sub))
		}
	}
	return d
}
