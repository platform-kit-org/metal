package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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

// restConfig loads the connection --kubeconfig/--context name, by the
// kubectl rules.
func restConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = global.kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: global.context}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot load a kubeconfig: %w", err)
	}
	return cfg, nil
}

// splitRef reads "name" or "namespace/name", the form pk-metal status prints.
// nsFlag is the -n value, nsSet whether -n was given; a namespace in both
// places must agree.
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
	case nsSet && ns != nsFlag:
		return "", "", fmt.Errorf("%q names namespace %s, but -n says %s; give only one", arg, ns, nsFlag)
	}
	return ns, n, nil
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
