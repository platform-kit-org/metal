package controller

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/drain"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	talosfake "github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// talosLab is a bootstrapped cluster "lab" of three control planes
// (cp-1..cp-3 at 10.0.0.1..3) and one worker (w-1 at 10.0.0.4), each behind a
// host host-<node>, its etcd, its Kubernetes Nodes, and an executor whose BMC
// never answers: every Talos step must run without one.
type talosLab struct {
	r        *HostOperationReconciler
	machines map[string]*talosfake.Machine
	etcd     *talosfake.EtcdCluster
	kube     client.Client
	bmcDials int
}

var labNodes = []struct {
	name string
	role osv1alpha1.NodeRole
	addr string
}{
	{"cp-1", osv1alpha1.RoleControlPlane, "10.0.0.1"},
	{"cp-2", osv1alpha1.RoleControlPlane, "10.0.0.2"},
	{"cp-3", osv1alpha1.RoleControlPlane, "10.0.0.3"},
	{"w-1", osv1alpha1.RoleWorker, "10.0.0.4"},
}

// bundleSecret is a Secret that holds the render package's test secrets
// bundle, as a person creates it from the output of `talosctl gen secrets`.
func bundleSecret(t *testing.T, namespace, name string) *corev1.Secret {
	t.Helper()
	bundle, err := os.ReadFile(filepath.Join("..", "talos", "render", "testdata", "secrets.yaml"))
	require.NoError(t, err)
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data: map[string][]byte{render.BundleKey: bundle}}
}

func newTalosLab(t *testing.T, objs ...client.Object) *talosLab {
	t.Helper()
	lab := &talosLab{machines: map[string]*talosfake.Machine{}, etcd: talosfake.NewEtcdCluster()}
	fleet := talosfake.NewFleet()

	cluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lab"},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion: "v1.14.6", Endpoint: "https://10.0.0.10:6443",
			SecretsRef: osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "lab-secrets"},
		},
	}
	cluster.Status.BootstrappedAt = ptr.To(metav1.Now())
	cluster.Status.KubeconfigSecretRef = &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "lab-kubeconfig"}
	objs = append(objs, cluster, bundleSecret(t, "pk-metal", "lab-secrets"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "lab-kubeconfig"},
			Data: map[string][]byte{KubeconfigKey: []byte("unused: the Workload seam answers")}})

	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	var kubeObjs []client.Object
	for _, n := range labNodes {
		node := &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: n.name},
			Spec:       osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"}, Role: n.role, Address: n.addr},
		}
		host := &metalv1alpha1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "host-" + n.name},
			Spec:       metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: n.name}}},
		}
		objs = append(objs, node, host)

		m := fleet.AddRunning(n.addr, n.name, "v1.14.6", []byte("machine: {}"))
		if n.role == osv1alpha1.RoleControlPlane {
			m.JoinEtcd(lab.etcd, false)
		}
		lab.machines[n.name] = m
		kubeObjs = append(kubeObjs, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: n.name},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			}},
		})
	}
	lab.kube = fake.NewClientBuilder().WithScheme(s).WithObjects(kubeObjs...).
		WithIndex(&corev1.Pod{}, drain.NodeNameField, func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).Build()

	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}).
		WithIndex(&metalv1alpha1.HostOperation{}, opHostIndex, func(o client.Object) []string {
			return []string{o.(*metalv1alpha1.HostOperation).Spec.HostRef.Name}
		}).
		WithObjects(objs...).Build()
	lab.r = &HostOperationReconciler{
		Client: cl, CredentialsNamespace: "pk-metal",
		Recorder: events.NewFakeRecorder(100),
		Connect: func(context.Context, *metalv1alpha1.Host) (bmc.Client, string, string, string, error) {
			lab.bmcDials++
			return nil, "", metalv1alpha1.ReasonNoReachablePath, "the BMC does not answer", nil
		},
		TalosDial: fleet.Dialer(),
		Workload: func(context.Context, []byte) (client.Client, error) {
			return lab.kube, nil
		},
		Guard: &ClusterGuard{Reader: cl, Dial: fleet.Dialer(), Namespace: "pk-metal"},
	}
	return lab
}

