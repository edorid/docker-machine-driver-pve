# Design decisions and provisioning workflows

This document explains how the project evolved from its first commit to
`3b45391` (`v1.2.0-rc22`), and how the resulting design works. It is a
historical explanation and a guide to the current design, not a production
qualification or a record of every commit. Commit IDs identify useful points
to investigate; dates below follow the Git history.

**How to read the history:** some early documents describe proposals that were
subsequently implemented or changed. In particular, `architecture.md`,
`concurrency.md`, and `placement-ui-design.md` contain historical statements
about unfinished or opt-in features. Where those conflict with later code and
the [placement guide](placement.md), do not treat them as the current status.
Reasons described as trade-offs below are supported by the repository's
analysis and implementation; the history does not necessarily record the
original author's reasoning for every early feature.

## 1. The system in one picture

```text
Administrator / Rancher user
          |
          v
Rancher UI extension -- credential-scoped inventory --> inventory API
          |                                             |
          | machine pool configuration                  | Proxmox API
          v                                             v
Rancher Machine provision job --> PVE machine driver --> Proxmox cluster
                                      |                 (hosts, templates,
                                      |                  storage and VMs)
                                      v
                            Management-cluster ledger
                            (reservations and identity)
```

The **driver** implements Rancher Machine's VM lifecycle; it is not itself a
Kubernetes scheduler or a continuous VM management controller. Rancher decides
when to create, remove, scale, or replace machines. In placement mode the
driver selects a safe destination for a particular creation and coordinates
that choice with other driver processes through a Kubernetes ledger. The
authorizer also provides narrowly scoped inventory and permissions needed by
the integration. Helm installs the Rancher resources and supporting components.

Two paths coexist:

| | Existing/legacy configuration | Placement configuration |
| --- | --- | --- |
| Template | One explicitly selected template | Explicit host-to-template mapping |
| Destination | Existing clone behavior | Eligible host and storage selected using placement policy |
| Coordination | No placement ledger required | Durable shared ledger and capacity admission |
| Initialization | Existing cloud-init ISO behavior | Per-incarnation generated NoCloud ISO |
| Compatibility | Existing configurations remain usable | Additional requirements; experimental, validate before production |

Existing configurations are not automatically converted. The current Rancher
form defaults new pools to placement, while placement itself is a distinct path
with additional prerequisites.

## 2. Timeline of important decisions

| Period / commits | What changed | Design significance |
| --- | --- | --- |
| March 2025: `05a7c25`, `60d6c24` | Initial repository, Go driver and Rancher Machine integration; example Ubuntu template. | Use the Machine driver lifecycle and Proxmox templates rather than implement a separate Kubernetes infrastructure provider. |
| March–April 2025: `ead1470`, `4f23bfa`, `3426b42`, `4797772` | Helm chart, published UI extension, Rancher cloud credentials and machine configuration. | Separate installation, user-facing configuration, credential storage and VM operations while integrating them into Rancher. |
| April 2025: `ebbdc77`, `d5c436b`, `dc95e30` | Cloud-init timeout, cleanup on creation failure and hardware-device-based interface handling. | Treat guest readiness and cleanup as part of provisioning, and identify the network interface using VM hardware information. |
| July–December 2025: `4c9982a`, `b0382aa`, `7b16828`, `03727d4`, `475f1d4` | Hardware controls, ballooning, VMID conflict retry, optional full clone and VM tags. | Expose operational choices without replacing the original lifecycle. |
| November 2026: `6c8e035`, `a4bbfd0` | Placement coordination foundation followed by live driver integration. | Move from single-template placement to coordinated host-local selection and durable reservations, keeping the legacy path. |
| `8e3db29`–`aa4af0b` | Rancher metadata tags, credential-scoped inventory, simpler placement form, default-enabled inventory API and endpoint-derived ledger identity. | Reduce operator input while separating advisory discovery, authorization and durable coordination. |
| `6f1b751`–`2416b76` | Disk and inventory fixes, preservation of host reserves, full task UPID, and SSH-key handling fixes. | Make admission and recovery depend on accurate evidence and durable task identity. |
| `8749196`, `15aa3ec`, `5233b30` | Custom network snippet option was added, then generated provisioning and per-incarnation identity were introduced, and custom snippets were removed. | Current placement uses a generated DHCP NoCloud configuration and identity-backed ISO lifecycle, not the earlier custom-snippet option. |
| `3b45391` | Prefer VM disk storage for the ISO when active and ISO-capable. | Keep initialization media with the disk when possible, otherwise choose an eligible fallback deterministically. |

