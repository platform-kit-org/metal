package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-clikit/cobratree/cobratreetest"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// commandPaths are the commands of the tree by their path after the program
// name ("verb" or "verb sub"); the root is "".
func commandPaths(root *cobra.Command) map[string]*cobra.Command {
	paths := map[string]*cobra.Command{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		paths[strings.TrimPrefix(strings.TrimPrefix(c.CommandPath(), root.Name()), " ")] = c
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return paths
}

// Every flag that takes a value has a decided kind, and every entry of
// flagKinds names a flag that exists.
func TestFlagKindsCoverEveryFlag(t *testing.T) {
	used := map[string]bool{}
	for path, c := range commandPaths(newRoot(noCluster)) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() == "bool" {
				return
			}
			switch {
			case hasKey(flagKinds, path+":"+f.Name):
				used[path+":"+f.Name] = true
			case hasKey(flagKinds, f.Name):
				used[f.Name] = true
			default:
				t.Errorf("%q: flag --%s has no entry in flagKinds", path, f.Name)
			}
		})
	}
	for key := range flagKinds {
		if !used[key] {
			t.Errorf("flagKinds[%q] names no flag that takes a value", key)
		}
	}
}

func hasKey[V any](m map[string]V, k string) bool { _, ok := m[k]; return ok }

func TestArgKindsNameRealPaths(t *testing.T) {
	paths := commandPaths(newRoot(noCluster))
	for key := range argKinds {
		if c, ok := paths[key]; !ok || c.HasSubCommands() {
			t.Errorf("argKinds[%q] names no command that takes arguments", key)
		}
	}
}

// complete runs cobra's hidden __complete command, as the shell scripts do
// on TAB, and returns the candidates and the directive.
func complete(t *testing.T, connect func() (client.Client, error), words ...string) ([]string, cobra.ShellCompDirective) {
	t.Helper()
	out, _, err := cobratreetest.Run(t, newRoot(connect), append([]string{cobra.ShellCompRequestCmd}, words...)...)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	dir, err := strconv.Atoi(strings.TrimPrefix(lines[len(lines)-1], ":"))
	require.NoError(t, err, out)
	var values []string
	for _, l := range lines[:len(lines)-1] {
		v, _, _ := strings.Cut(l, "\t")
		values = append(values, v)
	}
	return values, cobra.ShellCompDirective(dir)
}

const (
	noFiles = cobra.ShellCompDirectiveNoFileComp
	files   = cobra.ShellCompDirectiveDefault
	dirs    = cobra.ShellCompDirectiveFilterDirs
)

func TestCompleteStatic(t *testing.T) {
	cases := []struct {
		words []string
		want  []string
		dir   cobra.ShellCompDirective
	}{
		{[]string{"ro"}, []string{"rollout"}, noFiles},
		{[]string{"he"}, []string{"help"}, noFiles},
		{[]string{"help", "reb"}, []string{"reboot"}, noFiles},
		{[]string{"--"}, []string{"--context", "--help", "--kubeconfig"}, noFiles},
		{[]string{"--context", "x", "sta"}, []string{"status"}, noFiles},
		{[]string{"rollout", "p"}, []string{"pause"}, noFiles},
		{[]string{"backup", ""}, []string{"now", "list", "get", "decrypt"}, noFiles},
		{[]string{"sim", ""}, []string{"serve", "fleet"}, noFiles},
		{[]string{"rollout", "approve", "--y"}, []string{"--yes"}, noFiles},
		{[]string{"rollout", "show", "--y"}, nil, noFiles},
		{[]string{"approve", "-"}, []string{"--context", "--kubeconfig", "--help", "-h", "--namespace", "-n", "--yes"}, noFiles},
		{[]string{"status", "-o", ""}, []string{"table", "json"}, noFiles},
		{[]string{"status", "--output=j"}, []string{"json"}, noFiles},
		{[]string{"reboot", "--mode", "p"}, []string{"powercycle"}, noFiles},
		{[]string{"reboot", "--mode=p"}, []string{"powercycle"}, noFiles},
		{[]string{"upgrade", ""}, []string{"talos", "kubernetes"}, noFiles},
		{[]string{"completion", ""}, []string{"bash", "zsh", "fish", "powershell"}, noFiles},
		{[]string{"completion", "bash", ""}, nil, noFiles},
		{[]string{"--kubeconfig", ""}, nil, files},
		{[]string{"status", "--kubeconfig", ""}, nil, files},
		{[]string{"render", "--dir", ""}, nil, dirs},
		{[]string{"render", ""}, nil, noFiles},
		{[]string{"backup", "get", "x", "--to-file", ""}, nil, files},
		{[]string{"sim", "serve", "--fixture", "./"}, nil, files},
		{[]string{"approve", "--", "-"}, nil, noFiles}, // after "--" a dash is an argument
		{[]string{"nosuchverb", ""}, nil, noFiles},
		{[]string{"rollout", "nosuchsub", ""}, nil, noFiles},
	}
	for _, tc := range cases {
		got, dir := complete(t, noCluster, tc.words...)
		if !slices.Equal(got, tc.want) || dir != tc.dir {
			t.Errorf("complete(%q) = %q, %d; want %q, %d", tc.words, got, dir, tc.want, tc.dir)
		}
	}
}

func TestCompleteFixtures(t *testing.T) {
	got, _ := complete(t, noCluster, "sim", "serve", "--fixture", "")
	if !slices.Contains(got, "generic") {
		t.Errorf("fixtures = %q; want generic among them", got)
	}
}