// talosOp is an operation on host-<node> that needs no approval.
func talosOp(node string, actions ...metalv1alpha1.HostOperationAction) *metalv1alpha1.HostOperation {
	op := preClearedOp(actions...)
	op.Spec.HostRef.Name = "host-" + node
	return op
}

func ref(node string) metalv1alpha1.Reference { return metalv1alpha1.Reference{Name: node} }

func rebootPlan(node string) []metalv1alpha1.HostOperationAction {
	return []metalv1alpha1.HostOperationAction{
		{Cordon: &metalv1alpha1.CordonAction{NodeRef: ref(node)}},
		{Drain: &metalv1alpha1.DrainAction{NodeRef: ref(node), TimeoutSeconds: 900}},
		{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: ref(node)}},
		{WaitNodeReady: &metalv1alpha1.WaitNodeReadyAction{NodeRef: ref(node)}},
		{Uncordon: &metalv1alpha1.UncordonAction{NodeRef: ref(node)}},
	}
}

// pass reconciles the operation once and returns it.
func (l *talosLab) pass(t *testing.T) *metalv1alpha1.HostOperation {
	t.Helper()
	key := client.ObjectKey{Namespace: "pk-metal", Name: "op"}
	_, err := l.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	op := &metalv1alpha1.HostOperation{}
	require.NoError(t, l.r.Get(t.Context(), key, op))
	return op
}

func executed(op *metalv1alpha1.HostOperation) metav1.Condition {
	if c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted); c != nil {
		return *c
	}
	return metav1.Condition{}
}

func (l *talosLab) unschedulable(t *testing.T, node string) bool {
	t.Helper()
	n := &corev1.Node{}
	require.NoError(t, l.kube.Get(t.Context(), client.ObjectKey{Name: node}, n))
	return n.Spec.Unschedulable
}

func TestTalosRebootRunsWithoutTheBMC(t *testing.T) {
	lab := newTalosLab(t, talosOp("w-1", rebootPlan("w-1")...))
	require.NoError(t, lab.kube.Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps"},
		Spec:       corev1.PodSpec{NodeName: "w-1"},
	}))

	op := runOp(t, lab.r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
	assert.Zero(t, lab.bmcDials, "no step needs the BMC, so none dials it")
	require.NotNil(t, op.Status.Baseline)
	assert.Empty(t, op.Status.Baseline.PowerState, "no BMC baseline")
	assert.Len(t, lab.machines["w-1"].Reboots(), 1, "asked once")
	assert.Equal(t, 1, lab.machines["w-1"].Boots())
	assert.False(t, lab.unschedulable(t, "w-1"), "uncordoned at the end")
	assert.Empty(t, op.Status.CordonedNode, "the uncordon hands the cordon back")
	pods := &corev1.PodList{}
	require.NoError(t, lab.kube.List(t.Context(), pods))
	assert.Empty(t, pods.Items, "drained")
}

func TestResumedTalosRebootDoesNotAskAgain(t *testing.T) {
	op := talosOp("w-1", metalv1alpha1.HostOperationAction{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: ref("w-1")}})
	op.Status = resumedStatus(0)
	op.Status.OffObserved = nil
	lab := newTalosLab(t, op)
	m := lab.machines["w-1"]
	// The previous pass recorded the boot time, asked, and died before
	// recording the request; the node has rebooted since.
	before := talosBootTime(t, lab, "w-1")
	setTalosStep(t, lab, &metalv1alpha1.TalosStepProgress{Step: 0, BootTime: &before})
	m.Boot()

	got := runOp(t, lab.r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Empty(t, m.Reboots(), "a boot time that moved on means the reboot took: it is not asked again")
}

