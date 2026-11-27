package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// Shell completion. Cobra completes the commands and the flag names, and
// prints the scripts (pk-metal completion bash|zsh|fish|powershell); the
// scripts call its hidden command __complete on each TAB. This file says
// what the arguments and the flag values of each command are (argKinds,
// flagKinds) and where their candidates come from: the names of objects in
// the cluster that --kubeconfig and --context on the same command line
// select, kubeconfig contexts, files and fixed words. Each lookup in the
// cluster has a short timeout, so that TAB never hangs on an API server that
// does not answer.

// completeTimeout bounds every cluster lookup of one TAB.
const completeTimeout = 2 * time.Second

type candidate struct {
	Value string
	Desc  string
}

// --- what each argument and flag value is ------------------------------------

// kind names what an argument or a flag value is, and so where its
// candidates come from.
type kind int

const (
	kindNone            kind = iota // free text: no candidates
	kindFile                        // a file name
	kindDir                         // a directory name
	kindNamespace                   // a Namespace
	kindContext                     // a context of the --kubeconfig file
	kindToContext                   // a context of the --to kubeconfig file
	kindOpPending                   // a HostOperation that waits for approval
	kindOpFailed                    // a HostOperation that failed or was aborted
	kindOpActive                    // a HostOperation that has not finished
	kindOpAny                       // any HostOperation
	kindClaim                       // a HostClaim
	kindClaimOrNode                 // a HostClaim or a TalosNode
	kindNode                        // a TalosNode
	kindHost                        // a Host
	kindHostAvailable               // a Host that is Available
	kindCluster                     // a TalosCluster
	kindClusterOrPolicy             // a TalosCluster or an EtcdBackupPolicy
	kindSnapshot                    // an EtcdSnapshot
	kindRollout                     // a TalosRollout
	kindBMC                         // the address of a ManagementEndpoint
	kindFixture                     // an embedded simulator fixture, or a file
	kindStatic                      // the words in kindSpec.words
)

type kindSpec struct {
	kind  kind
	words []string // kindStatic only
}

func static(words ...string) kindSpec { return kindSpec{kind: kindStatic, words: words} }

// argSpec is what the positional arguments of a command are. The last
// entry repeats for a command that takes several arguments (variadic).
type argSpec struct {
	kinds    []kindSpec
	variadic bool
}

// argKinds is what each positional argument of a command is ("verb" or "verb
// sub"). A command that is not listed takes no argument that completes.
var argKinds = map[string]argSpec{
	"approve":         {kinds: []kindSpec{{kind: kindOpPending}}, variadic: true},
	"retry":           {kinds: []kindSpec{{kind: kindOpFailed}}},
	"abort":           {kinds: []kindSpec{{kind: kindOpActive}}},
	"reboot":          {kinds: []kindSpec{{kind: kindNode}}},
	"hold":            {kinds: []kindSpec{{kind: kindClaim}}},
	"unhold":          {kinds: []kindSpec{{kind: kindClaim}}},
	"wipe":            {kinds: []kindSpec{{kind: kindHost}}},
	"reinstall":       {kinds: []kindSpec{{kind: kindClaimOrNode}}},
	"replace":         {kinds: []kindSpec{{kind: kindClaimOrNode}}},
	"scale":           {kinds: []kindSpec{{kind: kindCluster}}},
	"upgrade":         {kinds: []kindSpec{static("talos", "kubernetes"), {kind: kindCluster}}},
	"backup now":      {kinds: []kindSpec{{kind: kindClusterOrPolicy}}},
	"backup list":     {kinds: []kindSpec{{kind: kindCluster}}},
	"backup get":      {kinds: []kindSpec{{kind: kindSnapshot}}},
	"backup decrypt":  {kinds: []kindSpec{{kind: kindSnapshot}}},
	"restore plan":    {kinds: []kindSpec{{kind: kindCluster}}},
	"restore upload":  {kinds: []kindSpec{{kind: kindOpAny}}},
	"rollout show":    {kinds: []kindSpec{{kind: kindRollout}}},
	"rollout approve": {kinds: []kindSpec{{kind: kindRollout}}},
	"rollout resume":  {kinds: []kindSpec{{kind: kindRollout}}},
	"rollout pause":   {kinds: []kindSpec{{kind: kindRollout}}},
	"rollout abort":   {kinds: []kindSpec{{kind: kindRollout}}},
	"check":           {kinds: []kindSpec{{kind: kindBMC}}},
	"inventory":       {kinds: []kindSpec{{kind: kindBMC}}},
	"boot":            {kinds: []kindSpec{{kind: kindBMC}}},
}

