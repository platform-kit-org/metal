package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func abortCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "abort [-n namespace] [--reason text] <operation> | <namespace>/<operation>",
		Short:       "stop an operation before its next step and undo what it left behind (media, boot override, cordon)",
		GroupID:     groupChanges,
		Annotations: needsCluster(),
		Long: `abort asks the operator to stop an operation. An operation that has not
started ends at once; nothing was done to the machine. A running one stops
before its next step. A step that already acted on the machine is finished
first: a reboot that was asked for is waited out, a power cycle that saw
the machine off turns it on again. Then the operator undoes what the
operation left behind:

  - it ejects the virtual media the operation mounted,
  - it clears the boot override the operation set,
  - it makes the Kubernetes Node schedulable again if the operation
    cordoned it (a node someone had cordoned before stays cordoned).

What cannot be undone stays: a reboot, an applied config, an upgrade, an
etcd member that left. The operation ends as Aborted and its last step says
what was undone. Deleting an operation also stops it, but undoes nothing.

The reason is required. It is stored on the operation with your user name,
as "<username>: <reason>"; admission accepts no other form.`,
		Example: `  pk-metal abort --reason "wrong node" pk-metal/reboot-node-2-x7k2p`,
	}
	ns := cmd.Flags().StringP("namespace", "n", operatorNamespace, "namespace of the operation")
	reason := cmd.Flags().String("reason", "", "why, for the people who read the operation later (required)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return cobratree.Usagef("give exactly one operation")
		}
		namespace, name, err := splitRef(args[0], *ns, cmd.Flags().Changed("namespace"))
		if err != nil {
			return cobratree.Usage(err)
		}
		cl, err := newClient()
		if err != nil {
			die(err)
		}
		ctx, stop := signalContext()
		defer stop()
		user, err := whoAmI(ctx, cl)
		if err != nil {
			die(err)
		}
		if err := abort(ctx, cl, namespace, name, user, *reason, os.Stdout); err != nil {
			die(err)
		}
		return nil
	}
	return cmd
}

// abort marks the operation for abort and says what happens next.
func abort(ctx context.Context, cl client.Client, namespace, name, user, reason string, w io.Writer) error {
	op := &metalv1alpha1.HostOperation{}
	if err := getNamed(ctx, cl, "operation", op, &metalv1alpha1.HostOperationList{}, namespace, name); err != nil {
		return err
	}
	ref := op.Namespace + "/" + op.Name
	if finished(op) {
		return fmt.Errorf("operation %s has finished (%s); there is nothing to abort", ref, opPhase(op))
	}
	if by, asked := op.Annotations[metalv1alpha1.AnnotationAbort]; asked {
		fmt.Fprintf(w, "operation %s is already being aborted (%s)\n", ref, orDash(by))
		return nil
	}
	// Admission accepts an abort only as "<username>: <reason>", so that the
	// operation records who stopped it and why.
	if reason = strings.TrimSpace(reason); reason == "" {
		return fmt.Errorf("say why: pk-metal abort --reason \"<reason>\" %s", ref)
	}
	value := metalv1alpha1.GuardLiftValue(user, reason)
	patch := client.MergeFrom(op.DeepCopy())
	if op.Annotations == nil {
		op.Annotations = map[string]string{}
	}
	op.Annotations[metalv1alpha1.AnnotationAbort] = value
	if err := cl.Patch(ctx, op, patch); err != nil {
		return fmt.Errorf("mark %s for abort: %w", ref, err)
	}
	if op.Status.Baseline == nil {
		fmt.Fprintf(w, "asked to abort %s. It has not started, so it ends now and nothing is done to host %s.\n",
			ref, op.Spec.HostRef.Name)
		return nil
	}
	fmt.Fprintf(w, "asked to abort %s. It stops before its next step (a step that already acted on the machine "+
		"is finished first), then ejects its media, clears its boot override and uncordons the node it cordoned.\n", ref)
	fmt.Fprintf(w, "Follow it: pk-metal status, or kubectl get hostoperation -n %s %s\n", op.Namespace, op.Name)
	return nil
}
