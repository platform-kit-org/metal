package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// Shell completion. The scripts that `pk-metal completion <shell>` prints
// call the hidden verb
//
//	pk-metal __complete <word>... <current word>
//
// with the words of the command line after the program name; the last word
// is the one under the cursor (empty after a space). It prints one
// candidate per line, as "value" or "value<TAB>description", and then one
// line ":<directive>" that tells the script what to do when it has no
// candidates of its own: complete nothing, file names or directories.
//
// The flags come from the verbs themselves (captureFlags), so a new flag
// completes without a change here; the names of objects come from the
// cluster that --kubeconfig/--context on the same line select, with a short
// timeout, so that TAB never hangs on an API server that does not answer.

// completeVerb is the hidden verb the completion scripts call.
const completeVerb = "__complete"

// completeTimeout bounds every cluster lookup of one TAB.
const completeTimeout = 2 * time.Second

type directive int

const (
	compNone  directive = 0 // the candidates printed, nothing else
	compFiles directive = 1 // file names
	compDirs  directive = 2 // directory names
)

type candidate struct {
	Value string
	Desc  string
}

// --- capture of a verb's flags ---------------------------------------------

// flagCapture, when set, is called by parseFlags with the verb's complete
// flag set, before it parses anything. captureFlags sets it to stop the verb
// there.
var flagCapture func(fs *flag.FlagSet)

type capturedFlags struct{ fs *flag.FlagSet }

