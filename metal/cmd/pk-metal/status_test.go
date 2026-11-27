package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// rack is a small cluster: one healthy host, one host whose install failed,
// two BMCs (one dead), a bound claim, a claim that lost the race for its
// machine, and operations in every phase status shows differently.
func rack() topInput {
	hosts := []metalv1alpha1.Host{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "simx12b000001"},
			Status: metalv1alpha1.HostStatus{
				Identity: &metalv1alpha1.HostIdentity{Serial: "SIMX12B000001", Vendor: "Supermicro"},
				ManagedBy: []metalv1alpha1.ManagementPath{
					{EndpointRef: metalv1alpha1.Reference{Name: "ci-smc"}, SystemID: "1", Reachable: true},
				},
				Phase: metalv1alpha1.HostAvailable,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "simhw2288v5000391"},
			Spec: metalv1alpha1.HostSpec{
				ClaimRef: &metalv1alpha1.ClaimReference{Namespace: "default", Name: "fleet-a"},
			},
			Status: metalv1alpha1.HostStatus{
				Identity: &metalv1alpha1.HostIdentity{Serial: "SIMHW2288V5000391", Vendor: "Huawei"},
				ManagedBy: []metalv1alpha1.ManagementPath{
					{EndpointRef: metalv1alpha1.Reference{Name: "ci-huawei"}, SystemID: "1", Reachable: true},
					{EndpointRef: metalv1alpha1.Reference{Name: "ci-dead"}, SystemID: "1", Reachable: false},
				},
				Phase: metalv1alpha1.HostProvisioning,
			},
		},
	}
	eps := []metalv1alpha1.ManagementEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-dead"},
			Spec:       metalv1alpha1.ManagementEndpointSpec{AssetRef: metalv1alpha1.Reference{Name: "rack1-u12"}, Address: "https://127.0.0.1:1"},
			Status: metalv1alpha1.ManagementEndpointStatus{
				Phase: metalv1alpha1.EndpointUnreachable,
				Conditions: []metav1.Condition{{Type: metalv1alpha1.ConditionReachable, Status: metav1.ConditionFalse,
					Reason: "ConnectFailed", Message: "dial tcp 127.0.0.1:1: connection refused"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-huawei"},
			Spec:       metalv1alpha1.ManagementEndpointSpec{AssetRef: metalv1alpha1.Reference{Name: "rack1-u12"}, Address: "https://127.0.0.1:8443"},
			Status:     metalv1alpha1.ManagementEndpointStatus{Vendor: "Huawei", Phase: metalv1alpha1.EndpointReady},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-smc"},
			Spec:       metalv1alpha1.ManagementEndpointSpec{AssetRef: metalv1alpha1.Reference{Name: "rack1-u14"}, Address: "https://127.0.0.1:8441"},
			Status:     metalv1alpha1.ManagementEndpointStatus{Vendor: "Supermicro", Phase: metalv1alpha1.EndpointReady},
		},
	}
	failedMsg := "boot operation pk-metal/provision-x failed: step 6/9 awaitCheckIn: gave up after 30m0s"
	claims := []metalv1alpha1.HostClaim{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fleet-a"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: "rack1-u12"},
				OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "node-2"}},
			},
			Status: metalv1alpha1.HostClaimStatus{
				HostRef:   &metalv1alpha1.Reference{Name: "simhw2288v5000391"},
				Phase:     metalv1alpha1.ClaimBound,
				BlockedOn: metalv1alpha1.ReasonOperationFailed,
				Conditions: []metav1.Condition{
					{Type: metalv1alpha1.ConditionBound, Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonHostBound},
					{Type: metalv1alpha1.ConditionProvisioned, Status: metav1.ConditionFalse,
						Reason: metalv1alpha1.ReasonOperationFailed, Message: failedMsg},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fleet-b",
				Annotations: map[string]string{holdAnnotation: "disk swap"}},
			Spec: metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "rack1-u12"}},
			Status: metalv1alpha1.HostClaimStatus{
				Phase: metalv1alpha1.ClaimPending, BlockedOn: metalv1alpha1.ReasonHostAlreadyClaimed,
				Conditions: []metav1.Condition{{Type: metalv1alpha1.ConditionBound, Status: metav1.ConditionFalse,
					Reason: metalv1alpha1.ReasonHostAlreadyClaimed, Message: "host simhw2288v5000391 is claimed by default/fleet-a"}},
			},
		},
	}
	ops := []metalv1alpha1.HostOperation{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "cycle"},
			Spec:       metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "simx12b000001"}},
			Status: metalv1alpha1.HostOperationStatus{
				Phase: metalv1alpha1.OperationAwaitingApproval, BlockedOn: metalv1alpha1.ReasonAwaitingApproval},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "done"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:  metalv1alpha1.Reference{Name: "simx12b000001"},
				Approval: metalv1alpha1.OperationApproval{ApprovedBy: "alice"},
			},
			Status: metalv1alpha1.HostOperationStatus{Phase: metalv1alpha1.OperationSucceeded},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "wedged"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:  metalv1alpha1.Reference{Name: "simhw2288v5000391"},
				Approval: metalv1alpha1.OperationApproval{ApprovedBy: "alice"},
			},
			Status: metalv1alpha1.HostOperationStatus{
				Phase: metalv1alpha1.OperationFailed,
				Conditions: []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
					Reason: metalv1alpha1.ReasonStepFailed, Message: "step 6/9 awaitCheckIn: gave up after 30m0s"}},
			},
		},
	}
	nodes := []osv1alpha1.TalosNode{{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Spec:       osv1alpha1.TalosNodeSpec{Role: osv1alpha1.RoleWorker, Address: "10.0.0.12"},
	}}
	return topInput{Hosts: hosts, Endpoints: eps, Claims: claims, Ops: ops, Nodes: nodes}
}