The implementation and its earlier source-level investigation are available in
[upstream analysis](upstream-analysis.md); that analysis describes its older
`9e561c4` baseline, not the current integration.

## 3. Foundational decisions

### Rancher Machine and template-based VMs

Rancher calls the driver as part of machine provisioning. The driver uses the
Proxmox API to clone a template, configure and start the VM, discover an
address, and wait for guest initialization over SSH. This reuses Rancher's
existing machine-pool lifecycle and an operator-prepared guest image. It also
means success depends on the template, networking, guest agent, cloud-init, SSH
and sudo—not just a successful Proxmox clone.

The Helm chart registers a `NodeDriver` and `UIPlugin`; the release pipeline
publishes a driver binary, checksum, chart and UI assets. The placement
authorizer is additionally delivered as a container image. Keeping artifacts
versioned together supports a consistent installation; air-gapped operators
can host the relevant artifacts themselves. See the [installation guide](installing-and-creating-clusters.md).

### Keep credentials separate from machine configuration

The Rancher cloud credential holds the Proxmox token. Machine-pool configuration
describes the desired machines and placement, rather than being a place to copy
token secrets. TLS verification is normally enabled; explicitly disabling it
is an operator security trade-off, not a recommended default.

For placement inventory, the UI asks the controller for inventory scoped to a
selected credential. The aggregated API authenticates the forwarded user using
the Kubernetes request-header trust configuration and checks authorization for
the specific credential secret before reading it. Outbound Proxmox hosts must
also be allowed. This avoids turning the controller into an unrestricted
credential reader or arbitrary proxy. Inventory responses are advisory: the
driver independently validates current evidence when provisioning. See
[ADR 0001](adr/0001-credential-scoped-inventory.md) and the current installation
guide for the Rancher version/security prerequisites; the ADR's original
opt-in gate predates the later default-enabled implementation.

## 4. Placement: what problem it solves

The original driver cloned from one configured template. That alone did not
coordinate parallel creations, account for reservations, or select among
host-local templates and storage. Proxmox's next-available VMID is a lookup, not
an atomic reservation; another creator can claim the ID before the clone.
Rancher Machine also does not checkpoint every external side effect during
`Create`.

Placement therefore adds a **shared management-cluster ledger** and a pure
selection algorithm. It deliberately does not claim to control manual Proxmox
operations, other tools, migrations, or Rancher's cluster lifecycle.

### Host-local full clone

The operator supplies one compatible template per eligible host and allowed
disk-storage IDs. Placement selects a template on the chosen host and uses a
full clone to the chosen storage on that host. A similarly named local storage
on another node is not the same shared storage. The source analysis found that
arbitrary remote-local cloning cannot be assumed and that linked clones retain
backing-volume dependencies and cannot provide the same destination-storage
choice. The trade-off is that operators must prepare compatible templates on
each host; the design does not automatically distribute templates or perform
clone-then-migrate.

### Spread without strict anti-affinity

Selection first excludes ineligible destinations, then prefers the host with
the fewest VMs and pending reservations for the same placement group. Ties use
host name. On that host it prefers an allowed disk storage with the greatest
remaining capacity, breaking ties by storage ID. Counts for spreading are
pool-specific, while admission considers broader allocations.

Example: three eligible hosts with counts `1, 1, 1` can accept a surge VM and
become `2, 1, 1`. Strict anti-affinity would block that surge. The trade-off is
that placement can colocate machines; it is not a high-availability guarantee,
and it does not later rebalance them. See [placement strategies](placement-strategies.md),
noting that its statement that the driver is not wired up is historical.

### Conservative capacity admission

