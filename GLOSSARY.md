# Proxmox Machine Placement

Vocabulary for placing Rancher-managed Kubernetes machines on Proxmox physical hosts.

## Language

**Physical host**:
A Proxmox compute server on which a virtual machine resides.
_Avoid_: Kubernetes node, machine

**Machine incarnation**:
One distinct instance of a Rancher-managed machine, separate from any predecessor or replacement.
_Avoid_: VM name as identity

**Placement group**:
A set of machines explicitly identified for distribution and anti-affinity evaluation, independent of their Proxmox resource pool.

**Machine-pool spread**:
A placement preference that independently balances each machine pool's VM count across its eligible physical hosts. Colocation is permitted for both control-plane and worker machines, including temporary upgrade capacity.

**Placement evaluation**:
An event-triggered decision made for a provisioning request using the available placement inventory and pending commitments. It is not continuous monitoring or automatic rebalancing.

**Pool storage allowlist**:
An explicit selection of Proxmox storage IDs permitted on every allowed physical host in a machine pool. The same storage ID on different hosts does not imply shared underlying storage.

**Placement policy update**:
A deliberate change to a machine pool's allowed hosts, storages or host-local templates for future placements; it does not itself move or delete existing VMs.

**Host-local template mapping**:
An explicit association between each allowed physical host and the local template VMID used to provision machines on that host. The templates represent an administrator-maintained equivalent guest image.

**Provisioning-ready template**:
A host-local template whose guest defaults provide the SSH access, locked password and passwordless sudo required for machine provisioning.

**Boot disk**:
The first supported VM disk in a template's configured boot order. Its requested size may be expanded for a new machine; other attached disks retain their template sizes.

**Placement inventory**:
The complete set of VMs and pending placements relevant to a placement policy, including members in other resource pools.

**Discovered inventory**:
The Proxmox hosts, storages, templates and resource pools shown as advisory choices when configuring a machine pool. It is distinct from the placement inventory used to admit a machine.

**Allocated capacity**:
CPU and memory committed to existing VMs, including stopped VMs, and pending placements; distinct from current utilization.

**Host capacity reserve**:
CPU and memory kept available for a physical host rather than committed to guest allocations. A central policy supplies a default reserve and may specify host-specific overrides.

**Eligible destination**:
A physical host satisfying all required placement constraints, including compatibility with the requested template and clone operation.

**Placement-time anti-affinity**:
A separation constraint enforced when admitting a new VM placement, not a guarantee against subsequent external migrations.
_Avoid_: Continuous anti-affinity enforcement

**External migration**:
A VM move initiated outside the driver's placement operation.

**Replacement relationship**:
An explicit association between a new machine incarnation and the original incarnation it supersedes.
_Avoid_: Name-based replacement inference

**Unidentified replacement**:
A new machine intended to replace an original machine when their replacement relationship is not supplied to the driver. It is indistinguishable to the driver from an ordinary scale-up or surge machine.

**Ambiguous original state**:
A condition in which the original VM may still run because its deletion, stopped state, or external fencing has not been reliably established.
_Avoid_: Confirmed failure, safely stopped
