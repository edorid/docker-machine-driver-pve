# Agreed implementation plan

## Status

The revised design below supersedes the original mandatory
anti-affinity and multi-strategy requirements for the first implementation.
Items below are milestones, not a statement that functionality is released.

Current increment: placement core, Kubernetes ledger transport, initial
authorization-controller/Helm integration, Proxmox inventory adapter, clone and
removal paths, native cloud-init, and an initial Rancher form are present locally.
Go race tests and vet pass. The Rancher form's discovery is advisory and its
placement selections are revalidated by the driver; it does not prove template
guest compatibility in the browser. Lint, ESLint, UI production build, Helm lint
and rendering, race tests, and vet pass locally. Controller HTTP integration and
live Proxmox/Rancher qualification have not been run. This is not ready to publish.

## Agreed placement contract

- Each Rancher machine pool has explicit allowed hosts, common allowed storage
  IDs, and a stable placement-group identity. Use Rancher identity only if its
  availability and stability are verified; otherwise require an explicit group.
- The UI discovers hosts, storage and local templates. Offer storage IDs suitable
  on all selected hosts and a local template selection per host. Preserve existing
  choices when discovery is incomplete, visibly mark offline hosts, and block
  changes that cannot be validated. Provisioning independently rechecks evidence.
- Balance **that pool's** VM count, including pending placements. Other pools
  influence resource admission, not its spread count. Colocation is permitted
  for workers and control plane, including surge upgrades.
- Filter host/storage combinations by health, complete resource evidence,
  template/network compatibility and capacity. Pick the host with the smallest
  group count; ties use lexical host name. On that host choose greatest remaining
  storage bytes after commitments and request; ties use lexical storage ID.
- Storage identity is `(physical host, storage ID)`. Matching IDs across hosts do
  not imply shared storage. Never leave the allowlists.
- Administrators maintain equivalent templates on every allowed host, with
  explicit host-to-VMID mapping. Use same-node full clone to selected storage.
  Template distribution and clone-then-migrate are outside initial scope.
- Use native Proxmox cloud-init on selected storage, subject to verified Rocky
  SSH/sudo/hostname/DHCP compatibility. Administrators attest to template guest
  defaults, including passwordless sudo and a locked password; API-visible checks
  alone cannot prove them. Preserve legacy generated ISO mode.
- Require explicit pool CPU and memory values. Networking is inherited from the
  templates; DHCP and guest-agent address discovery are required initially.
- A pool specifies desired boot-disk size. Resize the first supported VM disk in
  the template's configured boot order before first boot; reject ambiguous boot
  order or a requested size smaller than any mapped template's boot disk. Other
  disks remain unchanged. Reserve all final virtual disk sizes before cloning.
- No default CPU/memory/storage overcommit. Count stopped VM allocations, all
  cloned disks, cloud-init/auxiliary disk capacity and pending reservations.
- Require complete inventory across the placement domain, not just the
  destination Proxmox resource pool. Reject unknown capacity or compatibility.
- Placement evaluates on provisioning requests, not continuously. Refresh tracked
  VM locations on each request. Stopped VMs count until deletion is verified.
  Offline hosts remain configured but are not eligible. Pool-policy updates apply
  to future machines, do not drain hosts, and wait for unresolved operations.

## Coordination and lifecycle contract

- Use Rancher's management Kubernetes API, not direct etcd access or a service
  in the downstream cluster being provisioned.
- One bounded ledger per Proxmox cluster with optimistic-concurrency updates.
  Keep VMID claims, operation identity, reservations and ownership together.
  Missing/unavailable coordinator, exhausted limits or ambiguous updates block
  admission. The numeric bound must be measured; no untested scale guarantee.
- Each cluster has an administrator-assigned stable identity and a central host
  CPU/memory reserve policy shared by all its pools. The installed initial policy
  has no reserve values; administrators must configure a default or applicable
  per-host overrides. Missing policy blocks advanced provisioning. Reserve
  increases apply to future admission and never resize or stop existing machines.
- The reserve policy and ledger are intended to share one atomic Kubernetes
  resource. Policy updates preserve active records and invalidate stale admission
  decisions before clone submission. Administrators must not replace the whole
  object during routine updates. Field-level protection is not provided by RBAC;
  accepted first-release trust is whole-object access for authorized jobs.
- Persist intent before any Proxmox side effect. Exactly one caller can claim
  submission. Response loss never authorizes another clone.
- Reservations do not become reusable solely because a process/lease times out.
  Unknown tasks remain accounted for until safe reconciliation or documented
  operator resolution. No permanent recovery controller initially.