func TestRenderStatus(t *testing.T) {
	in := rack()
	rep := buildStatus(in, nil)
	out := renderReport(rep)

	// Column content, not tabwriter's widths, is the contract: assert row shapes.
	assert.Regexp(t, `(?m)^HOST\s+ASSET\s+VENDOR\s+PHASE\s+CLAIM\s+NODE\s+PATHS\s+BLOCKED$`, out)
	assert.Regexp(t, `(?m)^simhw2288v5000391\s+rack1-u12\s+Huawei\s+Provisioning\s+default/fleet-a\s+node-2 \(10\.0\.0\.12\)\s+1/2\s+OperationFailed$`, out,
		"a host whose claim is stuck shows the claim's reason")
	assert.Regexp(t, `(?m)^simx12b000001\s+rack1-u14\s+Supermicro\s+Available\s+-\s+-\s+1/1\s*$`, out)
	assert.Contains(t, out, "\n  simhw2288v5000391: boot operation pk-metal/provision-x failed: step 6/9",
		"the controller's explanation is printed under the table")

	assert.Regexp(t, `(?m)^ENDPOINT\s+ASSET\s+VENDOR\s+PHASE\s+ADDRESS\s+BLOCKED$`, out)
	assert.Regexp(t, `(?m)^ci-dead\s+rack1-u12\s+-\s+Unreachable\s+https://127\.0\.0\.1:1\s*$`, out)
	assert.Regexp(t, `(?m)^ci-huawei\s+rack1-u12\s+Huawei\s+Ready\s+https://127\.0\.0\.1:8443\s*$`, out)
	assert.Contains(t, out, "\n  ci-dead: dial tcp 127.0.0.1:1: connection refused")

	assert.Regexp(t, `(?m)^default/fleet-b\s+rack1-u12\s+Pending\s+HostAlreadyClaimed \(on hold\)$`, out)
	assert.Contains(t, out, "\n  default/fleet-b: host simhw2288v5000391 is claimed by default/fleet-a")
	assert.NotRegexp(t, `(?m)^default/fleet-a\s`, out, "bound claims show on their host row only")

	assert.Regexp(t, `(?m)^OPERATION\s+HOST\s+PHASE\s+APPROVED\s+BLOCKED$`, out)
	assert.Regexp(t, `(?m)^pk-metal/cycle\s+simx12b000001\s+AwaitingApproval\s+-\s+AwaitingApproval$`, out)
	assert.Contains(t, out, "pk-metal/cycle: waits for approval; see the plan and approve: pk-metal approve pk-metal/cycle")
	assert.Regexp(t, `(?m)^pk-metal/wedged\s+simhw2288v5000391\s+Failed\s+alice\s*$`, out,
		"failures stay visible until retried")
	assert.Contains(t, out, "pk-metal/wedged: step 6/9 awaitCheckIn: gave up after 30m0s; to try again: pk-metal retry pk-metal/wedged")
	assert.NotContains(t, out, "pk-metal/done", "succeeded operations are history, not rack state")

	assert.True(t, rep.NeedsAttention)

	// No unbound claims → no claims section; no unfinished ops → no operations section.
	in.Claims, in.Ops = in.Claims[:1], in.Ops[1:2]
	out = renderReport(buildStatus(in, nil))
	assert.NotContains(t, out, "\nCLAIM")
	assert.NotContains(t, out, "OPERATION")
}

