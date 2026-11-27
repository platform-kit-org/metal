package v1alpha1

// HandedOffNodes names the TalosNodes that a machine runs since a finished
// install: a HostClaim recorded the hand-off of the node (status.handOff) to
// the Host whose spec.os names the node now. Such a node answered once, so
// it is expected to be Ready while its cluster runs. The record stays when
// the node stops answering. A node that pk-metal replace moved to another
// Host joins again only when its boot there records it.
func HandedOffNodes(claims []HostClaim, hosts []Host) map[string]bool {
	runs := map[string]string{}
	for i := range hosts {
		if os := hosts[i].Spec.OS; os != nil {
			runs[os.NodeRef.Name] = hosts[i].Name
		}
	}
	out := map[string]bool{}
	for i := range claims {
		if h := claims[i].Status.HandOff; h != nil && h.Host != "" && runs[h.Node] == h.Host {
			out[h.Node] = true
		}
	}
	return out
}