func talosBootTime(t *testing.T, lab *talosLab, node string) (bt metav1.Time) {
	t.Helper()
	var addr string
	for _, n := range labNodes {
		if n.name == node {
			addr = n.addr
		}
	}
	acc := &talosAccess{dial: lab.r.TalosDial, talosconfig: []byte("tc")}
	require.NoError(t, acc.with(t.Context(), addr, func(c talos.Client) error {
		b, err := c.BootTime(t.Context())
		bt = metav1.Time{Time: b}
		return err
	}))
	return bt
}

func setTalosStep(t *testing.T, lab *talosLab, p *metalv1alpha1.TalosStepProgress) {
	t.Helper()
	op := &metalv1alpha1.HostOperation{}
	key := client.ObjectKey{Namespace: "pk-metal", Name: "op"}
	require.NoError(t, lab.r.Get(t.Context(), key, op))
	op.Status.TalosStep = p
	require.NoError(t, lab.r.Status().Update(t.Context(), op))
}

func TestTalosRebootWaitsForTheNodeToComeBack(t *testing.T) {
	lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: ref("w-1")}}))
	m := lab.machines["w-1"]
	m.SetStage("booting", false)

	lab.pass(t)
	require.Len(t, m.Reboots(), 1)
	op := lab.pass(t)
	assert.Equal(t, metav1.ConditionUnknown, executed(op).Status)
	assert.Contains(t, executed(op).Message, "restarted at")
	assert.Contains(t, executed(op).Message, "not ready yet")

	m.SetStage("running", true)
	op = lab.pass(t)
	assert.True(t, opSucceeded(op), "%+v", op.Status)
	assert.Len(t, m.Reboots(), 1)
}

func TestTalosUpgrade(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	upgrade := func(image string) metalv1alpha1.HostOperationAction {
		return metalv1alpha1.HostOperationAction{TalosUpgrade: &metalv1alpha1.TalosUpgradeAction{
			NodeRef: ref("w-1"), Image: image + digest, Version: "v1.14.7"}}
	}

	t.Run("runs the pinned image and waits for its version", func(t *testing.T) {
		lab := newTalosLab(t, talosOp("w-1", upgrade("factory.talos.dev/installer/x:v1.14.7")))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		ups := lab.machines["w-1"].Upgrades()
		require.Len(t, ups, 1)
		assert.Equal(t, "factory.talos.dev/installer/x:v1.14.7"+digest, ups[0].Image)
		assert.True(t, ups[0].Preserve)
		assert.Equal(t, "v1.14.7", lab.machines["w-1"].Version())
	})

	t.Run("a node on the version is not upgraded again", func(t *testing.T) {
		lab := newTalosLab(t, talosOp("w-1", upgrade("factory.talos.dev/installer/x:v1.14.7")))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op))
		require.NoError(t, lab.r.Delete(t.Context(), op))
		require.NoError(t, lab.r.Create(t.Context(), talosOp("w-1", upgrade("factory.talos.dev/installer/x:v1.14.7"))))
		op = runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		assert.Len(t, lab.machines["w-1"].Upgrades(), 1, "the second operation found the goal met")
		assert.Equal(t, 1, lab.machines["w-1"].Boots())
	})

	t.Run("a node that boots the old version fails the step", func(t *testing.T) {
		// The image's tag is what the fake installs: a mismatch stands in for
		// Talos falling back to the previous image.
		lab := newTalosLab(t, talosOp("w-1", upgrade("factory.talos.dev/installer/x:v1.14.6")))
		op := runOp(t, lab.r)
		assert.Equal(t, metav1.ConditionFalse, executed(op).Status)
		assert.Contains(t, executed(op).Message, "did not take")
	})
}

