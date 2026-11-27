# The virtual lab

Three virtual machines with simulated management controllers, and the operator
taking them from an inventory entry to a three-node Kubernetes cluster. The
machines are real UEFI machines under QEMU, the install is a real Talos install,
and the operator runs from the same image and manifests a real site would use.
Only the hardware is simulated.

All the documentation of the lab is on the
[documentation site](https://metal.platform-kit.org/reference/lab/). Start with
the tutorial [Your first cluster in the lab](https://metal.platform-kit.org/learn/lab-first-cluster/).
New to Kubernetes, or coming from PXE, kickstart, Foreman or xCAT? Read
[pk-metal for system administrators](https://metal.platform-kit.org/start/for-sysadmins/)
first. It maps the objects here to the tools you know, says where each piece
of state lives, and lists what is destructive.

## Before you start: what changes a machine

- **A HostClaim with `spec.os` is an install order.** The operator proposes
  to boot that machine into the Talos installer. Once a person approves, the
  install disk is repartitioned and overwritten. The lab's
  `.lab/generated/manifests/30-claims.yaml` has one such claim per machine.
- **Nothing happens to a machine before somebody approves.** The approval is
  `pk-metal approve` (the key `a` in `mise run lab:top`). To say no, remove
  `spec.os` from the claim, or hold it: `pk-metal hold default/node-1`.
- **Deleting a HostClaim removes its node and resets its machine**: the node
  is drained and leaves the cluster, Talos wipes its system disk, and the
  machine is powered off. For a machine that runs its node, that waits for an
  approval too. `kubectl delete -f .lab/generated/manifests/` does this to
  every lab machine. It is not "remove from the inventory". Adding, removing
  and replacing nodes: [Add and remove nodes](https://metal.platform-kit.org/workflows/add-and-remove-nodes/).

## The round

```sh
mise trust            # once per clone: let mise read mise.toml
mise install
mise run lab:doctor   # checks the workstation and prints the fix for each miss
mise run lab:up
mise run lab:top      # a shows the plan of an install, y approves it
mise run lab:kubeconfig
mise run lab:down && mise run lab:reset
```

## Reference

- [Virtual lab](https://metal.platform-kit.org/reference/lab/): what runs where
- [Lab prerequisites](https://metal.platform-kit.org/reference/lab/prerequisites/)
- [lab.yaml](https://metal.platform-kit.org/reference/lab/lab-yaml/): the size and the spec of the lab
- [Lab tasks](https://metal.platform-kit.org/reference/lab/tasks/): each `mise run lab:*` task
- [Simulated BMC](https://metal.platform-kit.org/reference/lab/simulated-bmc/)
- [Lab network](https://metal.platform-kit.org/reference/lab/network/), also behind a proxy
- [What runs as root](https://metal.platform-kit.org/reference/lab/what-runs-as-root/)
- [lab-e2e workflow](https://metal.platform-kit.org/reference/lab/lab-e2e/)
- [Lab troubleshooting](https://metal.platform-kit.org/reference/lab/troubleshooting/)

## Tutorials

- [Your first cluster in the lab](https://metal.platform-kit.org/learn/lab-first-cluster/)
- [Day 2 in the lab](https://metal.platform-kit.org/learn/lab-day-2/)
- [Scaling in the lab](https://metal.platform-kit.org/learn/lab-scaling/)
- [Disaster recovery in the lab](https://metal.platform-kit.org/learn/lab-disaster-recovery/)
- [Pivot in the lab](https://metal.platform-kit.org/learn/lab-pivot/)
