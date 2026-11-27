package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Exit codes of pk-metal status, for cron jobs and monitoring checks.
const (
	exitOK        = 0
	exitError     = 1 // the cluster could not be read
	exitAttention = 2 // something is blocked or failed and needs a person
)

func init() {
	verbHelp["status"] = `
Tables:
  HOST       one row per machine: its asset, vendor, phase, the claim that
             holds it, the Talos node it runs (name and address), how many of
             its BMC paths answer, and what blocks it
  OPERATION  each operation that has not succeeded: waiting for approval,
             running, or failed (failed ones stay until you retry them)
  ENDPOINT   one row per BMC
  CLAIM      claims that have no machine yet
  CLUSTER    one row per TalosCluster: Talos and Kubernetes versions wanted
             and running, ready control planes and workers, etcd health,
             when it was bootstrapped, the backup state, and whether its
             Kubernetes API is available
  NODE       one row per TalosNode: its cluster, role, the host it runs on,
             address, Talos version, boot stage and readiness
  ROLLOUT    each TalosRollout that has not succeeded: its cluster, kind,
             nodes done of the plan, where it stands (the reason it waits)
             and who approved it
Under each table, every blocked or failed row gets a line that says why, in
the operator's own words, and what to run next when there is a command for it.

-o json prints the same facts as one JSON object, for scripts:
  {"version": 1, "needsAttention": bool,
   "hosts":      [{"name", "asset", "vendor", "phase", "claim",
                   "node": {"name", "address"} or null,
                   "pathsUp", "pathsTotal", "blockedOn", "message", "needsAttention"}],
   "operations": [{"namespace", "name", "host", "phase", "approvedBy",
                   "blockedOn", "message", "needsAttention"}],
   "endpoints":  [{"name", "asset", "vendor", "phase", "address",
                   "blockedOn", "message", "needsAttention"}],
   "claims":     [{"namespace", "name", "asset", "host", "phase",
                   "blockedOn", "message", "onHold", "needsAttention"}],
   "clusters":   [{"name", "talosVersion", "talosVersionsRunning",
                   "kubernetesVersion", "kubernetesVersionRunning",
                   "controlPlanes", "readyControlPlanes", "workers",
                   "readyWorkers", "available", "etcd", "bootstrappedAt",
                   "backup", "backupMessage", "message", "needsAttention"}],
   "nodes":      [{"name", "cluster", "role", "host", "address", "version",
                   "stage", "ready", "message", "needsAttention"}],
   "rollouts":   [{"name", "cluster", "kind", "nodesDone", "nodesTotal",
                   "state", "approvedBy", "message", "needsAttention"}]}
  Every key is always present; a missing value is "" (or null for node).
  "operations", "claims" and "rollouts" list all of them, also succeeded and
  bound ones.
  New keys may be added; existing keys keep their meaning while version is 1.

exit status:
  0  nothing needs a person
  1  error: the cluster could not be read
  2  something is blocked or failed (a row has needsAttention), including
     operations that wait for approval, a bootstrapped cluster whose etcd or
     Kubernetes API is unhealthy or whose backup is stale, a node of a
     running cluster that is not Ready, and a rollout that waits for
     approval, cannot plan, pauses after its first node, or failed

examples:
  pk-metal status
  watch -n 10 pk-metal status
  pk-metal status -o json | jq '.hosts[] | select(.needsAttention)'`
}

// statusCmd shows the rack from the cluster's point of view. It reads the
// cluster only; it never talks to a BMC.
func statusCmd(args []string) {
	fs := newFlags("status")
	format := fs.String("o", "table", "output format: table or json")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		badUsage(fs, "status takes no arguments")
	}
	if *format != "table" && *format != "json" {
		badUsage(fs, fmt.Sprintf("unknown output format %q; use table or json", *format))
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	code, err := runStatus(context.Background(), cl, *format, os.Stdout)
	if err != nil {
		die(err)
	}
	os.Exit(code)
}