func TestTalosApplyConfigSendsOnlyTheApprovedBytes(t *testing.T) {
	cfg := []byte("version: v1alpha1\nmachine: {type: worker}\n")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "machineconfig-w-1"},
		Data: map[string][]byte{asset.ConfigFilename: cfg}}
	apply := func(digest, mode string) metalv1alpha1.HostOperationAction {
		return metalv1alpha1.HostOperationAction{TalosApplyConfig: &metalv1alpha1.TalosApplyConfigAction{
			NodeRef: ref("w-1"), ConfigSecretRef: ref("machineconfig-w-1"), Digest: digest, Mode: mode}}
	}

	t.Run("a changed Secret sends nothing", func(t *testing.T) {
		lab := newTalosLab(t, secret.DeepCopy(), talosOp("w-1", apply(godigest.FromString("other").String(), "NoReboot")))
		op := runOp(t, lab.r)
		assert.Equal(t, metav1.ConditionFalse, executed(op).Status)
		assert.Contains(t, executed(op).Message, "changed since approval")
		assert.Len(t, lab.machines["w-1"].Applied(), 1, "only the install config")
	})

	t.Run("no reboot", func(t *testing.T) {
		lab := newTalosLab(t, secret.DeepCopy(), talosOp("w-1", apply(godigest.FromBytes(cfg).String(), "NoReboot")))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		calls := lab.machines["w-1"].ApplyCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, talos.ApplyNoReboot, calls[0].Mode)
		assert.Equal(t, cfg, lab.machines["w-1"].Applied()[1])
		assert.Zero(t, lab.machines["w-1"].Boots())
		require.NotNil(t, op.Status.TalosStep)
		assert.Equal(t, "no-reboot", op.Status.TalosStep.Mode)
	})

	t.Run("reboot waits for the restart", func(t *testing.T) {
		lab := newTalosLab(t, secret.DeepCopy(), talosOp("w-1", apply(godigest.FromBytes(cfg).String(), "Reboot")))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		assert.Equal(t, 1, lab.machines["w-1"].Boots())
		assert.Len(t, lab.machines["w-1"].ApplyCalls(), 1, "applied once")
	})
}

func TestEtcdLeaveAndRemoveMember(t *testing.T) {
	t.Run("leave", func(t *testing.T) {
		lab := newTalosLab(t, talosOp("cp-3", metalv1alpha1.HostOperationAction{EtcdLeave: &metalv1alpha1.EtcdLeaveAction{NodeRef: ref("cp-3")}}))
		lab.r.Guard = nil // the guard has its own tests
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		assert.Len(t, lab.etcd.Members(), 2)
		assert.Empty(t, lab.machines["cp-3"].Reboots(), "leaving restarts nothing")
	})

	t.Run("remove a stale member through a peer", func(t *testing.T) {
		lab := newTalosLab(t)
		id := lab.etcd.AddStale("cp-4")
		require.NoError(t, lab.r.Create(t.Context(), talosOp("cp-3", metalv1alpha1.HostOperationAction{
			EtcdRemoveMember: &metalv1alpha1.EtcdRemoveMemberAction{NodeRef: ref("cp-3"), MemberID: strconv.FormatUint(id, 16), Hostname: "cp-4"}})))
		lab.machines["cp-1"].SetUnreachable(true) // the first peer is down: the next one is asked

		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		assert.Len(t, lab.etcd.Members(), 3)
		assert.Equal(t, []uint64{id}, lab.machines["cp-2"].RemovedByID())
	})
}