// captureFlags runs a verb only up to its parseFlags call and returns the
// flag set it built. Every verb builds its flags and then calls parseFlags
// before it reads a file or the cluster, so nothing else runs. It returns nil
// when the verb returns without parsing flags.
func captureFlags(run func([]string), args []string) (fs *flag.FlagSet) {
	defer func() {
		flagCapture = nil
		if r := recover(); r != nil {
			c, ok := r.(capturedFlags)
			if !ok {
				panic(r)
			}
			fs = c.fs
		}
	}()
	flagCapture = func(f *flag.FlagSet) { panic(capturedFlags{f}) }
	run(args)
	return nil
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// flagName is how help writes a flag: -n for one letter, --name otherwise.
func flagName(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
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

// --- what each argument and flag value is ------------------------------------

// kind names what an argument or a flag value is, and so where its
// candidates come from.
type kind int

const (
	kindNone            kind = iota // free text: no candidates
	kindFile                        // a file name
	kindDir                         // a directory name
	kindVerb                        // a pk-metal command
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

// argKinds is what each positional argument of a verb is ("verb" or "verb
// sub"). The last entry repeats for the verbs that take several arguments
// (variadic). A verb that is not listed takes no argument that completes.
var argKinds = map[string]struct {
	kinds    []kindSpec
	variadic bool
}{
	"help":            {kinds: []kindSpec{{kind: kindVerb}}},
	"completion":      {kinds: []kindSpec{static("bash", "zsh", "fish")}},
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
	"n":                   {kind: kindNamespace},
	"status:o":            static("table", "json"),
	"backup get:o":        {kind: kindFile},
	"backup decrypt:o":    {kind: kindFile},
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
	verb    *command
	path    string            // "verb" or "verb sub"
	fs      *flag.FlagSet     // the flags of path; nil when unknown
	flags   map[string]string // flag values given on the line
	pos     []string          // positional arguments after path
	current string            // the word under the cursor
	// valueOf is the flag whose value the current word is ("" if none).
	valueOf string
}

// complete returns the candidates for the last word of words. connect is
// called at most once, and only when a candidate comes from the cluster.
func complete(words []string, connect func() (client.Client, error)) ([]candidate, directive) {
	if len(words) == 0 {
		words = []string{""}
	}
	current, before := words[len(words)-1], words[:len(words)-1]

	// Global flags in front of the verb.
	gfs := flag.NewFlagSet("pk-metal", flag.ContinueOnError)
	addGlobalFlags(gfs)
	l := line{flags: map[string]string{}, current: current}
	i := 0
	for ; i < len(before); i++ {
		w := before[i]
		if !strings.HasPrefix(w, "-") || w == "-" {
			break
		}
		if name, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "="); gfs.Lookup(name) == nil {
			return nil, compNone // not a global flag: the line fails anyway
		}
		if n := readFlag(gfs, before, &i, l.flags); n != "" {
			l.valueOf = n
		}
	}
	applyGlobal(l.flags)
	if l.valueOf != "" {
		return valueCandidates(l, gfs.Lookup(l.valueOf), connect)
	}
	if i == len(before) {
		if strings.HasPrefix(current, "-") {
			return flagCandidates(gfs, current), compNone
		}
		return verbCandidates(current), compNone
	}
	l.verb = lookup(before[i])
	if l.verb == nil {
		if before[i] == "help" {
			l.path = "help"
			l.pos = before[i+1:]
			return argCandidates(l, connect)
		}
		return nil, compNone
	}
	rest := before[i+1:]
	l.path = l.verb.name

	// A verb with subcommands: the first word after it names one.
	if subs := verbSubcommands(l.verb); subs != nil {
		if len(rest) == 0 {
			if strings.HasPrefix(current, "-") {
				return nil, compNone
			}
			return filter(words2cands(subs), current), compNone
		}
		if !slices.Contains(subs, rest[0]) {
			return nil, compNone
		}
		l.path += " " + rest[0]
		l.fs = captureFlags(l.verb.run, rest[:1])
		rest = rest[1:]
	} else {
		l.fs = captureFlags(l.verb.run, nil)
	}
	if l.fs == nil {
		return nil, compNone
	}

	for j := 0; j < len(rest); j++ {
		w := rest[j]
		if w == "--" {
			l.pos = append(l.pos, rest[j+1:]...)
			break
		}
		if strings.HasPrefix(w, "-") && w != "-" {
			l.valueOf = readFlag(l.fs, rest, &j, l.flags)
			continue
		}
		l.pos = append(l.pos, w)
	}
	applyGlobal(l.flags)
	if l.valueOf != "" {
		return valueCandidates(l, l.fs.Lookup(l.valueOf), connect)
	}
	if strings.HasPrefix(current, "-") && !slices.Contains(rest, "--") {
		if name, val, ok := strings.Cut(strings.TrimLeft(current, "-"), "="); ok {
			f := l.fs.Lookup(name)
			if f == nil || isBoolFlag(f) {
				return nil, compNone
			}
			lv := l
			lv.current = val
			cands, dir := valueCandidates(lv, f, connect)
			prefix := current[:len(current)-len(val)]
			for k := range cands {
				cands[k].Value = prefix + cands[k].Value
			}
			if len(cands) == 0 && dir != compNone {
				// The shells complete files only as whole words.
				return nil, compNone
			}
			return cands, compNone
		}
		return flagCandidates(l.fs, current), compNone
	}
	return argCandidates(l, connect)
}

// readFlag reads the flag at words[*i], records its value and moves *i past
// a value in the next word. It returns the flag's name when the value is the
// word under the cursor (the flag is the last word before it).
func readFlag(fs *flag.FlagSet, words []string, i *int, vals map[string]string) string {
	name, val, hasVal := strings.Cut(strings.TrimLeft(words[*i], "-"), "=")
	f := fs.Lookup(name)
	if f == nil || isBoolFlag(f) || hasVal {
		if hasVal {
			vals[name] = val
		}
		return ""
	}
	if *i+1 >= len(words) {
		return name
	}
	*i++
	vals[name] = words[*i]
	return ""
}

// applyGlobal makes --kubeconfig and --context from the line select the
// cluster that the lookups ask.
func applyGlobal(vals map[string]string) {
	if v, ok := vals["kubeconfig"]; ok {
		global.kubeconfig = v
	}
	if v, ok := vals["context"]; ok {
		global.context = v
	}
}

// --- candidates ----------------------------------------------------------------

func verbCandidates(prefix string) []candidate {
	cands := []candidate{{Value: "help", Desc: "flags and details of one command"}}
	for _, c := range commands {
		cands = append(cands, candidate{Value: c.name, Desc: c.summary})
	}
	return filter(cands, prefix)
}

func flagCandidates(fs *flag.FlagSet, prefix string) []candidate {
	var cands []candidate
	fs.VisitAll(func(f *flag.Flag) {
		cands = append(cands, candidate{Value: flagName(f.Name), Desc: f.Usage})
	})
	// -n and --n are the same flag to the parser; offer what help writes,
	// but accept what the person started to type.
	if strings.HasPrefix(prefix, "--") {
		for i := range cands {
			if !strings.HasPrefix(cands[i].Value, "--") {
				cands[i].Value = "-" + cands[i].Value
			}
		}
	}
	return filter(cands, prefix)
}

func argCandidates(l line, connect func() (client.Client, error)) ([]candidate, directive) {
	spec, ok := argKinds[l.path]
	if !ok {
		return nil, compNone
	}
	idx := len(l.pos)
	if idx >= len(spec.kinds) {
		if !spec.variadic {
			return nil, compNone
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

func valueCandidates(l line, f *flag.Flag, connect func() (client.Client, error)) ([]candidate, directive) {
	if f == nil {
		return nil, compNone
	}
	k, _ := flagKind(l.path, f.Name)
	return kindCandidates(k, l, connect)
}

func kindCandidates(k kindSpec, l line, connect func() (client.Client, error)) ([]candidate, directive) {
	switch k.kind {
	case kindNone:
		return nil, compNone
	case kindFile:
		return nil, compFiles
	case kindDir:
		return nil, compDirs
	case kindVerb:
		return verbCandidates(l.current), compNone
	case kindStatic:
		return filter(words2cands(k.words), l.current), compNone
	case kindFixture:
		if strings.ContainsAny(l.current, "./") {
			return nil, compFiles
		}
		return filter(words2cands(sim.Fixtures()), l.current), compNone
	case kindContext:
		return filter(contexts(global.kubeconfig), l.current), compNone
	case kindToContext:
		if l.flags["to"] == "" {
			return nil, compNone
		}
		return filter(contexts(l.flags["to"]), l.current), compNone
	}
	cl, err := connect()
	if err != nil {
		return nil, compNone
	}
	ctx, cancel := context.WithTimeout(context.Background(), completeTimeout)
	defer cancel()
	cands, _ := clusterCandidates(ctx, cl, k.kind, l)
	return filter(cands, l.current), compNone
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
	if v, ok := l.flags["n"]; ok {
		ns = v
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

// --- the hidden verb and the scripts -----------------------------------------

// completeMain is `pk-metal __complete <words>`. It never fails: a lookup
// that does not work gives no candidates.
func completeMain(args []string) {
	cands, dir := complete(args, completionClient)
	writeCandidates(os.Stdout, cands, dir)
}

func writeCandidates(w io.Writer, cands []candidate, dir directive) {
	for _, c := range cands {
		desc := strings.Join(strings.Fields(c.Desc), " ")
		if desc == "" {
			fmt.Fprintln(w, c.Value)
		} else {
			fmt.Fprintf(w, "%s\t%s\n", c.Value, desc)
		}
	}
	fmt.Fprintf(w, ":%d\n", dir)
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

func init() {
	verbHelp["completion"] = `
The script completes the commands, their subcommands and flags, and the
names of the objects that the arguments and flags take: operations, claims,
nodes, hosts, clusters, snapshots, rollouts, BMC addresses, namespaces and
kubeconfig contexts. The names come from the cluster that --kubeconfig and
--context on the same command line select (default: as for every other
command). Each lookup stops after 2 seconds; then there are no names.

Operations are shown by state: approve completes the operations that wait
for approval, retry the failed and aborted ones, abort the unfinished ones.
Type a "/" to complete namespace/name from every namespace.

bash (needs the bash-completion package):
  echo 'source <(pk-metal completion bash)' >> ~/.bashrc

zsh:
  echo 'source <(pk-metal completion zsh)' >> ~/.zshrc   (after compinit)

fish:
  pk-metal completion fish > ~/.config/fish/completions/pk-metal.fish

Open a new shell after the change.`
}

func completionCmd(args []string) {
	fs := newFlags("completion")
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		badUsage(fs, "give one shell: bash, zsh or fish")
	}
	script, ok := completionScripts[fs.Arg(0)]
	if !ok {
		badUsage(fs, fmt.Sprintf("unknown shell %q: bash, zsh or fish", fs.Arg(0)))
	}
	fmt.Print(script)
}

var completionScripts = map[string]string{
	"bash": bashCompletion,
	"zsh":  zshCompletion,
	"fish": fishCompletion,
}

// The scripts pass the words before the cursor and the current word to
// `pk-metal __complete`; the program that runs is the one on the command
// line, so a binary that is not on PATH (./bin/pk-metal) completes too.

const bashCompletion = `# bash completion for pk-metal. Load it with:
#   source <(pk-metal completion bash)
_pk_metal() {
    local cur words cword
    if declare -F _get_comp_words_by_ref >/dev/null 2>&1; then
        _get_comp_words_by_ref -n =: cur words cword
    else
        cur=${COMP_WORDS[COMP_CWORD]}
        words=("${COMP_WORDS[@]}")
        cword=$COMP_CWORD
    fi
    local out
    out=$("${words[0]}" ` + completeVerb + ` "${words[@]:1:cword-1}" "$cur" 2>/dev/null) || return
    local -a lines
    mapfile -t lines <<<"$out"
    local n=${#lines[@]}
    (( n > 0 )) || return
    local directive=${lines[n-1]#:}
    unset 'lines[n-1]'
    COMPREPLY=()
    case $directive in
    1) compopt -o filenames 2>/dev/null; mapfile -t COMPREPLY < <(compgen -f -- "$cur") ;;
    2) compopt -o filenames 2>/dev/null; mapfile -t COMPREPLY < <(compgen -d -- "$cur") ;;
    *)
        local l
        for l in "${lines[@]}"; do
            [[ -n $l ]] && COMPREPLY+=("${l%%$'\t'*}")
        done
        ;;
    esac
    # bash completes the part after "=" or ":" (COMP_WORDBREAKS) only.
    if [[ $cur == *=* && $COMP_WORDBREAKS == *=* ]]; then
        local p=${cur%%=*}=
        COMPREPLY=("${COMPREPLY[@]#"$p"}")
    fi
    if declare -F __ltrim_colon_completions >/dev/null 2>&1; then
        __ltrim_colon_completions "$cur"
    fi
}
complete -o nosort -F _pk_metal pk-metal 2>/dev/null || complete -F _pk_metal pk-metal
`

const zshCompletion = `#compdef pk-metal
# zsh completion for pk-metal. Load it with (after compinit):
#   source <(pk-metal completion zsh)
_pk_metal() {
    local out directive l
    local -a lines cands
    out=$(${words[1]} ` + completeVerb + ` "${(@)words[2,CURRENT]}" 2>/dev/null) || return 1
    lines=("${(@f)out}")
    (( ${#lines} > 0 )) || return 1
    directive=${lines[-1]#:}
    lines=("${(@)lines[1,-2]}")
    case $directive in
    1) _files; return ;;
    2) _files -/; return ;;
    esac
    for l in "${lines[@]}"; do
        [[ -n $l ]] || continue
        if [[ $l == *$'\t'* ]]; then
            cands+=("${${l%%$'\t'*}//:/\\:}:${l#*$'\t'}")
        else
            cands+=("${l//:/\\:}")
        fi
    done
    (( ${#cands} > 0 )) || return 1
    _describe -V -t values pk-metal cands
}
if [[ $funcstack[1] == _pk_metal ]]; then
    _pk_metal "$@"
else
    compdef _pk_metal pk-metal
fi
`

const fishCompletion = `# fish completion for pk-metal. Install it with:
#   pk-metal completion fish > ~/.config/fish/completions/pk-metal.fish
function __pk_metal_complete
    set -l tokens (commandline -opc)
    set -l current (commandline -ct)
    set -l out ($tokens[1] ` + completeVerb + ` $tokens[2..-1] $current 2>/dev/null)
    test (count $out) -gt 0; or return
    set -l directive (string replace -r '^:' '' -- $out[-1])
    set -e out[-1]
    switch $directive
        case 1
            __fish_complete_path $current
        case 2
            __fish_complete_directories $current
        case '*'
            printf '%s\n' $out
    end
end
complete -c pk-metal -f -a '(__pk_metal_complete)'
`
