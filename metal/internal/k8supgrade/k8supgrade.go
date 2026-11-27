// Package k8supgrade is the narrow part of `talosctl upgrade-k8s` that does
// not ride a machine config: moving the kube-proxy DaemonSet to the new
// version's image, and reading whether the cluster's nodes are Ready. The
// control-plane static pods and the kubelets move with the rendered config,
// applied without a reboot node by node; Talos itself restarts them.
//
// It talks to the workload cluster through a client that the caller opens
// with the admin kubeconfig the operator publishes, never through a talosctl
// binary or the main Talos module: each change is a step the rollout records.
package k8supgrade

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FieldManager owns the kube-proxy image field the upgrade sets.
const FieldManager = "pk-metal-k8s-upgrade"

// The kube-proxy DaemonSet Talos deploys, and its one container.
const (
	kubeProxyNamespace = "kube-system"
	kubeProxyName      = "kube-proxy"
	kubeProxyContainer = "kube-proxy"
)

// NodesReady reports, by node name, whether each Node is Ready. c reads now,
// not from a cache: a rollout gates on it between nodes.
func NodesReady(ctx context.Context, c client.Reader) (map[string]bool, error) {
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make(map[string]bool, len(nodes.Items))
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ready = c.Status == corev1.ConditionTrue
			}
		}
		out[n.Name] = ready
	}
	return out, nil
}

// SyncKubeProxy moves the kube-proxy DaemonSet to image and reports whether
// its rollout is done. A cluster without kube-proxy is done.
func SyncKubeProxy(ctx context.Context, c client.Client, image string) (bool, string, error) {
	ds := &appsv1.DaemonSet{}
	err := c.Get(ctx, client.ObjectKey{Namespace: kubeProxyNamespace, Name: kubeProxyName}, ds)
	if apierrors.IsNotFound(err) {
		return true, "the cluster runs no kube-proxy DaemonSet", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("get kube-proxy: %w", err)
	}
	var current string
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == kubeProxyContainer {
			current = c.Image
		}
	}
	if current == "" {
		return false, "", fmt.Errorf("kube-proxy DaemonSet has no %s container; not changing it", kubeProxyContainer)
	}
	if current != image {
		// A strategic merge patch keyed on the container name sets the
		// image and touches nothing else of the DaemonSet Talos manages.
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []map[string]any{{"name": kubeProxyContainer, "image": image}},
		}}}})
		if err := c.Patch(ctx, ds, client.RawPatch(types.StrategicMergePatchType, patch), client.FieldOwner(FieldManager)); err != nil {
			return false, "", fmt.Errorf("patch kube-proxy image to %s: %w", image, err)
		}
	}
	done, msg := rolledOut(ds)
	return done, msg, nil
}

// rolledOut reads a DaemonSet's rollout the way kubectl rollout status does.
func rolledOut(ds *appsv1.DaemonSet) (bool, string) {
	st := ds.Status
	switch {
	case st.ObservedGeneration < ds.Generation:
		return false, "kube-proxy: waiting for the DaemonSet controller to see the new image"
	case st.UpdatedNumberScheduled < st.DesiredNumberScheduled:
		return false, fmt.Sprintf("kube-proxy: %d of %d pods updated", st.UpdatedNumberScheduled, st.DesiredNumberScheduled)
	case st.NumberAvailable < st.DesiredNumberScheduled:
		return false, fmt.Sprintf("kube-proxy: %d of %d updated pods available", st.NumberAvailable, st.DesiredNumberScheduled)
	}
	return true, fmt.Sprintf("kube-proxy: %d pods run the new image", st.DesiredNumberScheduled)
}