func TestEtcdForfeitLeadershipAndDeleteKubernetesNode(t *testing.T) {
	t.Run("the leader hands leadership on", func(t *testing.T) {
		lab := newTalosLab(t)
		leader := ""
		for _, m := range lab.etcd.Members() {
			if m.ID == lab.etcd.Leader() {
				leader = m.Hostname
			}
		}
		require.NotEmpty(t, leader)
		require.NoError(t, lab.r.Create(t.Context(), talosOp(leader, metalv1alpha1.HostOperationAction{
			EtcdForfeitLeadership: &metalv1alpha1.EtcdForfeitLeadershipAction{NodeRef: ref(leader)}})))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		assert.Len(t, lab.etcd.Members(), 3, "handing leadership on keeps every member")
		for _, m := range lab.etcd.Members() {
			if m.Hostname == leader {
				assert.NotEqual(t, m.ID, lab.etcd.Leader(), "another member leads now")
			}
		}
		assert.Empty(t, lab.machines[leader].Reboots(), "nothing restarts")
	})

	t.Run("a worker runs no member", func(t *testing.T) {
		lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{
			EtcdForfeitLeadership: &metalv1alpha1.EtcdForfeitLeadershipAction{NodeRef: ref("w-1")}}))
		op := runOp(t, lab.r)
		assert.Equal(t, metalv1alpha1.OperationFailed, op.Status.Phase)
		assert.Contains(t, executed(op).Message, "not a control plane")
	})

	t.Run("delete the Node, twice", func(t *testing.T) {
		lab := newTalosLab(t, talosOp("w-1",
			metalv1alpha1.HostOperationAction{DeleteKubernetesNode: &metalv1alpha1.DeleteKubernetesNodeAction{NodeRef: ref("w-1")}},
			// Already gone is the goal met, not a failure.
			metalv1alpha1.HostOperationAction{DeleteKubernetesNode: &metalv1alpha1.DeleteKubernetesNodeAction{NodeRef: ref("w-1")}}))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		err := lab.kube.Get(t.Context(), client.ObjectKey{Name: "w-1"}, &corev1.Node{})
		assert.True(t, apierrors.IsNotFound(err), "the Node is gone: %v", err)
		assert.Zero(t, lab.bmcDials, "neither step needs the BMC")
	})
}

func TestTalosResetAndShutdown(t *testing.T) {
	// Each look moves the clock on, so the node's silence lasts long enough.
	tickTalosClock(t, 20*time.Second)
	t.Run("reset leaves etcd and wipes", func(t *testing.T) {
		lab := newTalosLab(t, talosOp("cp-3", metalv1alpha1.HostOperationAction{TalosReset: &metalv1alpha1.TalosResetAction{
			NodeRef: ref("cp-3"), Graceful: ptr.To(true), Mode: "All"}}))
		lab.r.Guard = nil
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		resets := lab.machines["cp-3"].Resets()
		require.Len(t, resets, 1)
		assert.True(t, resets[0].Graceful)
		assert.Len(t, lab.etcd.Members(), 2, "a graceful reset leaves etcd")
	})

	t.Run("shutdown", func(t *testing.T) {
		lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{TalosShutdown: &metalv1alpha1.TalosShutdownAction{NodeRef: ref("w-1")}}))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		assert.Len(t, lab.machines["w-1"].Shutdowns(), 1)
	})
}

func TestWaitEtcdHealthyWaitsForALearner(t *testing.T) {
	lab := newTalosLab(t)
	// cp-3 rejoined as a learner: Talos promotes it once it caught up.
	lab.etcd = talosfake.NewEtcdCluster()
	for _, n := range []string{"cp-1", "cp-2"} {
		lab.machines[n].JoinEtcd(lab.etcd, false)
	}
	lab.machines["cp-3"].JoinEtcd(lab.etcd, true)
	require.NoError(t, lab.r.Create(t.Context(), talosOp("cp-3",
		metalv1alpha1.HostOperationAction{WaitEtcdHealthy: &metalv1alpha1.WaitEtcdHealthyAction{NodeRef: ref("cp-3")}})))

	op := lab.pass(t)
	assert.Contains(t, executed(op).Message, "member cp-3")
	assert.Contains(t, executed(op).Message, "learner")

	lab.etcd.Promote("cp-3")
	op = lab.pass(t)
	assert.True(t, opSucceeded(op), "%+v", op.Status)
}

func TestDrainRefusingEmptyDirDataFailsAndKeepsTheCordon(t *testing.T) {
	lab := newTalosLab(t, talosOp("w-1", rebootPlan("w-1")...))
	require.NoError(t, lab.kube.Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "apps"},
		Spec: corev1.PodSpec{NodeName: "w-1", Volumes: []corev1.Volume{{Name: "scratch",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}},
	}))

	op := runOp(t, lab.r)
	assert.Equal(t, metav1.ConditionFalse, executed(op).Status)
	assert.Contains(t, executed(op).Message, "step 2/5 drain")
	assert.Contains(t, executed(op).Message, "apps/cache")
	assert.True(t, lab.unschedulable(t, "w-1"), "the node stays cordoned")
	assert.Equal(t, "w-1", op.Status.CordonedNode)
	assert.Empty(t, lab.machines["w-1"].Reboots())
}