// flagKinds is what the value of a flag is: by "verb:flag" ("verb sub:flag"
// for a subcommand) first, then by "flag" alone. Every flag that takes a
// value is listed (a test checks it), kindNone included, so that a new flag
// is a decision and not an omission.
var flagKinds = map[string]kindSpec{
	"kubeconfig":          {kind: kindFile},
	"context":             {kind: kindContext},
	"namespace":           {kind: kindNamespace},
	"output":              static("table", "json"),
	"to-file":             {kind: kindFile},
	"plaintext-file":      {kind: kindFile},
	"mode":                static("default", "powercycle"),
	"media-store":         static("copy", "ephemeral"),
	"host":                {kind: kindHostAvailable},
	"snapshot":            {kind: kindSnapshot},
	"restore plan:node":   {kind: kindNode},
	"boot:node":           {kind: kindNone}, // a node of the rendered config, not of the cluster
	"to":                  {kind: kindFile},
	"to-context":          {kind: kindToContext},
	"workload-kubeconfig": {kind: kindFile},
	"talosconfig":         {kind: kindFile},
	"identity":            {kind: kindFile},
	"in":                  {kind: kindFile},
	"from-file":           {kind: kindFile},
	"secret-file":         {kind: kindFile},
	"secrets":             {kind: kindFile},
	"bmc-ca":              {kind: kindFile},
	"config":              {kind: kindFile},
	"dir":                 {kind: kindDir},
	"out":                 {kind: kindDir},
	"fixture":             {kind: kindFixture},
	"relay-secret-file":   {kind: kindFile},
	// Names that are not objects of the cluster.
	"inventory:node": {}, // the name for the fact file
	"bootstrap:node": {}, // a TalosNode of --dir
	"endpoint":       {}, // the address of the bootstrap --node
	// Free text: durations, numbers, versions, addresses, names and reasons.
	"ack-etcd-risk": {}, "reason": {}, "talos-version": {}, "control-planes": {}, "workers": {},
	"drain-timeout": {}, "grace-period": {}, "wait": {}, "timeout": {}, "settle": {}, "interval": {},
	"ttl": {}, "credentials-ttl": {}, "hang-timeout": {},
	"image": {}, "image-factory": {}, "media-url": {}, "media": {}, "fleet": {}, "user": {}, "system": {},
	"address": {}, "listen": {}, "state-listen": {}, "relay": {}, "front": {}, "tunnel": {}, "advertise": {},
	"allow": {},
}

func flagKind(path, name string) (kindSpec, bool) {
	if k, ok := flagKinds[path+":"+name]; ok {
		return k, true
	}
	k, ok := flagKinds[name]
	return k, ok
}

// --- the line ----------------------------------------------------------------

// line is a command line read up to the word under the cursor.
type line struct {
	cmd     *cobra.Command // the command; its flags hold the values given on the line
	pos     []string       // the arguments before the word under the cursor
	current string         // the word under the cursor
}