// TestStatusJSONShape pins the documented -o json keys: scripts depend on them.
func TestStatusJSONShape(t *testing.T) {
	b, err := json.Marshal(buildStatus(rack(), nil))
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	first := func(list string) map[string]any { return doc[list].([]any)[0].(map[string]any) }

	assert.ElementsMatch(t, []string{"version", "needsAttention", "hosts", "operations", "endpoints", "claims",
		"clusters", "nodes", "rollouts"}, keys(doc))
	assert.EqualValues(t, 1, doc["version"])
	assert.Equal(t, true, doc["needsAttention"])
	assert.ElementsMatch(t, []string{"name", "asset", "vendor", "phase", "claim", "node", "pathsUp", "pathsTotal",
		"blockedOn", "message", "needsAttention"}, keys(first("hosts")))
	assert.ElementsMatch(t, []string{"namespace", "name", "host", "phase", "approvedBy", "awaitsApproval", "blockedOn", "message",
		"needsAttention"}, keys(first("operations")))
	assert.ElementsMatch(t, []string{"name", "asset", "vendor", "phase", "address", "blockedOn", "message",
		"needsAttention"}, keys(first("endpoints")))
	assert.ElementsMatch(t, []string{"namespace", "name", "asset", "host", "phase", "blockedOn", "message", "onHold",
		"needsAttention"}, keys(first("claims")))

	// Sorted by name: simhw… before simx…; the claimed one carries its node.
	h := first("hosts")
	assert.Equal(t, "simhw2288v5000391", h["name"])
	assert.Equal(t, map[string]any{"name": "node-2", "address": "10.0.0.12"}, h["node"])
	assert.Nil(t, doc["hosts"].([]any)[1].(map[string]any)["node"], "no node is null, not a missing key")
	assert.Len(t, doc["operations"], 3, "json lists every operation, succeeded ones too")

	// An empty cluster still has every list, as [] and not null.
	b, err = json.Marshal(buildStatus(topInput{}, nil))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"needsAttention":false,"hosts":[],"operations":[],"endpoints":[],"claims":[],"clusters":[],"nodes":[],"rollouts":[]}`, string(b))
}

func TestStatusExitCodes(t *testing.T) {
	ctx := context.Background()
	in := rack()

	healthy := fakeClient(&in.Hosts[0], &in.Endpoints[2])
	var out bytes.Buffer
	code, err := runStatus(ctx, healthy, "table", &out)
	require.NoError(t, err)
	assert.Equal(t, exitOK, code, out.String())

	var objs []client.Object
	for i := range in.Hosts {
		objs = append(objs, &in.Hosts[i])
	}
	for i := range in.Endpoints {
		objs = append(objs, &in.Endpoints[i])
	}
	for i := range in.Claims {
		objs = append(objs, &in.Claims[i])
	}
	for i := range in.Ops {
		objs = append(objs, &in.Ops[i])
	}
	objs = append(objs, &in.Nodes[0])
	out.Reset()
	code, err = runStatus(ctx, fakeClient(objs...), "json", &out)
	require.NoError(t, err)
	assert.Equal(t, exitAttention, code, "something failed: exit 2")
	var rep statusReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
	assert.True(t, rep.NeedsAttention)

	// Waiting for approval alone needs a person too.
	code, err = runStatus(ctx, fakeClient(&in.Hosts[0], &in.Endpoints[2], &in.Ops[0]), "table", &out)
	require.NoError(t, err)
	assert.Equal(t, exitAttention, code)

	broken := fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("connection refused")
		},
	}).Build()
	code, err = runStatus(ctx, broken, "table", &out)
	assert.ErrorContains(t, err, "connection refused")
	assert.Equal(t, exitError, code)
}

// Status says that an operation waits for approval exactly when pk-metal
// approve lists it: a proposal that the controller did not read yet waits,
// and an approved operation whose status is old does not.
func TestStatusAwaitsApprovalAsApproveDoes(t *testing.T) {
	fresh := metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "fresh"},
		Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "simx12b000001"}}}
	approved := *fresh.DeepCopy()
	approved.Name, approved.Spec.Approval.ApprovedBy = "approved", "alice"
	approved.Status = metalv1alpha1.HostOperationStatus{Phase: metalv1alpha1.OperationAwaitingApproval,
		BlockedOn: metalv1alpha1.ReasonAwaitingApproval}
	rep := buildStatus(topInput{Ops: []metalv1alpha1.HostOperation{fresh, approved}}, nil)
	require.Len(t, rep.Operations, 2)
	assert.False(t, rep.Operations[0].AwaitsApproval)
	assert.True(t, rep.Operations[1].AwaitsApproval)
	assert.True(t, rep.Operations[1].NeedsAttention, "a proposal with no status yet needs a person")
	out := renderReport(rep)
	assert.Contains(t, out, "pk-metal/fresh: waits for approval; see the plan and approve: pk-metal approve pk-metal/fresh")
	assert.NotContains(t, out, "approve pk-metal/approved", "alice approved it already")
}

// A running operation says which step it is at and what that step waits
// for, in the controller's words.
func TestStatusShowsWhyARunningOperationWaits(t *testing.T) {
	msg := "step 5/9 powerCycle: BMC unavailable, retrying: connection refused"
	op := metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "run"},
		Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "simx12b000001"},
			Approval: metalv1alpha1.OperationApproval{ApprovedBy: "alice"}},
		Status: metalv1alpha1.HostOperationStatus{Phase: metalv1alpha1.OperationRunning, Conditions: []metav1.Condition{{
			Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionUnknown, Reason: metalv1alpha1.ReasonExecuting, Message: msg}}},
	}
	rep := buildStatus(topInput{Ops: []metalv1alpha1.HostOperation{op}}, nil)
	assert.Equal(t, msg, rep.Operations[0].Message)
	assert.False(t, rep.NeedsAttention, "a running operation is the plan working")
	assert.Contains(t, renderReport(rep), "\n  pk-metal/run: "+msg+"\n")
}

// A kind that the caller may not read fails the command: an empty ROLLOUT
// table or BACKUP column would read as nothing to do.
func TestStatusFailsOnAKindItMayNotRead(t *testing.T) {
	for _, kind := range []client.ObjectList{&osv1alpha1.TalosRolloutList{}, &osv1alpha1.EtcdBackupPolicyList{}} {
		cl := fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if fmt.Sprintf("%T", list) == fmt.Sprintf("%T", kind) {
					return apierrors.NewForbidden(schema.GroupResource{Group: "os.platform-kit.org"}, "", errors.New("no rule"))
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
		code, err := runStatus(context.Background(), cl, "json", io.Discard)
		assert.ErrorContains(t, err, "forbidden", "%T", kind)
		assert.Equal(t, exitError, code, "%T", kind)
	}
}

// TestStatusWithoutOSKinds: a cluster without the TalosNode kind installed
// still has a status; only the NODE address stays empty.
func TestStatusWithoutOSKinds(t *testing.T) {
	in := rack()
	cl := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(&in.Hosts[0], &in.Endpoints[2]).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*osv1alpha1.TalosNodeList); ok {
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "os.platform-kit.org", Kind: "TalosNode"}}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	var out bytes.Buffer
	code, err := runStatus(context.Background(), cl, "table", &out)
	require.NoError(t, err)
	assert.Equal(t, exitOK, code)
	assert.Contains(t, out.String(), "simx12b000001")
}