// runStatus reads the cluster, prints the report and returns the exit code.
func runStatus(ctx context.Context, cl client.Client, format string, w io.Writer) (int, error) {
	var in topInput
	kinds, fill := in.kinds()
	// The BACKUP column comes from the backup policies, which top does not show.
	policies := &osv1alpha1.EtcdBackupPolicyList{}
	for _, k := range append(kinds, kindList{name: "EtcdBackupPolicy", list: policies, optional: true}) {
		if err := cl.List(ctx, k.list); err != nil && (!k.optional || !meta.IsNoMatchError(err)) {
			return exitError, fmt.Errorf("read the cluster: %w", err)
		}
	}
	fill()
	rep := buildStatus(in, policies.Items)
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return exitError, err
		}
	default:
		fmt.Fprint(w, renderReport(rep))
	}
	if rep.NeedsAttention {
		return exitAttention, nil
	}
	return exitOK, nil
}

// statusReport is the -o json shape (version 1). Keep the keys stable: scripts
// and monitoring checks read them.
type statusReport struct {
	Version        int           `json:"version"`
	NeedsAttention bool          `json:"needsAttention"`
	Hosts          []hostStatus  `json:"hosts"`
	Operations     []opStatus    `json:"operations"`
	Endpoints      []epStatus    `json:"endpoints"`
	Claims         []claimStatus `json:"claims"`
	// Clusters, Nodes and Rollouts came later (still version 1: new keys only).
	Clusters []clusterStatus   `json:"clusters"`
	Nodes    []talosNodeStatus `json:"nodes"`
	Rollouts []rolloutStatus   `json:"rollouts"`
}

type nodeStatus struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type hostStatus struct {
	Name           string      `json:"name"`
	Asset          string      `json:"asset"`
	Vendor         string      `json:"vendor"`
	Phase          string      `json:"phase"`
	Claim          string      `json:"claim"`
	Node           *nodeStatus `json:"node"`
	PathsUp        int         `json:"pathsUp"`
	PathsTotal     int         `json:"pathsTotal"`
	BlockedOn      string      `json:"blockedOn"`
	Message        string      `json:"message"`
	NeedsAttention bool        `json:"needsAttention"`
}

type opStatus struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Host           string `json:"host"`
	Phase          string `json:"phase"`
	ApprovedBy     string `json:"approvedBy"`
	BlockedOn      string `json:"blockedOn"`
	Message        string `json:"message"`
	NeedsAttention bool   `json:"needsAttention"`
}

type epStatus struct {
	Name           string `json:"name"`
	Asset          string `json:"asset"`
	Vendor         string `json:"vendor"`
	Phase          string `json:"phase"`
	Address        string `json:"address"`
	BlockedOn      string `json:"blockedOn"`
	Message        string `json:"message"`
	NeedsAttention bool   `json:"needsAttention"`
	host           string // a Host that this BMC manages
}

type claimStatus struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Asset          string `json:"asset"`
	Host           string `json:"host"`
	Phase          string `json:"phase"`
	BlockedOn      string `json:"blockedOn"`
	Message        string `json:"message"`
	OnHold         bool   `json:"onHold"`
	NeedsAttention bool   `json:"needsAttention"`
}

// explain finds the sentence a controller wrote for why the object is stuck:
// the condition that carries the blockedOn reason, else the first of the
// failure condition types that is False.
func explain(conds []metav1.Condition, blockedOn string, failTypes ...string) string {
	if blockedOn != "" {
		if strings.HasPrefix(blockedOn, "Approval(") {
			if c := meta.FindStatusCondition(conds, metalv1alpha1.ConditionApprovalPending); c != nil && c.Message != "" {
				return c.Message
			}
			// A claim has no ApprovalPending: its Provisioned condition
			// waits with the reason AwaitingApproval.
			blockedOn = metalv1alpha1.ReasonAwaitingApproval
		}
		for _, c := range conds {
			if c.Reason == blockedOn && c.Message != "" {
				return c.Message
			}
		}
	}
	for _, t := range failTypes {
		if c := meta.FindStatusCondition(conds, t); c != nil && c.Status == metav1.ConditionFalse && c.Message != "" {
			return c.Message
		}
	}
	return ""
}

func nsName(namespace, name string) string { return namespace + "/" + name }

