package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
)

// operatorNamespace is where the operator creates its HostOperations (the
// manager's --credentials-namespace default). HostClaims usually live
// elsewhere, so the claim verbs default to "default".
const operatorNamespace = "pk-metal"

// The labels the operator puts on the operations it proposes for a claim.
const (
	labelClaimNamespace = "metal.platform-kit.org/claim-namespace"
	labelClaimName      = "metal.platform-kit.org/claim-name"
)

// claimNamespace is where HostClaims usually live: they are the user's
// requests, not the operator's records.
const claimNamespace = "default"

// newScheme knows every kind the CLI reads or writes.
func newScheme() *runtime.Scheme {
	s, err := pkscheme.New()
	if err != nil {
		panic(err) // static registration; fails only on a programming error
	}
	return s
}

// newClient connects to the cluster that --kubeconfig/--context name, or the
// default the kubectl rules pick ($KUBECONFIG, ~/.kube/config, in-cluster).
func newClient() (client.Client, error) {
	cfg, err := restConfig()
	if err != nil {
		return nil, err
	}
	cl, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the cluster at %s: %w", cfg.Host, err)
	}
	return cl, nil
}

// mustClient is newClient for a verb: it exits 1 on an error.
func mustClient() client.Client {
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	return cl
}

// onCluster runs a verb that talks to the cluster: it connects, runs run
// under signalContext, and exits 1 with run's error.
func onCluster(run func(ctx context.Context, cl client.Client) error) {
	cl := mustClient()
	ctx, stop := signalContext()
	defer stop()
	if err := run(ctx, cl); err != nil {
		die(err)
	}
}

// restConfig loads the connection --kubeconfig/--context name, by the
// kubectl rules.
func restConfig() (*rest.Config, error) {
	cfg, _, err := namedRestConfig(global.kubeconfig, global.context)
	return cfg, err
}

// splitRef reads "name" or "namespace/name", the form pk-metal status prints,
// or kubectl's "type.group/name". nsFlag is the -n value, nsSet whether -n was
// given; a namespace in both places must agree.
func splitRef(arg, nsFlag string, nsSet bool) (namespace, name string, err error) {
	ns, n, found := strings.Cut(arg, "/")
	switch {
	case !found:
		if arg == "" {
			return "", "", fmt.Errorf("empty name")
		}
		return nsFlag, arg, nil
	case ns == "" || n == "" || strings.Contains(n, "/"):
		return "", "", fmt.Errorf("%q is not a name or namespace/name", arg)
	case strings.Contains(ns, "."):
		// kubectl's TYPE/NAME (kubectl get -o name): a namespace has no dot.
		return nsFlag, n, nil
	case nsSet && ns != nsFlag:
		return "", "", fmt.Errorf("%q names namespace %s, but -n says %s; give only one", arg, ns, nsFlag)
	}
	return ns, n, nil
}

// oneRef reads the one "name" or "namespace/name" argument of a verb with a
// -n flag, as splitRef does.
func oneRef(fs *flag.FlagSet, ns, what string) (namespace, name string) {
	if fs.NArg() != 1 {
		badUsage(fs, "give exactly one "+what)
	}
	namespace, name, err := splitRef(fs.Arg(0), ns, flagGiven(fs, "n"))
	if err != nil {
		badUsage(fs, err.Error())
	}
	return namespace, name
}

// getNamed reads one namespaced object. When it is not in namespace, it looks
// for the name in every namespace and says where it is, so that a missing or
// wrong -n is a one-step fix.
func getNamed(ctx context.Context, cl client.Client, kind string, obj client.Object, list client.ObjectList,
	namespace, name string) error {
	err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
	if !apierrors.IsNotFound(err) {
		return err
	}
	return notFoundHint(ctx, cl, kind, list, namespace, name)
}

// getCluster reads one cluster-scoped object by the name a person gave. When
// there is none, it says where to find the names: list is that clause.
func getCluster(ctx context.Context, cl client.Client, kind string, obj client.Object, name, list string) error {
	err := cl.Get(ctx, client.ObjectKey{Name: name}, obj)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("no %s %s; %s", kind, name, list)
	case err != nil:
		return fmt.Errorf("read %s %s: %w", kind, name, err)
	}
	return nil
}

// The list clauses of getCluster for the kinds that more than one verb reads.
const (
	listHosts    = "a Host has the name of its serial in lower case, and pk-metal status lists them"
	listClusters = "kubectl get talosclusters lists them"
)

func notFoundHint(ctx context.Context, cl client.Client, kind string, list client.ObjectList, namespace, name string) error {
	msg := fmt.Sprintf("no %s %s in namespace %s", kind, name, namespace)
	if err := cl.List(ctx, list); err != nil {
		return fmt.Errorf("%s", msg)
	}
	items, err := apimeta.ExtractList(list)
	if err != nil {
		return fmt.Errorf("%s", msg)
	}
	var found []string
	for _, item := range items {
		if o, ok := item.(client.Object); ok && o.GetName() == name {
			found = append(found, o.GetNamespace())
		}
	}
	switch len(found) {
	case 0:
		return fmt.Errorf("%s, and in no other namespace", msg)
	case 1:
		return fmt.Errorf("%s; it is in namespace %s: use %s/%s", msg, found[0], found[0], name)
	}
	return fmt.Errorf("%s; it is in namespaces %s: name one as <namespace>/%s",
		msg, strings.Join(found, ", "), name)
}

// mergeSpec is what a JSON merge patch sets. UID and ResourceVersion, when
// given, make the API server refuse the patch (409 Conflict) when the object
// was made again under the same name, or changed in any way since it was
// read. An annotation with a nil value is removed. Fields that are empty
// are not sent: a nil Annotations map must not delete every annotation.
type mergeSpec struct {
	UID             types.UID
	ResourceVersion string
	Annotations     map[string]any
	Spec            map[string]any
}

// mergePatch sends m as a merge patch of obj. The patch is marshalled, not
// formatted: Go's %q escapes are not JSON's, and a user name comes from
// whatever the identity provider allows.
func mergePatch(ctx context.Context, cl client.Client, obj client.Object, m mergeSpec) error {
	meta := map[string]any{}
	if m.UID != "" {
		meta["uid"] = m.UID
	}
	if m.ResourceVersion != "" {
		meta["resourceVersion"] = m.ResourceVersion
	}
	if m.Annotations != nil {
		meta["annotations"] = m.Annotations
	}
	body := map[string]any{}
	if len(meta) > 0 {
		body["metadata"] = meta
	}
	if m.Spec != nil {
		body["spec"] = m.Spec
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return cl.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
}

// refused is the error a person reads when the API server refuses to do
// action. An admission policy's denial carries the policy's own message,
// which says what to do; the prefix that names the policy and its binding is
// left out. Any other error is passed on, after the action.
func refused(action string, err error) error {
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		return fmt.Errorf("%s: %w", action, err)
	}
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, "denied request: "); ok {
		msg = after
	}
	return fmt.Errorf("the cluster refused to %s: %s", action, msg)
}