The scheduler works with resource **commitments**, not simply current usage:

- CPU accounts for allocated vCPUs against host CPU capacity and reserve.
- Memory accounts for configured maximum memory, not just the balloon target,
  and also checks actual free memory and the host reserve.
- Storage accounts for full virtual allocations and physical free space,
  including pending commitments, rather than assuming unused thin-provisioned
  bytes are permanently available.
- Existing VMs and containers, including stopped guests, matter to admission;
  an already observed owned VM must not be counted twice as a reservation.

Defaults and optional host overrides live in the ledger's capacity policy.
Incomplete or inconsistent evidence fails closed. This is safer than admitting
from an optimistic partial view, but can reject a request that might have fit.
The provisioning token needs sufficient permissions for the required evidence.

## 5. Workflow: from a Rancher pool to a running VM

1. **Configure the pool.** Select a credential, allowed hosts and common disk
   storage IDs, map each host to a compatible local template, and supply CPU,
   memory and disk requirements. UI discovery helps with these selections but
   is not authoritative at clone time.
2. **Start a Machine job.** Rancher starts the driver for an individual machine.
   Placement obtains cluster and pool metadata from the matching CAPI Machine;
   absent, ambiguous or unauthorized metadata blocks provisioning.
3. **Establish the coordination domain.** The ledger identity is derived from
   the normalized Proxmox endpoint host and port, not the token. Use one
   canonical endpoint per Proxmox cluster: different aliases for the same
   cluster could otherwise produce independent ledgers and lose coordination.
4. **Discover and select.** Read the ledger and current Proxmox evidence, check
   policy and capacity, choose a host and disk storage, then choose ISO storage.
5. **Reserve atomically.** Record the machine intent, VMID, destination,
   resource commitments, incarnation and ISO identity using Kubernetes
   `resourceVersion` compare-and-swap. On a revision conflict the coordinator
   re-reads the ledger and rediscovers before selecting again.
6. **Revalidate and claim submission.** Before the clone, validate the reserved
   destination again. Only the caller that successfully claims submission may
   submit it; the clone uses the selected host-local template, storage and VMID.
7. **Record the task and initialize.** Persist the returned Proxmox task UPID,
   configure ownership and hardware, create and attach the unique NoCloud ISO,
   start the VM, and wait for guest address and SSH/cloud-init readiness. Rancher
   then continues Kubernetes bootstrap and health checks.

A created or running Proxmox VM is therefore not by itself proof that the
Rancher cluster is Ready. The VM must boot, acquire networking, run guest
initialization, and complete Rancher's Kubernetes bootstrap.

## 6. Workflow: reservation, concurrency and failures

```text
             successful CAS               claim              task recorded
new intent ----------------------> reserved -------> submitting ----------> submitted
                                      |                  |                      |
                                      |                  +-- uncertain --------+--> investigate
                                      +-------------------------------------------> removing
                                                                                       |
                                                                        verified cleanup/absence
                                                                                       v
                                                                                release record
```

This diagram summarizes the principal phases, not every possible transition.
The critical boundary is between Kubernetes and Proxmox: they do **not** share
one transaction. A successful reservation does not mean a VM exists, and a
timeout while submitting a clone does not prove that Proxmox rejected it.

- Replaying the same intent can recover its existing reservation; a conflicting
  identity, VMID, resources or group policy is rejected.
- Only one caller gets a submission claim. The claim is not a fencing token
  enforced by Proxmox and cannot make arbitrary external operations exactly once.
- Reservations do not expire simply because time passed. An uncertain clone,
  task, ownership or cleanup outcome retains the record for investigation.
- During removal, identity and absence must be verified before releasing the
  reservation. Do not manually delete ledger records merely to unblock capacity.
- A competing external Proxmox creator can still race for a VMID. Definitive
  collision handling and Rancher retries are not a guarantee that all ambiguous
  failures can safely be retried.

