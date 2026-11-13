# Proxmox placement mode (experimental)

Placement is an opt-in experimental path. Pin a tested node-driver release and
validate it against your Rancher and Proxmox environment before production use.

## Required setup

- Use a Rancher management cluster and a supported Proxmox cluster. Placement
  uses the management-cluster Kubernetes API; do not put its ledger in the
  downstream cluster being provisioned.
- The placement authorization controller grants each placement machine's
  ServiceAccount read-only access to CAPI `Machine` objects in its namespace.
  The controller itself needs cluster-scoped read access to CAPI Machines so it
  can delegate this narrower per-machine Role without general RBAC escalation.
  The driver matches `spec.infrastructureRef.name` to its machine name and reads
  the `cluster.x-k8s.io/cluster-name` and
  `rke.cattle.io/rke-machine-pool-name` labels. Missing, ambiguous, or denied
  metadata blocks provisioning. The controller revokes this grant after Rancher
  reports successful machine removal.
- Install the chart's placement CRD and authorization controller. The chart
  defaults to a reserve of 1 CPU and 1 GiB per host; adjust these positive
  shared CPU and memory reserves for your environment with
  `placement.capacityPolicy.default.cpu` and
  `placement.capacityPolicy.default.memoryBytes`. The first inventory request
  creates a ledger for the selected Proxmox endpoint using these defaults.
  Endpoint identity hashes the normalized hostname and port, not the credential
  or token. Each endpoint therefore has an independent ledger. Do not replace a
  ledger while it has active records.
- Give the Proxmox API token sufficient read access to the complete node, VM,
  container, storage, network, template, guest-agent, and cloud-init evidence
  required by discovery, plus permissions to clone, configure, start, stop,
  and delete managed VMs. Discovery fails closed when an endpoint is denied.
- Make a compatible host-local cloud-init template available on every allowed
  host. Templates need a configured boot order, a supported boot disk, a
  Proxmox cloud-init drive, guest agent, exactly one DHCP network interface with
  a MAC address on an existing bridge, and no custom Cloud-Init snippets. At
  least one active ISO-capable storage must be available on each eligible host;
  the driver selects it automatically. Guest networking uses DHCP-provided DNS
  and domain, and SSH uses the configured public key and Linux account. Guest
  account/sudo defaults must be verified by the operator; the Proxmox API cannot
  prove them.

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
- Ledger namespace, name, and cluster identity are derived automatically from
  the selected Proxmox endpoint and are not machine-pool inputs.
- Explicit processor sockets, processor cores, and memory are required.

The form's inventory display is advisory. Use the live provisioning-time
validation as authoritative; inaccessible or inconsistent inventory blocks
creation. Placement reservations do not constrain manual Proxmox operations,
external migrations, or other tools that bypass the ledger.

The form discovers online hosts, enabled/active image storages, and local
templates using the selected Proxmox credential. Select the allowed hosts first;
the storage choices then narrow to storages common to those hosts. Choose one
template per selected host. Discovery can be refreshed, but is advisory and
does not replace provisioning-time validation. The driver adds normalized
`rancher-cluster-*` and `rancher-pool-*` tags from CAPI metadata; it rejects
names that cannot be represented or collide with a different persisted name.
These tags are informational, not ownership proof.

New placement VMs receive a unique machine-incarnation identity in both the
placement ledger and VM description. Their unique NoCloud ISO supplies user
data, metadata, and DHCP network configuration, and stays attached until VM
deletion. The ledger records its name and ISO storage before upload. Uncertain
upload, attachment, ownership, host, or cleanup outcomes retain the ledger
reservation; the driver never overwrites an existing ISO.

## Recovery behavior

Reservations do not expire. A clone submission whose response or durable task
record is uncertain is not retried or automatically deleted. Keep the ledger
record and investigate the recorded VMID, machine name, task, and host before
operator recovery. A removal retains its reservation until the driver verifies
the VM is absent across the Proxmox cluster. Do not rename driver-managed VMs
directly in Proxmox.

There is no HA, continuous monitoring, rebalancing, automatic replacement, or
legacy VM adoption in this mode.