func TestTalosStepRefusesAnotherHostsNode(t *testing.T) {
	op := talosOp("w-1", metalv1alpha1.HostOperationAction{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: ref("cp-1")}})
	lab := newTalosLab(t, op)
	got := runOp(t, lab.r)
	assert.Equal(t, metav1.ConditionFalse, executed(got).Status)
	assert.Contains(t, executed(got).Message, "host host-w-1 runs TalosNode w-1")
	assert.Empty(t, lab.machines["cp-1"].Reboots())
}

func TestWaitNodeReadyWantsAReadyFromSinceTheBoot(t *testing.T) {
	lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{
		WaitNodeReady: &metalv1alpha1.WaitNodeReadyAction{NodeRef: ref("w-1")}}))
	booted := talosBootTime(t, lab, "w-1")
	n := &corev1.Node{}
	require.NoError(t, lab.kube.Get(t.Context(), client.ObjectKey{Name: "w-1"}, n))
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue,
		LastHeartbeatTime: metav1.NewTime(booted.Add(-time.Minute))}}
	require.NoError(t, lab.kube.Status().Update(t.Context(), n))

	op := lab.pass(t)
	assert.False(t, op.Finished(), "a Ready from before the boot is no answer from the kubelet")

	n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(booted.Add(time.Minute))
	require.NoError(t, lab.kube.Status().Update(t.Context(), n))
	op = runOp(t, lab.r)
	assert.True(t, opSucceeded(op), "%+v", op.Status)
}