func op(ns, name, host string, approvedBy string, executed metav1.ConditionStatus, reason string) *metalv1alpha1.HostOperation {
	o := &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	o.Spec.HostRef.Name = host
	o.Spec.Approval.Required = ptr.To(true)
	o.Spec.Approval.ApprovedBy = approvedBy
	if executed != "" {
		o.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: executed, Reason: reason}}
	}
	return o
}

func TestCompleteFromCluster(t *testing.T) {
	cl := fakeClient(
		op("pk-metal", "provision-a", "m1", "", "", ""),
		op("pk-metal", "provision-b", "m2", "alice", "", ""),
		op("pk-metal", "reboot-c", "m3", "alice", metav1.ConditionFalse, "Failed"),
		op("lab", "provision-d", "m4", "", "", ""),
		&metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cp-1"}},
		&metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "w-1"}},
		&osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		&metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "m1"}, Status: metalv1alpha1.HostStatus{Phase: metalv1alpha1.HostAvailable}},
		&metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "m2"}, Status: metalv1alpha1.HostStatus{Phase: metalv1alpha1.HostProvisioned}},
		&osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab"}},
		&osv1alpha1.EtcdBackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: "lab-hourly"}},
		&osv1alpha1.EtcdSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "lab-20261126"}},
		&osv1alpha1.TalosRollout{ObjectMeta: metav1.ObjectMeta{Name: "lab-upgrade-1"}},
		&metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "bmc-1"},
			Spec: metalv1alpha1.ManagementEndpointSpec{Address: "https://10.0.0.5"}},
	)
	connects := 0
	connect := func() (client.Client, error) { connects++; return cl, nil }
	cases := []struct {
		words []string
		want  []string
	}{
		{[]string{"approve", ""}, []string{"provision-a"}},
		{[]string{"approve", "provision-a", ""}, nil},
		{[]string{"approve", "-n", "lab", ""}, []string{"provision-d"}},
		{[]string{"approve", "--namespace=lab", ""}, []string{"provision-d"}},
		{[]string{"approve", "lab/"}, []string{"lab/provision-d"}},
		{[]string{"approve", "pk-metal/"}, []string{"pk-metal/provision-a"}},
		{[]string{"abort", ""}, []string{"provision-a", "provision-b"}},
		{[]string{"retry", ""}, []string{"reboot-c"}},
		{[]string{"restore", "upload", "-n", "pk-metal", "re"}, []string{"reboot-c"}},
		{[]string{"hold", ""}, []string{"cp-1"}},
		{[]string{"hold", "team/"}, []string{"team/w-1"}},
		{[]string{"reinstall", ""}, []string{"cp-1", "node-1"}},
		{[]string{"reboot", ""}, []string{"node-1"}},
		{[]string{"wipe", ""}, []string{"m1", "m2"}},
		{[]string{"replace", "--host", ""}, []string{"m1"}},
		{[]string{"scale", "--workers", "2", ""}, []string{"lab"}},
		{[]string{"upgrade", "talos", ""}, []string{"lab"}},
		{[]string{"upgrade", "talos", "lab", ""}, nil},
		{[]string{"backup", "now", ""}, []string{"lab", "lab-hourly"}},
		{[]string{"backup", "get", ""}, []string{"lab-20261126"}},
		{[]string{"restore", "plan", "lab", "--snapshot", ""}, []string{"lab-20261126"}},
		{[]string{"rollout", "approve", ""}, []string{"lab-upgrade-1"}},
		{[]string{"check", ""}, []string{"https://10.0.0.5"}},
		{[]string{"status", "-n", ""}, nil}, // status has no -n
	}
	for _, tc := range cases {
		got, _ := complete(t, connect, tc.words...)
		if !slices.Equal(got, tc.want) {
			t.Errorf("complete(%q) = %q; want %q", tc.words, got, tc.want)
		}
	}
	if connects == 0 {
		t.Fatal("no case asked the cluster")
	}
}

// A cluster that cannot be reached gives no candidates and no error.
func TestCompleteWithoutCluster(t *testing.T) {
	got, dir := complete(t, noCluster, "approve", "")
	if len(got) != 0 || dir != noFiles {
		t.Errorf("complete without a cluster = %q, %d; want nothing", got, dir)
	}
}

const twoContexts = `apiVersion: v1
kind: Config
current-context: lab
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {}}]
contexts:
- {name: lab, context: {cluster: c, user: u}}
- {name: prod, context: {cluster: c, user: u}}
`

func TestCompleteContextsFromTheLine(t *testing.T) {
	saved := global
	t.Cleanup(func() { global = saved })
	file := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(file, []byte(twoContexts), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, words := range [][]string{
		{"--kubeconfig", file, "--context", ""},
		{"status", "--kubeconfig", file, "--context", ""},
		{"--kubeconfig=" + file, "status", "--context", ""},
		{"pivot", "--to", file, "--to-context", ""},
	} {
		got, _ := complete(t, noCluster, words...)
		if !slices.Equal(got, []string{"lab", "prod"}) {
			t.Errorf("complete(%q) = %q; want lab, prod", words, got)
		}
	}
	if got, _ := complete(t, noCluster, "status", "--kubeconfig", file, "--context", "p"); !slices.Equal(got, []string{"prod"}) {
		t.Errorf("prefix p = %q; want prod", got)
	}
}