// buildStatus is the pure part of status and top: objects in, report out,
// sorted by name so that two runs on the same cluster print the same thing.
// top reads no backup policies.
func buildStatus(in topInput, policies []osv1alpha1.EtcdBackupPolicy) statusReport {
	rep := statusReport{Version: 1, Hosts: []hostStatus{}, Operations: []opStatus{}, Endpoints: []epStatus{}, Claims: []claimStatus{},
		Clusters: []clusterStatus{}, Nodes: []talosNodeStatus{}, Rollouts: []rolloutStatus{}}
	addMachines(&rep, in)
	addClusters(&rep, in.Clusters, in.Nodes, policies)
	addRollouts(&rep, in.Rollouts)
	rep.NeedsAttention = len(attentionKeys(rep)) > 0
	return rep
}

// attentionKeys names each thing in the report that needs a person, once: a
// machine (its Host, its operations, its bound claim and its node are one
// machine), a BMC that no Host lists, a claim without a machine, a cluster,
// a node without a Host, and a rollout. status exits 2 when there is one; top
// counts them.
func attentionKeys(rep statusReport) map[string]bool {
	keys := map[string]bool{}
	add := func(yes bool, host, own string) {
		if yes {
			keys[cmp.Or(host, own)] = true
		}
	}
	for _, r := range rep.Hosts {
		add(r.NeedsAttention, r.Name, "")
	}
	for _, r := range rep.Operations {
		add(r.NeedsAttention, r.Host, "operation/"+nsName(r.Namespace, r.Name))
	}
	for _, r := range rep.Endpoints {
		add(r.NeedsAttention, r.host, "endpoint/"+r.Name)
	}
	for _, r := range rep.Claims {
		add(r.NeedsAttention, r.Host, "claim/"+nsName(r.Namespace, r.Name))
	}
	for _, r := range rep.Clusters {
		add(r.NeedsAttention, "", "cluster/"+r.Name)
	}
	for _, r := range rep.Nodes {
		add(r.NeedsAttention, r.Host, "node/"+r.Name)
	}
	for _, r := range rep.Rollouts {
		add(r.NeedsAttention, "", "rollout/"+r.Name)
	}
	return keys
}

