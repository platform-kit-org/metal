package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// verbPaths are every "verb" and "verb sub" with the flag set it parses.
func verbPaths(t *testing.T) map[string]*flag.FlagSet {
	t.Helper()
	paths := map[string]*flag.FlagSet{}
	for i := range commands {
		c := &commands[i]
		subs := verbSubcommands(c)
		if subs == nil {
			paths[c.name] = captureFlags(c.run, nil)
			continue
		}
		for _, sub := range subs {
			paths[c.name+" "+sub] = captureFlags(c.run, []string{sub})
		}
	}
	return paths
}

func TestCaptureFlagsReachesEveryVerb(t *testing.T) {
	for path, fs := range verbPaths(t) {
		if fs == nil {
			t.Errorf("%s: the verb returned before parseFlags, so its flags do not complete", path)
		}
	}
}

// Every flag that takes a value has a decided kind, and every entry of
// flagKinds names a flag that exists.
func TestFlagKindsCoverEveryFlag(t *testing.T) {
	used := map[string]bool{}
	for path, fs := range verbPaths(t) {
		if fs == nil {
			continue
		}
		fs.VisitAll(func(f *flag.Flag) {
			if isBoolFlag(f) {
				return
			}
			switch {
			case hasKey(flagKinds, path+":"+f.Name):
				used[path+":"+f.Name] = true
			case hasKey(flagKinds, f.Name):
				used[f.Name] = true
			default:
				t.Errorf("%s: flag --%s has no entry in flagKinds", path, f.Name)
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
	paths := verbPaths(t)
	for key := range argKinds {
		if _, ok := paths[key]; !ok && key != "help" {
			t.Errorf("argKinds[%q] names no command", key)
		}
	}
}

func values(c []candidate) []string {
	v := make([]string, 0, len(c))
	for _, x := range c {
		v = append(v, x.Value)
	}
	return v
}

func noCluster() (client.Client, error) { return nil, errors.New("no cluster in this test") }

func TestCompleteStatic(t *testing.T) {
	cases := []struct {
		words []string
		want  []string
		dir   directive
	}{
		{[]string{"ro"}, []string{"rollout"}, compNone},
		{[]string{"he"}, []string{"help"}, compNone},
		{[]string{"help", "reb"}, []string{"reboot"}, compNone},
		{[]string{"--"}, []string{"--context", "--kubeconfig"}, compNone},
		{[]string{"--context", "x", "sta"}, []string{"status"}, compNone},
		{[]string{"rollout", "p"}, []string{"pause"}, compNone},
		{[]string{"backup", ""}, []string{"now", "list", "get", "decrypt"}, compNone},
		{[]string{"sim", ""}, []string{"serve", "fleet"}, compNone},
		{[]string{"rollout", "approve", "--y"}, []string{"--yes"}, compNone},
		{[]string{"rollout", "show", "--y"}, nil, compNone},
		{[]string{"status", "-o", ""}, []string{"table", "json"}, compNone},
		{[]string{"status", "-o=j"}, []string{"-o=json"}, compNone},
		{[]string{"reboot", "--mode", "p"}, []string{"powercycle"}, compNone},
		{[]string{"reboot", "--mode=p"}, []string{"--mode=powercycle"}, compNone},
		{[]string{"upgrade", ""}, []string{"talos", "kubernetes"}, compNone},
		{[]string{"completion", ""}, []string{"bash", "zsh", "fish"}, compNone},
		{[]string{"completion", "bash", ""}, nil, compNone},
		{[]string{"--kubeconfig", ""}, nil, compFiles},
		{[]string{"status", "--kubeconfig", ""}, nil, compFiles},
		{[]string{"render", "--dir", ""}, nil, compDirs},
		{[]string{"backup", "get", "x", "-o", ""}, nil, compFiles},
		{[]string{"sim", "serve", "--fixture", "./"}, nil, compFiles},
		{[]string{"approve", "--", "-"}, nil, compNone}, // after "--" a dash is an argument
		{[]string{"nosuchverb", ""}, nil, compNone},
		{[]string{"rollout", "nosuchsub", ""}, nil, compNone},
	}
	for _, tc := range cases {
		got, dir := complete(tc.words, noCluster)
		if !slices.Equal(values(got), tc.want) || dir != tc.dir {
			t.Errorf("complete(%q) = %q, %d; want %q, %d", tc.words, values(got), dir, tc.want, tc.dir)
		}
	}
}

func TestCompleteFixtures(t *testing.T) {
	got, _ := complete([]string{"sim", "serve", "--fixture", ""}, noCluster)
	if !slices.Contains(values(got), "generic") {
		t.Errorf("fixtures = %q; want generic among them", values(got))
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
		{[]string{"-n", ""}, nil}, // not a global flag
		{[]string{"status", "-n", ""}, nil},
	}
	for _, tc := range cases {
		got, dir := complete(tc.words, connect)
		if !slices.Equal(values(got), tc.want) || dir != compNone {
			t.Errorf("complete(%q) = %q, %d; want %q", tc.words, values(got), dir, tc.want)
		}
	}
	if connects == 0 {
		t.Fatal("no case asked the cluster")
	}
}

// A cluster that cannot be reached gives no candidates and no error.
func TestCompleteWithoutCluster(t *testing.T) {
	got, dir := complete([]string{"approve", ""}, noCluster)
	if len(got) != 0 || dir != compNone {
		t.Errorf("complete without a cluster = %q, %d; want nothing", values(got), dir)
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
		got, _ := complete(words, noCluster)
		if !slices.Equal(values(got), []string{"lab", "prod"}) {
			t.Errorf("complete(%q) = %q; want lab, prod", words, values(got))
		}
	}
	if got, _ := complete([]string{"status", "--kubeconfig", file, "--context", "p"}, noCluster); !slices.Equal(values(got), []string{"prod"}) {
		t.Errorf("prefix p = %q; want prod", values(got))
	}
}

func TestWriteCandidates(t *testing.T) {
	var b bytes.Buffer
	writeCandidates(&b, []candidate{{Value: "a"}, {Value: "b", Desc: "two\n  lines"}}, compFiles)
	if want := "a\nb\ttwo lines\n:1\n"; b.String() != want {
		t.Errorf("output = %q; want %q", b.String(), want)
	}
}

// The scripts are valid shell, and the bash one completes through the
// binary end to end.
func TestCompletionScripts(t *testing.T) {
	for shell, check := range map[string][]string{"bash": {"bash", "-n"}, "zsh": {"zsh", "-n"}, "fish": {"fish", "--no-execute"}} {
		if _, err := exec.LookPath(check[0]); err != nil {
			t.Logf("%s is not installed; its script is not checked", shell)
			continue
		}
		cmd := exec.Command(check[0], check[1:]...)
		cmd.Stdin = strings.NewReader(completionScripts[shell])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script: %v\n%s", shell, err, out)
		}
	}
}

// buildPkMetal builds the program into a temporary directory once per test.
func buildPkMetal(t *testing.T) (dir, bin string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "pk-metal")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return dir, bin
}

func TestCompletionScriptBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	dir, bin := buildPkMetal(t)
	script := completionScripts["bash"] + `
COMP_WORDS=(` + bin + ` rollout ap); COMP_CWORD=2; _pk_metal; printf '%s\n' "${COMPREPLY[@]}"
COMP_WORDS=(` + bin + ` reboot --mode po); COMP_CWORD=3; _pk_metal; printf '%s\n' "${COMPREPLY[@]}"
`
	cmd := exec.Command("bash", "--norc", "--noprofile", "-c", script)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(dir, "none"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	if got, want := string(out), "approve\npowercycle\n"; got != want {
		t.Errorf("bash completions = %q; want %q", got, want)
	}
}

func TestCompletionScriptFish(t *testing.T) {
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish is not installed")
	}
	dir, _ := buildPkMetal(t)
	script := completionScripts["fish"] + `
complete -C "pk-metal rollout ap"
complete -C "pk-metal status -o j"
`
	cmd := exec.Command("fish", "--no-config", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "KUBECONFIG="+filepath.Join(dir, "none"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fish: %v\n%s", err, out)
	}
	if got, want := string(out), "approve\njson\n"; got != want {
		t.Errorf("fish completions = %q; want %q", got, want)
	}
}