// The Kubernetes API server of a control plane that reboots answers 503 for
// a while. An uncordon that meets it waits; before, the operation failed for
// good after the node had rebooted fine, and its rollout stopped.
func TestUncordonOnABusyAPIServerWaits(t *testing.T) {
	lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{Uncordon: &metalv1alpha1.UncordonAction{NodeRef: ref("w-1")}}))
	n := &corev1.Node{}
	require.NoError(t, lab.kube.Get(t.Context(), client.ObjectKey{Name: "w-1"}, n))
	n.Spec.Unschedulable = true
	require.NoError(t, lab.kube.Update(t.Context(), n))
	busy := 1
	refuse := func() error {
		if busy > 0 {
			busy--
			return apierrors.NewServiceUnavailable("the apiserver is shutting down")
		}
		return nil
	}
	kube := interceptor.NewClient(lab.kube.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if err := refuse(); err != nil {
				return err
			}
			return c.Patch(ctx, obj, p, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := refuse(); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	lab.r.Workload = func(context.Context, []byte) (client.Client, error) { return kube, nil }

	op := lab.pass(t)
	require.False(t, op.Finished(), "a busy API server is no verdict: %+v", op.Status)
	assert.Contains(t, executed(op).Message, "cluster API unavailable")
	assert.True(t, lab.unschedulable(t, "w-1"))

	op = runOp(t, lab.r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
	assert.False(t, lab.unschedulable(t, "w-1"))
}

// tickTalosClock makes every reading of the Talos steps' clock step forward,
// so a test sees time pass between two looks without waiting for it.
func tickTalosClock(t *testing.T, step time.Duration) {
	t.Helper()
	var mu sync.Mutex
	now := time.Now()
	prev := talosClock
	talosClock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(step)
		return now
	}
	t.Cleanup(func() { talosClock = prev })
}

func waiting(t *testing.T, op *metalv1alpha1.HostOperation, contains string) {
	t.Helper()
	assert.Equal(t, metav1.ConditionUnknown, executed(op).Status, "the step still waits: %s", executed(op).Message)
	assert.Contains(t, executed(op).Message, contains)
}

func resetOp(node string) *metalv1alpha1.HostOperation {
	return talosOp(node, metalv1alpha1.HostOperationAction{TalosReset: &metalv1alpha1.TalosResetAction{
		NodeRef: ref(node), Graceful: ptr.To(false), Mode: "SystemDisk",
		// STATE stays, so the fake keeps answering to the cluster's
		// credentials once it is reachable again.
		SystemPartitions: []metalv1alpha1.SystemPartition{"EPHEMERAL"}}})
}

func TestTalosResetIsNotDoneOnOneSilentLook(t *testing.T) {
	tickTalosClock(t, 20*time.Second)
	lab := newTalosLab(t, resetOp("w-1"))
	m := lab.machines["w-1"]

	lab.pass(t)
	require.Len(t, m.Resets(), 1)
	// The node still runs its reset: it answers, but one call is slow.
	m.SetUnreachable(false)
	m.SetFailure("BootTime", context.DeadlineExceeded)
	waiting(t, lab.pass(t), "has not answered since")

	m.SetFailure("BootTime", nil)
	waiting(t, lab.pass(t), "still answers")

	m.SetUnreachable(true)
	waiting(t, lab.pass(t), "has not answered since")
	waiting(t, lab.pass(t), "has not answered since")
	op := lab.pass(t)
	require.True(t, opSucceeded(op), "three silent looks over 40s end the step: %+v", op.Status)
	assert.Len(t, m.Resets(), 1, "a reset the node accepted is not sent again within the resend window")
}

func TestTalosResetLostAnswerIsAskedAgain(t *testing.T) {
	tickTalosClock(t, 20*time.Second)
	lab := newTalosLab(t, resetOp("w-1"))
	m := lab.machines["w-1"]
	m.SetFailure("Reset", status.Error(codes.Unavailable, "connection reset by peer"))

	op := lab.pass(t)
	waiting(t, op, "did not answer the request")
	require.NotNil(t, op.Status.TalosStep)
	assert.Nil(t, op.Status.TalosStep.RequestedAt, "a request without an answer is not recorded as accepted")
	assert.NotNil(t, op.Status.TalosStep.BootTime)
	assert.Empty(t, m.Resets())

	m.SetFailure("Reset", nil)
	lab.pass(t)
	assert.Len(t, m.Resets(), 1, "the node answers with its old boot time: the reset is sent again")
	op = runOp(t, lab.r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
	assert.Len(t, m.Resets(), 1)
}

func TestTalosResetDoneWhenTheNodeServesAnotherCA(t *testing.T) {
	lab := newTalosLab(t, resetOp("w-1"))
	m := lab.machines["w-1"]

	lab.pass(t)
	require.Len(t, m.Resets(), 1)
	m.SetUnreachable(false)
	m.SetFailure("BootTime", status.Error(codes.Unavailable, `connection error: desc = "transport: authentication `+
		`handshake failed: tls: failed to verify certificate: x509: certificate signed by unknown authority"`))
	op := lab.pass(t)
	assert.True(t, opSucceeded(op), "a node in maintenance mode is wiped: one look is enough: %+v", op.Status)
}

func TestTalosShutdownIsNotDoneOnOneSilentLook(t *testing.T) {
	tickTalosClock(t, 20*time.Second)
	lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{TalosShutdown: &metalv1alpha1.TalosShutdownAction{NodeRef: ref("w-1")}}))
	m := lab.machines["w-1"]

	lab.pass(t)
	require.Len(t, m.Shutdowns(), 1)
	m.SetUnreachable(false)
	m.SetFailure("Version", context.DeadlineExceeded)
	waiting(t, lab.pass(t), "has not answered since")
	m.SetFailure("Version", nil)
	waiting(t, lab.pass(t), "still answers")
	assert.Len(t, m.Shutdowns(), 1, "an accepted shutdown is not sent again: Talos would start it over")

	m.SetUnreachable(true)
	op := runOp(t, lab.r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
}

func TestRestartedNeedsMoreThanAClockStep(t *testing.T) {
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.False(t, restarted(before, before.Add(5*time.Second)), "a clock step on the node is no restart")
	assert.False(t, restarted(before, before.Add(-time.Hour)), "an earlier boot time is no restart")
	assert.True(t, restarted(before, before.Add(10*time.Minute)))
}

func TestTalosApplyConfigConfirmsATry(t *testing.T) {
	cfg := []byte("version: v1alpha1\nmachine: {type: worker}\n")
	secret := func() *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "machineconfig-w-1"},
			Data: map[string][]byte{asset.ConfigFilename: cfg}}
	}
	try := metalv1alpha1.HostOperationAction{TalosApplyConfig: &metalv1alpha1.TalosApplyConfigAction{
		NodeRef: ref("w-1"), ConfigSecretRef: ref("machineconfig-w-1"), Digest: godigest.FromBytes(cfg).String(),
		Mode: "Try", TryTimeoutSeconds: 30}}

	t.Run("a ready node is confirmed with the same bytes", func(t *testing.T) {
		lab := newTalosLab(t, secret(), talosOp("w-1", try))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		calls := lab.machines["w-1"].ApplyCalls()
		require.Len(t, calls, 2, "the try, then its confirmation")
		assert.Equal(t, talos.ApplyTry, calls[0].Mode)
		assert.Equal(t, talos.ApplyNoReboot, calls[1].Mode, "only an apply that is not a try cancels the rollback")
		applied := lab.machines["w-1"].Applied()
		assert.Equal(t, cfg, applied[len(applied)-1], "the confirmation sends the approved bytes")
	})

	t.Run("a node not ready within the window is left to roll back", func(t *testing.T) {
		tickTalosClock(t, 20*time.Second)
		lab := newTalosLab(t, secret(), talosOp("w-1", try))
		lab.machines["w-1"].SetStage("running", false, "nodeReady")
		op := lab.pass(t)
		waiting(t, op, "not ready yet")
		op = runOp(t, lab.r)
		assert.Equal(t, metav1.ConditionFalse, executed(op).Status)
		assert.Contains(t, executed(op).Message, "Try window")
		assert.Len(t, lab.machines["w-1"].ApplyCalls(), 1, "nothing confirmed the try")
	})
}

