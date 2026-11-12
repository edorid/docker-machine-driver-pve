# Proxmox placement mode (experimental)

Placement is an opt-in path and is not part of the published `v1.2.0-rc4`
release. The local implementation is not qualified for production or testing
until the release gates in [implementation-plan.md](implementation-plan.md) pass.

## Required setup

- Use a Rancher management cluster and a supported Proxmox cluster. Placement
  uses the management-cluster Kubernetes API; do not put its ledger in the
  downstream cluster being provisioned.
- Install the chart's placement CRD, ledger, and authorization controller. Set
  the ledger's administrator-assigned `clusterId` to the exact value supplied
  to the machine configuration. Do not replace the ledger object while it has
  active records.
- Configure `spec.capacityPolicy.configured: true` on the ledger and set the
  central default CPU and memory reserves (and optional per-host overrides).
  An unset reserve policy blocks placement.
- Give the Proxmox API token sufficient read access to the complete node, VM,
  container, storage, network, template, guest-agent, and cloud-init evidence
  required by discovery, plus permissions to clone, configure, start, stop,
  and delete managed VMs. Discovery fails closed when an endpoint is denied.
- Make a compatible host-local cloud-init template available on every allowed
  host. Templates need a configured boot order, a supported boot disk, a
  Proxmox cloud-init drive, guest agent, and the selected network interface on
  an existing bridge. Guest networking uses DHCP and SSH uses the configured
  public key and Linux account. Guest account/sudo defaults must be verified by
  the operator; the Proxmox API cannot prove them.

## Machine configuration

Enable placement in the Rancher machine configuration and set:

- **Placement group identity**: stable for machines in one Rancher pool. It is
  used for that pool's VM-count balancing.
- **Allowed hosts**: comma-separated Proxmox node names.
- **Allowed storages**: comma-separated storage IDs, available as host-local
  storage on the selected hosts.
- **Host-local templates**: comma-separated `host=VMID` entries.
- **Boot disk size (GiB)**: requested final size. It must be at least the
  configured boot disk size of every eligible mapped template.
- **Ledger namespace**, **ledger name**, and **Proxmox cluster identity**: must
  identify the installed ledger.
- Explicit processor sockets, processor cores, and memory are required.

The form's inventory display is advisory. Use the live provisioning-time
validation as authoritative; inaccessible or inconsistent inventory blocks
creation. Placement reservations do not constrain manual Proxmox operations,
external migrations, or other tools that bypass the ledger.

## Recovery behavior

Reservations do not expire. A clone submission whose response or durable task
record is uncertain is not retried or automatically deleted. Keep the ledger
record and investigate the recorded VMID, machine name, task, and host before
operator recovery. A removal retains its reservation until the driver verifies
the VM is absent across the Proxmox cluster. Do not rename driver-managed VMs
directly in Proxmox.

There is no HA, continuous monitoring, rebalancing, automatic replacement, or
legacy VM adoption in this mode.
