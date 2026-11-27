package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Helpers for the map functions of the watches. A map function lists what
// an event touches and ends in one of these.

// requestFor is the request of the cluster-scoped object name.
func requestFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}

// requestsFor is the requests of the cluster-scoped objects names.
func requestsFor(names ...string) []ctrl.Request {
	reqs := make([]ctrl.Request, 0, len(names))
	for _, n := range names {
		reqs = append(reqs, requestFor(n))
	}
	return reqs
}

// requestsOf is the requests of the items that keep accepts. A nil keep
// accepts every item. Each request has the namespace of its item.
func requestsOf[T any, P interface {
	*T
	client.Object
}](items []T, keep func(P) bool) []ctrl.Request {
	var reqs []ctrl.Request
	for i := range items {
		if o := P(&items[i]); keep == nil || keep(o) {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(o)})
		}
	}
	return reqs
}

// hostsWhere lists the Hosts and returns the requests of those that keep
// accepts. A nil keep accepts every Host.
func hostsWhere(ctx context.Context, c client.Reader, keep func(*metalv1alpha1.Host) bool) []ctrl.Request {
	hosts := &metalv1alpha1.HostList{}
	if err := c.List(ctx, hosts); err != nil {
		return nil
	}
	return requestsOf(hosts.Items, keep)
}

// hostsOfEndpoint maps an endpoint event to the Hosts that the endpoint
// fronts. A deleted endpoint's last status still names them, so each Host
// runs again on the paths that are left.
func hostsOfEndpoint(_ context.Context, obj client.Object) []ctrl.Request {
	return requestsFor(hostNames(obj.(*metalv1alpha1.ManagementEndpoint))...)
}