func TestTalosResetNamesTheUserDisks(t *testing.T) {
	tickTalosClock(t, 20*time.Second)
	lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{TalosReset: &metalv1alpha1.TalosResetAction{
		NodeRef: ref("w-1"), Graceful: ptr.To(false), Mode: "UserDisks", UserDisks: []string{"/dev/sdb", "/dev/sdc"}}}))
	m := lab.machines["w-1"]

	lab.pass(t)
	require.Len(t, m.Resets(), 1)
	assert.Equal(t, talos.WipeUserDisks, m.Resets()[0].Mode)
	assert.Equal(t, []string{"/dev/sdb", "/dev/sdc"}, m.Resets()[0].UserDisks, "Talos wipes only the user disks the reset names")
}

// A firmware step whose image went to the BMC has acted: an abort or the
// cluster guard waits for it to finish instead of leaving the manager
// halfway through a write.
func TestActedAfterFirmwareUpload(t *testing.T) {
	op := &metalv1alpha1.HostOperation{Spec: metalv1alpha1.HostOperationSpec{Actions: []metalv1alpha1.HostOperationAction{
		{EjectMedia: &metalv1alpha1.EjectMediaAction{}},
		{FlashFirmware: &metalv1alpha1.FlashFirmwareAction{}},
		{StageFirmware: &metalv1alpha1.StageFirmwareAction{}},
	}}}
	x := &exec{op: op}
	assert.False(t, x.acted(1), "nothing sent yet")
	assert.False(t, x.acted(2), "nothing sent yet")

	op.Status.UploadAccepted = &metalv1alpha1.FirmwareUploadProgress{Step: 1}
	assert.True(t, x.acted(1), "the flash image went to the BMC")
	assert.False(t, x.acted(2), "the record names another step")

	op.Status.UploadAccepted = &metalv1alpha1.FirmwareUploadProgress{Step: 2}
	assert.True(t, x.acted(2), "the staged image went to the BMC")
	assert.False(t, x.acted(1))
	assert.False(t, x.acted(0))
}
