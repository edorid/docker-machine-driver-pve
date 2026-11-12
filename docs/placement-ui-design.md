# Placement UI design handoff

This document records agreed design decisions; implementation is in progress
and deployment remains pending. See [the inventory security decision](adr/0001-credential-scoped-inventory.md).

## Security integration gate

- Prove how the management-cluster Rancher service proxy authenticates the
  requesting user to the controller and how to authorize use of the specific
  selected cloud credential. Do not trust client-supplied identity headers or
  assume service-proxy permission authorizes credential use.
- Investigate Rancher's credential-substitution mechanisms as an alternative,
  but do not assume they support this Proxmox credential format. No secret or
  raw credential-bearing request may be exposed in the UI, logs, or errors.
- The current `pve.vue` reads credential secrets into browser state and calls
  `/meta/proxy`; this is the existing behavior to replace for placement UI.
  Honor explicit insecure TLS per credential with a warning; otherwise verify
  TLS. Protect against arbitrary targets and redirects.
- Until the identity and authorization path is verified, fail closed. Do not
  implement a privileged service that reads arbitrary credentials on behalf of
  an unauthenticated caller.
- The aggregated inventory API is opt-in until ordinary Rancher-user allow and
  deny behavior has been tested end to end. Enabling it requires cert-manager;
  outbound Proxmox hosts must be explicitly approved in the PVE node driver's
  `whitelistDomains` (or the operator's `placement.inventoryAPI.allowedProxmoxHosts`
  baseline). The UI may offer an explicit, permission-gated add-host action;
  redirects are disabled and unlisted hosts are rejected.

## Inventory and form

- Automatically load on credential selection/change and on reopening the form;
  no permanent Discover button or continuous polling. Show loading, empty,
  partial failure and sanitized actionable error states; allow error retry.
- Offer checklists of physical hosts and eligible host-local image storages.
  The selected storage IDs must be available on every selected host, although
  storage with the same ID is physically separate on each host. No shared
  storage or per-host destination storage mapping in this iteration.
- Require an explicit template selection per host. Show its host, VMID, name,
  all disk storage locations, boot disk, inferred interface, cloud-init status,
  and compatibility reasons. Do not infer equivalence from names. Block
  templates with shared or unidentifiable required disks, missing cloud-init,
  unsupported networking or insufficient boot disk size; warn on differences
  that do not prove incompatibility.
- Keep stale selections visible as unavailable; do not silently remove or
  substitute them. Block saving new or changed placement choices until missing
  evidence or incompatibility is resolved. Retain choices on discovery failure.
- The Proxmox resource pool is an explicit automatically populated selection,
  separate from the Rancher machine pool and placement group. Template
  membership in that pool is not required.
- Show central ledger identity and reserves read-only when authorized. Require
  operator confirmation of template guest SSH and passwordless sudo readiness;
  invalidate that confirmation when templates or relevant evidence change.

## Placement behavior

- Preserve template network devices, bridges and cloud-init network settings.
  Use `net0` for the connection when present, otherwise the sole interface;
  reject ambiguity. Do not silently configure DHCP.
- Reject static or unverifiable addressing; guest-only DHCP with no Proxmox
  evidence is not enough. Preserve all template interfaces. Show native
  cloud-init status but do not ask the user to select a cloud-init device.
- Disable memory ballooning explicitly in placement mode. Existing legacy
  provisioning behavior and UI remain unchanged. Existing VMs are untouched;
  existing placement configurations needing the new evidence or confirmation
  remain viewable but require review before saving or new provisioning.
- Revalidate all placement evidence at provisioning time. There is no
  continuous monitoring, rebalance or automated replacement. Keep the current
  explicit Rancher cluster and machine-pool tag fields until reliable identities
  are verified; do not infer names from VM names.