// completion makes TAB complete the arguments and the flag values of every
// command under root, as argKinds and flagKinds say. connect is called only
// when a candidate comes from the cluster.
func completion(root *cobra.Command, connect func() (client.Client, error)) {
	none := cobra.ShellCompDirectiveNoFileComp
	root.CompletionOptions.DefaultShellCompDirective = &none
	if c, _, err := root.Find([]string{"completion"}); err == nil {
		c.Short = "print the shell completion script: TAB then completes commands, flags, and the names of objects in the cluster"
		c.Long = completionHelp
		for _, sh := range c.Commands() {
			if h, ok := shellHelp[sh.Name()]; ok {
				sh.Short, sh.Long = h[0], h[1]
			}
		}
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		path := strings.TrimPrefix(strings.TrimPrefix(c.CommandPath(), root.Name()), " ")
		if spec, ok := argKinds[path]; ok {
			c.ValidArgsFunction = func(cmd *cobra.Command, args []string, current string) ([]cobra.Completion, cobra.ShellCompDirective) {
				return completions(argCandidates(spec, line{cmd, args, current}, connect))
			}
		}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if k, ok := flagKind(path, f.Name); ok {
				_ = c.RegisterFlagCompletionFunc(f.Name, func(cmd *cobra.Command, args []string, current string) ([]cobra.Completion, cobra.ShellCompDirective) {
					return completions(kindCandidates(k, line{cmd, args, current}, connect))
				})
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// completions writes candidates as cobra wants them: "value" or
// "value<TAB>description".
func completions(cands []candidate, dir cobra.ShellCompDirective) ([]cobra.Completion, cobra.ShellCompDirective) {
	out := make([]cobra.Completion, 0, len(cands))
	for _, c := range cands {
		out = append(out, cobra.CompletionWithDesc(c.Value, c.Desc))
	}
	return out, dir
}

// --- candidates ----------------------------------------------------------------

func argCandidates(spec argSpec, l line, connect func() (client.Client, error)) ([]candidate, cobra.ShellCompDirective) {
	idx := len(l.pos)
	if idx >= len(spec.kinds) {
		if !spec.variadic {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		idx = len(spec.kinds) - 1
	}
	// approve takes several operations, each once.
	cands, dir := kindCandidates(spec.kinds[idx], l, connect)
	if spec.variadic {
		cands = slices.DeleteFunc(cands, func(c candidate) bool { return slices.Contains(l.pos, c.Value) })
	}
	return cands, dir
}

func kindCandidates(k kindSpec, l line, connect func() (client.Client, error)) ([]candidate, cobra.ShellCompDirective) {
	none := cobra.ShellCompDirectiveNoFileComp
	switch k.kind {
	case kindNone:
		return nil, none
	case kindFile:
		return nil, cobra.ShellCompDirectiveDefault
	case kindDir:
		return nil, cobra.ShellCompDirectiveFilterDirs
	case kindStatic:
		return filter(words2cands(k.words), l.current), none
	case kindFixture:
		if strings.ContainsAny(l.current, "./") {
			return nil, cobra.ShellCompDirectiveDefault
		}
		return filter(words2cands(sim.Fixtures()), l.current), none
	case kindContext:
		return filter(contexts(global.kubeconfig), l.current), none
	case kindToContext:
		to, _ := l.cmd.Flags().GetString("to")
		if to == "" {
			return nil, none
		}
		return filter(contexts(to), l.current), none
	}
	cl, err := connect()
	if err != nil {
		return nil, none
	}
	ctx, cancel := context.WithTimeout(context.Background(), completeTimeout)
	defer cancel()
	cands, _ := clusterCandidates(ctx, cl, k.kind, l)
	return filter(cands, l.current), none
}

// clusterCandidates lists the objects of a kind. A failed list gives no
// candidates: completion never prints an error.
func clusterCandidates(ctx context.Context, cl client.Client, k kind, l line) ([]candidate, error) {
	switch k {
	case kindNamespace:
		var list corev1.NamespaceList
		if err := cl.List(ctx, &list); err != nil {
			return nil, err
		}
		var c []candidate
		for _, o := range list.Items {
			c = append(c, candidate{Value: o.Name})
		}
		return c, nil
	case kindOpPending, kindOpFailed, kindOpActive, kindOpAny:
		return operationCandidates(ctx, cl, k, l)
	case kindClaim:
		return claimCandidates(ctx, cl, l)
	case kindClaimOrNode:
		claims, err := claimCandidates(ctx, cl, l)
		if err != nil {
			return nil, err
		}
		if strings.Contains(l.current, "/") {
			return claims, nil
		}
		nodes, err := nodeCandidates(ctx, cl)
		if err != nil {
			return nil, err
		}
		return append(claims, nodes...), nil
	case kindNode:
		return nodeCandidates(ctx, cl)
	case kindHost, kindHostAvailable:
		var list metalv1alpha1.HostList
		if err := cl.List(ctx, &list); err != nil {
			return nil, err
		}
		var c []candidate
		for _, o := range list.Items {
			if k == kindHostAvailable && o.Status.Phase != metalv1alpha1.HostAvailable {
				continue
			}
			c = append(c, candidate{Value: o.Name, Desc: string(o.Status.Phase)})
		}
		return c, nil
	case kindCluster, kindClusterOrPolicy:
		var list osv1alpha1.TalosClusterList
		if err := cl.List(ctx, &list); err != nil {
			return nil, err
		}
		var c []candidate
		for _, o := range list.Items {
			c = append(c, candidate{Value: o.Name, Desc: "Talos " + o.Spec.TalosVersion})
		}
		if k == kindClusterOrPolicy {
			var policies osv1alpha1.EtcdBackupPolicyList
			if err := cl.List(ctx, &policies); err == nil {
				for _, o := range policies.Items {
					c = append(c, candidate{Value: o.Name, Desc: "backup policy of cluster " + o.Spec.ClusterRef.Name})
				}
			}
		}
		return dedupe(c), nil
	case kindSnapshot:
		var list osv1alpha1.EtcdSnapshotList
		if err := cl.List(ctx, &list); err != nil {
			return nil, err
		}
		var c []candidate
		for _, o := range list.Items {
			desc := "cluster " + o.Status.ClusterName
			if o.Status.TakenAt != nil {
				desc += ", taken " + o.Status.TakenAt.UTC().Format(time.RFC3339)
			}
			c = append(c, candidate{Value: o.Name, Desc: desc})
		}
		return c, nil
	case kindRollout:
		var list osv1alpha1.TalosRolloutList
		if err := cl.List(ctx, &list); err != nil {
			return nil, err
		}
		var c []candidate
		for _, o := range list.Items {
			c = append(c, candidate{Value: o.Name, Desc: "cluster " + o.Spec.ClusterRef.Name})
		}
		return c, nil
	case kindBMC:
		var list metalv1alpha1.ManagementEndpointList
		if err := cl.List(ctx, &list); err != nil {
			return nil, err
		}
		var c []candidate
		for _, o := range list.Items {
			if o.Spec.Address != "" {
				c = append(c, candidate{Value: o.Spec.Address, Desc: o.Name})
			}
		}
		return c, nil
	}
	return nil, nil
}

// namespaced lists a namespaced kind for the word under the cursor: with a
// "/" in it, the objects of every namespace as namespace/name; else the
// objects of the -n namespace (or of def) by name.
func namespaced(ctx context.Context, cl client.Client, list client.ObjectList, l line, def string) (all bool, err error) {
	all = strings.Contains(l.current, "/")
	ns := def
	if f := l.cmd.Flags().Lookup("namespace"); f != nil && f.Changed {
		ns = f.Value.String()
	}
	if all {
		return true, cl.List(ctx, list)
	}
	return false, cl.List(ctx, list, client.InNamespace(ns))
}

func refValue(all bool, namespace, name string) string {
	if all {
		return namespace + "/" + name
	}
	return name
}

func operationCandidates(ctx context.Context, cl client.Client, k kind, l line) ([]candidate, error) {
	var list metalv1alpha1.HostOperationList
	all, err := namespaced(ctx, cl, &list, l, operatorNamespace)
	if err != nil {
		return nil, err
	}
	var c []candidate
	for i := range list.Items {
		op := &list.Items[i]
		p := opPhase(op)
		switch k {
		case kindOpPending:
			if !op.AwaitsApproval() {
				continue
			}
		case kindOpFailed:
			if p != metalv1alpha1.OperationFailed && p != metalv1alpha1.OperationAborted {
				continue
			}
		case kindOpActive:
			if op.Finished() {
				continue
			}
		}
		c = append(c, candidate{
			Value: refValue(all, op.Namespace, op.Name),
			Desc:  strings.TrimSpace(fmt.Sprintf("%s on host %s", orDash(string(p)), op.Spec.HostRef.Name)),
		})
	}
	return c, nil
}

func claimCandidates(ctx context.Context, cl client.Client, l line) ([]candidate, error) {
	var list metalv1alpha1.HostClaimList
	all, err := namespaced(ctx, cl, &list, l, claimNamespace)
	if err != nil {
		return nil, err
	}
	var c []candidate
	for _, o := range list.Items {
		desc := "claim, " + orDash(string(o.Status.Phase))
		if o.Status.HostRef != nil {
			desc += " on host " + o.Status.HostRef.Name
		}
		c = append(c, candidate{Value: refValue(all, o.Namespace, o.Name), Desc: desc})
	}
	return c, nil
}

func nodeCandidates(ctx context.Context, cl client.Client) ([]candidate, error) {
	var list osv1alpha1.TalosNodeList
	if err := cl.List(ctx, &list); err != nil {
		return nil, err
	}
	var c []candidate
	for _, o := range list.Items {
		c = append(c, candidate{Value: o.Name, Desc: "node of cluster " + o.Spec.ClusterRef.Name})
	}
	return c, nil
}

// contexts are the context names of a kubeconfig, by the kubectl rules when
// file is empty. It reads files only.
func contexts(file string) []candidate {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = file
	cfg, err := rules.Load()
	if err != nil {
		return nil
	}
	var c []candidate
	for _, name := range slices.Sorted(maps.Keys(cfg.Contexts)) {
		desc := ""
		if name == cfg.CurrentContext {
			desc = "current context"
		}
		c = append(c, candidate{Value: name, Desc: desc})
	}
	return c
}

func words2cands(words []string) []candidate {
	c := make([]candidate, 0, len(words))
	for _, w := range words {
		c = append(c, candidate{Value: w})
	}
	return c
}

func filter(cands []candidate, prefix string) []candidate {
	return slices.DeleteFunc(cands, func(c candidate) bool { return !strings.HasPrefix(c.Value, prefix) })
}

func dedupe(cands []candidate) []candidate {
	seen := map[string]bool{}
	return slices.DeleteFunc(cands, func(c candidate) bool {
		if seen[c.Value] {
			return true
		}
		seen[c.Value] = true
		return false
	})
}

// completionClient connects like every other verb, with a timeout on each
// request so that an API server that does not answer does not hold TAB.
func completionClient() (client.Client, error) {
	cfg, err := restConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = completeTimeout
	return client.New(cfg, client.Options{Scheme: newScheme()})
}

// completionHelp is the help of the completion command, before cobra's
// list of the shells. Each shell's own help (shellHelp) says how to load its
// script.
const completionHelp = `The script completes the commands, their subcommands and flags, and the
names of the objects that the arguments and flags take: operations, claims,
nodes, hosts, clusters, snapshots, rollouts, BMC addresses, namespaces and
kubeconfig contexts. The names come from the cluster that --kubeconfig and
--context on the same command line select (default: as for every other
command). Each lookup stops after 2 seconds; then there are no names.

Operations are shown by state: approve completes the operations that wait
for approval, retry the failed and aborted ones, abort the unfinished ones.
Type a "/" to complete namespace/name from every namespace.

The script comes from this version of pk-metal. After an upgrade, write
the script again to the file where you keep it, or open a new shell where
your shell loads it at start.`

// shellHelp is the summary and the help of each shell's completion command.
var shellHelp = map[string][2]string{
	"bash": {"print the completion script for bash (needs the bash-completion package)",
		`Load the script in each new shell:
  echo 'source <(pk-metal completion bash)' >> ~/.bashrc

Open a new shell after the change.`},
	"zsh": {"print the completion script for zsh",
		`Load the script in each new shell (after compinit in ~/.zshrc):
  echo 'source <(pk-metal completion zsh)' >> ~/.zshrc

Open a new shell after the change.`},
	"fish": {"print the completion script for fish",
		`Write the script to the completions directory of fish:
  pk-metal completion fish > ~/.config/fish/completions/pk-metal.fish

Write it again after an upgrade of pk-metal. Open a new shell after the change.`},
	"powershell": {"print the completion script for PowerShell",
		`Load the script in each new shell: add this line to your profile ($PROFILE):
  pk-metal completion powershell | Out-String | Invoke-Expression

Open a new shell after the change.`},
}