- Use the Rancher-generated machine name and recorded VMID for lookup, under the
  operator's rule that administrators do not create VMs using Rancher naming
  patterns. Fail closed on conflicting or unresolved records. This is weaker than
  unique incarnation ownership proof if names and VMIDs are reused; do not claim
  otherwise. Track migration without assuming the original host.
- Do not automatically adopt or delete a VM after an ambiguous clone when its
  relationship to the request cannot be established; require operator recovery.
  A driver ownership tag is added before cloud-init and is required for any
  placement-mode deletion; it does not authorize adoption without a ledger record.
- Rancher owns replacement and upgrade decisions. Without reliable original
  linkage and safe original-state evidence, automatic safe replacement is
  unsupported; do not infer it or implement fencing/etcd recovery. Ordinary
  scale-up and surge requests do not require a predecessor. When Rancher does
  not identify a replacement, operators must establish original VM safety before
  permitting it; the driver cannot recognize or enforce that prerequisite.
- External migration must be excluded during placement. Do not claim that
  driver reservations constrain administrator or other external actions.
- Administrators must not rename Rancher-created VMs directly in Proxmox. An
  unexpected name change blocks lifecycle actions until operator recovery.
- Coordinator restore requires a blocked reconciliation procedure before writes
  resume; restored history cannot prove that newer Proxmox actions never ran.

## Rancher installation and authorization

- Work with an unmodified Rancher installation. The Helm chart may install a
  small authorization controller in addition to the driver and UI, but no
  background Proxmox monitoring, placement or recovery controller.
- Rancher 2.15.2 normally places node-driver provisioning Jobs and their
  per-machine ServiceAccounts in `fleet-default` on the management cluster;
  this is a default, not a universal namespace guarantee. The chart should work
  automatically there and support other valid provisioning namespaces.
- The authorization controller lists Rancher Jobs cluster-wide and stores each
  machine ServiceAccount's ledger association in a managed RoleBinding. Ledgers
  may share a namespace: the controller creates a per-ledger namespaced Role
  constrained by `resourceNames`, then binds only that ServiceAccount. The
  controller itself has cluster-wide Job/RoleBinding reads and can create Roles
  and RoleBindings, plus get/update ledger permissions. It must be treated as a
  trusted authorization component. Successful remove Jobs revoke bindings;
  failed/ambiguous removes retain them. Create/remove classification and the
  polling implementation still require Kubernetes HTTP and Rancher integration
  tests, including response-loss behavior and Rancher upgrades.
- Rancher 2.15.2 does not explicitly pass pool UID or machine UID to the driver.
  The derived machine name is stable for retries of an existing infrastructure
  object but is not guaranteed unique forever. The first release relies on name,
  recorded VMID, durable history and the operator naming rule; block conflicting
  cases. Pool identity and safe delete-path behavior remain integration gates.
- An authorization controller may be unavailable without affecting existing
  VMs; new provisioning requiring a missing binding fails closed.

## Milestones

1. **Audit:** source/API findings and compatibility risks in
   [upstream-analysis.md](upstream-analysis.md), before major implementation.
2. **Placement core:** validated policy, deterministic spread, explicit storage
   filtering, resource accounting, diagnostic exclusions and unit tests.
3. **Coordination core:** Kubernetes CAS transport and bounded durable state,
   replay protection, operation transitions, concurrency and HTTP failure tests.
4. **Discovery and lifecycle integration:** live evidence adapter, explicit VMID
   clones, ownership metadata, cloud-init, recoverable Create/Remove and migration
   tests. Do not advertise advanced provisioning before this is complete.
5. **Rancher integration:** secure coordinator access from actual provisioner
   processes, scoped authorization automation if needed, CLI/environment/
   persistence and UI options, CRD/RBAC installation.
6. **Qualification:** unit/race/integration/format/static/dependency gates,
   isolated end-to-end matrix, Linux artifacts, checksums and release notes.

Each increment records actual tests and limitations; incomplete behavior stays
disabled rather than falling back to unsafe provisioning.

## Validation target

User-selected target: Proxmox VE **9.2.26**, Rancher **2.15.2**, latest RKE2 offered
and supported by that Rancher release, Rocky Linux **9 x86_64** guests.
Record exact Proxmox package versions (including the meaning of 9.2.26), Rancher
build, resolved RKE2 version, Rocky minor release and template checksum before
real validation. Do not let mutable "latest" change during testing.
This is a requested target, not a certified or end-to-end-validated combination.

## Deferred

Strict anti-affinity, rack/failure-domain policy, weighted resource strategies,
round robin, alternate coordination backends, template transfer/replication,
automatic rebalancing and a permanent recovery controller. Metrics only if they
fit without introducing a permanent service. No Proxmox HA dependency.
