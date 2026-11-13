# Install the Proxmox VE node driver and create a cluster

Install the node driver in Rancher's **management (local) cluster before**
creating a downstream cluster with Proxmox machine pools. The chart installs the
Rancher `NodeDriver`, UI extension, placement ledger resources, and placement
authorization controller.

## Requirements

- A Rancher management cluster running Rancher `>= 2.15.3` and UI Extensions
  `>= 3.0, < 4.0`. The chart enables its credential-scoped inventory API by
  default; Rancher 2.15.3 or newer is required for the supported proxy/auth
  behavior.
- Network access from Rancher and users' browsers to the Proxmox API, and from
  users' browsers to the published UI extension assets.
- A Proxmox API token and a compatible Linux cloud-init template on each
  Proxmox host that will be available for placement. Templates must have a
  usable boot disk, cloud-init, a guest agent, and DHCP networking. Verify that
  the configured SSH user can connect and use passwordless `sudo`.
- For placement, configure shared CPU and memory reserves in the Helm values.
  Each endpoint gets a separate ledger, initialized with those defaults on the
  first inventory request. The endpoint identity is a SHA-256 fingerprint of
  the normalized Proxmox hostname and port; tokens and Rancher credential IDs
  are not part of it. Use one canonical endpoint per Proxmox cluster. Do not
  replace or delete a ledger while it contains active machine records.

## Install from the Rancher UI

Do this in the Rancher **local** cluster:

1. Open **Apps > Repositories > Create** and add the chart repository:
   - Name: `pve-node-driver`
   - Index URL: <https://edorid.github.io/docker-machine-driver-pve>
   - Authentication: none
2. Open **Extensions > Available**, find **Proxmox VE Node Driver**, and install
    the release you intend to use. Pin a tested version; `1.2.0-rc12` is a
    pre-release, not a stable release.
3. Confirm that the `pve` NodeDriver is active and the Proxmox VE extension is
   ready before starting cluster creation. The chart enables the placement
   inventory API by default; it authorizes access to the selected cloud
   credential and only connects to Proxmox hosts allowed by the NodeDriver
   whitelist.

Install or upgrade the chart as the same Rancher release (`pve-node-driver`).
Do not install a second release of the chart under another Helm release name.
There must be only one UI extension registered with plugin name
`pve-node-driver` in a Rancher instance; a test UIPlugin with the same plugin
name can make Rancher load the wrong version.

## Install with Helm

Run Helm against the Rancher management cluster, not the downstream cluster
being created. Replace the version below with the desired published release:

```sh
export PVE_NODE_DRIVER_VERSION=1.2.0-rc12
: "${PVE_RESERVED_HOST_CPUS:?Set the CPU reserve for Proxmox host services}"
: "${PVE_RESERVED_HOST_MEMORY_BYTES:?Set the memory reserve in bytes}"

helm repo add pve-node-driver https://edorid.github.io/docker-machine-driver-pve
helm repo update
helm upgrade --install pve-node-driver \
  pve-node-driver/pve-node-driver \
  --version "${PVE_NODE_DRIVER_VERSION}" \
  --namespace cattle-ui-plugin-system \
  --create-namespace \
  --set placement.capacityPolicy.default.cpu="${PVE_RESERVED_HOST_CPUS}" \
  --set placement.capacityPolicy.default.memoryBytes="${PVE_RESERVED_HOST_MEMORY_BYTES}" \
  --wait
```

Choose reserves appropriate for your Proxmox hosts. These shared defaults seed
each endpoint's ledger; do not set them to zero. Optional per-host overrides
can be applied to an individual ledger's `spec.capacityPolicy.hosts`.

The chart enables `placement.inventoryAPI.enabled` by default. To explicitly
disable inventory discovery, set `--set placement.inventoryAPI.enabled=false`.

For an existing Helm installation with custom values, use Helm 3.14 or newer
and add `--reset-then-reuse-values` to preserve the release's supplied values
while incorporating defaults added by newer chart versions. Keep the release
name and namespace unchanged when upgrading.

Check the installation:

```sh
helm -n cattle-ui-plugin-system status pve-node-driver
kubectl -n cattle-ui-plugin-system get nodedriver.management.cattle.io pve
kubectl -n cattle-ui-plugin-system get uiplugins.catalog.cattle.io
```

The UIPlugin should report `READY=true` and use the same version as the Helm
release. If Rancher shows raw fields such as `placementGroup` instead of the
Proxmox form, inspect the browser console and ensure there is not a second
UIPlugin with `spec.plugin.name: pve-node-driver` registered at another
version. Hard-refresh after correcting the registration.

## Create a Proxmox cluster

1. Start **Create** from Rancher's cluster management view and choose the
   applicable RKE2/Kubernetes cluster type.
2. Add a machine pool and choose **Proxmox VE** as its infrastructure provider.
   Create or select a Proxmox cloud credential. Keep the API token secret in the
   Rancher credential; do not place Proxmox secrets in machine-pool fields. When
   testing a new credential, the form checks whether its Proxmox hostname is
   allowlisted. If it is not, click **Add host to allowlist and retry**; this
   updates the `pve` NodeDriver and requires permission to edit that resource.
   If you do not have that permission or the prompt is unavailable, ask a Rancher
   administrator to add the hostname under **Cluster Management > Drivers >
   Node Drivers > Pve > Edit config > Whitelist Domains**.
3. Select the Proxmox resource pool and configure pool name and machine count.
   New pools use safe placement by default. Existing legacy machine
   configurations are not automatically converted.
4. Wait for inventory discovery. Select the allowed Proxmox hosts and their
   common image storages, then choose a compatible host-local template for each
   selected host.
5. Configure processor cores, memory, and boot disk size. Full cloning is on by
   default. Memory ballooning is off by default and can be enabled next to the
   Memory field if desired.
6. Confirm that the selected guest templates support SSH and passwordless
   `sudo` for the configured user (default `user`). Fill in any required VM
   tags, then save the machine pool and create the cluster.

The endpoint-specific ledger is created automatically in `fleet-default` when
inventory is first requested. Its identity is hidden from the machine-pool form
and remains stable across Rancher credential recreation, provided the
normalized Proxmox endpoint remains the same.
Provisioning fails closed if inventory, templates, ledger capacity policy, or
guest-readiness confirmation is incomplete.