// addMachines fills the HOST, OPERATION, ENDPOINT and CLAIM rows.
func addMachines(rep *statusReport, in topInput) {
	epByName := map[string]*metalv1alpha1.ManagementEndpoint{}
	for i := range in.Endpoints {
		epByName[in.Endpoints[i].Name] = &in.Endpoints[i]
	}
	claimByRef := map[string]*metalv1alpha1.HostClaim{}
	for i := range in.Claims {
		claimByRef[nsName(in.Claims[i].Namespace, in.Claims[i].Name)] = &in.Claims[i]
	}
	nodeByName := map[string]*osv1alpha1.TalosNode{}
	for i := range in.Nodes {
		nodeByName[in.Nodes[i].Name] = &in.Nodes[i]
	}
	hostOfEp := map[string]string{}

	for _, h := range in.Hosts {
		row := hostStatus{Name: h.Name, Phase: string(h.Status.Phase), BlockedOn: h.Status.BlockedOn,
			PathsTotal: len(h.Status.ManagedBy)}
		if h.Status.Identity != nil {
			row.Vendor = h.Status.Identity.Vendor
		}
		var paths []metalv1alpha1.ManagementEndpoint
		for _, p := range h.Status.ManagedBy {
			if p.Reachable {
				row.PathsUp++
			}
			if ep := epByName[p.EndpointRef.Name]; ep != nil {
				paths = append(paths, *ep)
			}
			hostOfEp[p.EndpointRef.Name] = h.Name
		}
		row.Asset = strings.Join(assetsOf(paths), ",")
		row.Message = explain(h.Status.Conditions, h.Status.BlockedOn,
			metalv1alpha1.ConditionBooted, metalv1alpha1.ConditionOSReady)

		var claim *metalv1alpha1.HostClaim
		if ref := h.Spec.ClaimRef; ref != nil {
			row.Claim = nsName(ref.Namespace, ref.Name)
			claim = claimByRef[row.Claim]
		}
		nodeName := ""
		if h.Spec.OS != nil {
			nodeName = h.Spec.OS.NodeRef.Name
		}
		if claim != nil {
			if nodeName == "" && claim.Spec.OS != nil {
				nodeName = claim.Spec.OS.NodeRef.Name
			}
			// The host is fine but its claim is stuck (an install that failed,
			// say): that is what holds this machine up.
			if row.BlockedOn == "" && claim.Status.BlockedOn != "" {
				row.BlockedOn = claim.Status.BlockedOn
				row.Message = explain(claim.Status.Conditions, claim.Status.BlockedOn)
			}
		}
		if nodeName != "" {
			row.Node = &nodeStatus{Name: nodeName}
			if n := nodeByName[nodeName]; n != nil {
				row.Node.Address = nodeAddress(n)
			}
		}
		row.NeedsAttention = row.BlockedOn != "" || h.Status.Phase == metalv1alpha1.HostFailed
		rep.Hosts = append(rep.Hosts, row)
	}

	for _, op := range in.Ops {
		row := opStatus{Namespace: op.Namespace, Name: op.Name, Host: op.Spec.HostRef.Name,
			Phase: string(op.Status.Phase), ApprovedBy: op.Spec.Approval.ApprovedBy, BlockedOn: op.Status.BlockedOn}
		row.Message = explain(op.Status.Conditions, op.Status.BlockedOn, metalv1alpha1.ConditionExecuted)
		// Queued behind another operation or waiting for its maintenance
		// window is the plan working, not something a person must do.
		row.NeedsAttention = op.Status.Phase == metalv1alpha1.OperationFailed || op.Status.Phase == metalv1alpha1.OperationAborted ||
			(op.Status.BlockedOn != "" && op.Status.BlockedOn != metalv1alpha1.ReasonOperationQueued &&
				op.Status.BlockedOn != metalv1alpha1.ReasonOutsideMaintenanceWindow)
		rep.Operations = append(rep.Operations, row)
	}

	for _, ep := range in.Endpoints {
		row := epStatus{Name: ep.Name, Asset: ep.Spec.AssetRef.Name, Vendor: ep.Status.Vendor,
			Phase: string(ep.Status.Phase), Address: ep.Spec.Address, BlockedOn: ep.Status.BlockedOn, host: hostOfEp[ep.Name]}
		row.Message = explain(ep.Status.Conditions, ep.Status.BlockedOn,
			metalv1alpha1.ConditionReachable, metalv1alpha1.ConditionVerified)
		row.NeedsAttention = row.BlockedOn != "" || ep.Status.Phase == metalv1alpha1.EndpointUnreachable ||
			ep.Status.Phase == metalv1alpha1.EndpointMismatch
		rep.Endpoints = append(rep.Endpoints, row)
	}

	for _, c := range in.Claims {
		row := claimStatus{Namespace: c.Namespace, Name: c.Name, Asset: c.Spec.AssetRef.Name,
			Phase: string(c.Status.Phase), BlockedOn: c.Status.BlockedOn}
		if c.Status.HostRef != nil {
			row.Host = c.Status.HostRef.Name
		}
		_, row.OnHold = c.Annotations[holdAnnotation]
		row.Message = explain(c.Status.Conditions, c.Status.BlockedOn)
		row.NeedsAttention = row.BlockedOn != "" || c.Status.Phase == metalv1alpha1.ClaimAmbiguous
		rep.Claims = append(rep.Claims, row)
	}

	slices.SortFunc(rep.Hosts, func(a, b hostStatus) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(rep.Endpoints, func(a, b epStatus) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(rep.Operations, func(a, b opStatus) int {
		return strings.Compare(nsName(a.Namespace, a.Name), nsName(b.Namespace, b.Name))
	})
	slices.SortFunc(rep.Claims, func(a, b claimStatus) int {
		return strings.Compare(nsName(a.Namespace, a.Name), nsName(b.Namespace, b.Name))
	})
}

// renderReport prints the tables: hosts first (the machines), unfinished
// operations, endpoints (the BMCs), then claims that have no machine yet —
// bound ones already show on their host row. Under each table, the reason for
// every row that needs a person.
func renderReport(rep statusReport) string {
	var sb strings.Builder
	section := func(header string, rows [][]string, notes []string) {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		w := tabwriter.NewWriter(&sb, 2, 0, 2, ' ', 0)
		fmt.Fprintln(w, header)
		for _, r := range rows {
			fmt.Fprintln(w, strings.Join(r, "\t"))
		}
		_ = w.Flush()
		for _, n := range notes {
			fmt.Fprintf(&sb, "  %s\n", n)
		}
	}

	var rows [][]string
	var notes []string
	for _, h := range rep.Hosts {
		node := "-"
		if h.Node != nil {
			node = h.Node.Name
			if h.Node.Address != "" {
				node += " (" + h.Node.Address + ")"
			}
		}
		rows = append(rows, []string{h.Name, orDash(h.Asset), orDash(h.Vendor), orDash(h.Phase), orDash(h.Claim), node,
			fmt.Sprintf("%d/%d", h.PathsUp, h.PathsTotal), h.BlockedOn})
		if h.NeedsAttention {
			notes = append(notes, note(h.Name, h.Message, h.BlockedOn))
		}
	}
	section("HOST\tASSET\tVENDOR\tPHASE\tCLAIM\tNODE\tPATHS\tBLOCKED", rows, notes)

	// Succeeded operations are history, not state of the rack; failures stay
	// visible until someone retries them.
	rows, notes = nil, nil
	for _, op := range rep.Operations {
		if op.Phase == string(metalv1alpha1.OperationSucceeded) {
			continue
		}
		ref := nsName(op.Namespace, op.Name)
		rows = append(rows, []string{ref, op.Host, orDash(op.Phase), orDash(op.ApprovedBy), op.BlockedOn})
		switch {
		case op.Phase == string(metalv1alpha1.OperationFailed):
			notes = append(notes, note(ref, op.Message, "failed")+"; to try again: pk-metal retry "+ref)
		case op.Phase == string(metalv1alpha1.OperationAborted):
			notes = append(notes, note(ref, op.Message, "aborted")+"; to run it again: pk-metal retry "+ref)
		case op.Phase == string(metalv1alpha1.OperationAwaitingApproval) || (op.ApprovedBy == "" && op.BlockedOn == metalv1alpha1.ReasonAwaitingApproval):
			notes = append(notes, ref+": waits for approval; see the plan and approve: pk-metal approve "+ref)
		case op.NeedsAttention:
			notes = append(notes, note(ref, op.Message, op.BlockedOn))
		}
	}
	if len(rows) > 0 {
		section("OPERATION\tHOST\tPHASE\tAPPROVED\tBLOCKED", rows, notes)
	}

	rows, notes = nil, nil
	for _, ep := range rep.Endpoints {
		rows = append(rows, []string{ep.Name, orDash(ep.Asset), orDash(ep.Vendor), orDash(ep.Phase), ep.Address, ep.BlockedOn})
		if ep.NeedsAttention {
			notes = append(notes, note(ep.Name, ep.Message, ep.Phase))
		}
	}
	section("ENDPOINT\tASSET\tVENDOR\tPHASE\tADDRESS\tBLOCKED", rows, notes)

	rows, notes = nil, nil
	for _, c := range rep.Claims {
		if c.Phase == string(metalv1alpha1.ClaimBound) {
			continue
		}
		ref := nsName(c.Namespace, c.Name)
		blocked := c.BlockedOn
		if c.OnHold {
			blocked = strings.TrimSpace(blocked + " (on hold)")
		}
		rows = append(rows, []string{ref, c.Asset, orDash(c.Phase), blocked})
		if c.NeedsAttention {
			notes = append(notes, note(ref, c.Message, c.BlockedOn))
		}
	}
	if len(rows) > 0 {
		section("CLAIM\tASSET\tPHASE\tBLOCKED", rows, notes)
	}
	renderClusters(rep, section)
	renderRollouts(rep, section)
	return sb.String()
}

// note is the "why" line under a table: the controller's sentence, or the bare
// reason when it wrote none.
func note(name, message, fallback string) string {
	if message == "" {
		message = fallback
	}
	return name + ": " + message
}