The ledger is bounded (currently 256 records and 512 KiB serialized ledger),
which is an implementation safety limit, not a demonstrated production scale.
Restores and recreated ledgers need reconciliation against real Proxmox state;
see [placement recovery](placement.md#recovery-behavior) and the historical
[concurrency design](concurrency.md).

## 7. Workflow: Cloud-Init ISO and machine identity

Placement generates a separate NoCloud ISO for each machine incarnation. The
layout is similar across VMs, but metadata, hostname and SSH provisioning data
are machine-specific. Network configuration requests DHCP; it does not hardcode
DNS servers or search domains. The current design does not support user-supplied
placement network snippets.

```text
new machine incarnation
  -> reserve incarnation ID + unique ISO name + selected ISO storage
  -> clone and configure VM; mark ownership in VM metadata
  -> generate user-data, meta-data and DHCP network configuration
  -> upload and attach unique ISO (never overwrite an existing ISO)
  -> boot, guest agent / DHCP, SSH, cloud-init and Rancher bootstrap
  -> retain ISO until identity-checked VM removal and cleanup
```

The incarnation ID distinguishes one particular creation from a replacement
that happens to reuse a machine name or VMID. It connects the ledger record,
VM description marker, Cloud-Init instance identity and unique ISO name.
Human-readable Rancher cluster and pool tags help operators identify resources,
but are **not** proof that deletion is safe. Conflicting or uncertain identity
blocks destructive cleanup. See [ADR 0002](adr/0002-placement-machine-identity.md).

### ISO storage selection

Disk storage and ISO storage have different content capabilities. On the
selected host, placement first checks whether the selected disk storage is
active and supports ISO content. If so it uses that storage. Otherwise it picks
the lexically first active ISO-capable storage on that host. If none exists,
placement fails instead of assuming `local` or using storage on another host.
The chosen storage and ISO name are reserved before upload, and the same
selection rule is used during reservation revalidation.

For example, a VM disk on `local-vm1` uses `local-vm1` for its ISO if it is
active and supports ISOs; otherwise an eligible `local` may be selected. This
choice applies to new placement reservations; it does not move existing ISOs
or change the legacy driver's behavior. The ISO remains attached until removal,
which makes its storage availability an ongoing operational consideration.

## 8. Intentional boundaries and trade-offs

- **Safety over automatic recovery:** ambiguous external side effects can
  require operator investigation and retain capacity rather than risk duplicate
  clones or deleting the wrong VM.
- **Admission, not continuous scheduling:** there is no automatic rebalance,
  HA controller, etcd management, or automatic replacement in placement mode.
- **Cooperation has limits:** the ledger coordinates participating drivers;
  it cannot prevent manual changes or other tools from consuming capacity.
- **Templates are a contract:** the operator must provide compatible local
  templates and verify guest SSH/sudo behavior; API discovery cannot prove every
  guest property. See [placement requirements](placement.md#required-setup).
- **Advisory UI, authoritative admission:** inventory can become stale between
  form selection and creation. Provisioning performs its own checks.
- **Identity over names:** tags and VM names are useful for humans, while
  incarnation-backed ownership and conservative verification protect cleanup.
- **Experimental placement:** a successful test or release does not establish
  support for every Rancher, Proxmox, storage or template combination.

## 9. Where to look in the repository

| Topic | Source |
| --- | --- |
| Driver lifecycle and placement integration | `cmd/docker-machine-driver-pve/driver/driver.go`, `placement.go` |
| Discovery and template validation | `cmd/docker-machine-driver-pve/driver/placement_discovery.go` |
| Placement ISO and cloud-init | `cmd/docker-machine-driver-pve/driver/placement_iso.go`, `placement_cloud_init.go` |
| Selection, ledger and Kubernetes CAS | `internal/placement/scheduler.go`, `coordinator.go`, `kubernetes.go` |
| Credential authorization and inventory | `internal/authorization/` and [ADR 0001](adr/0001-credential-scoped-inventory.md) |
| UI and installation | `pkg/pve-node-driver/`, `deploy/charts/pve-node-driver/` |
| Terminology and operational guidance | [Glossary](../GLOSSARY.md), [placement guide](placement.md), [installation guide](installing-and-creating-clusters.md) |

For exact behavior, use the source and tests at the commit being deployed;
this document is a snapshot through `3b45391`.
