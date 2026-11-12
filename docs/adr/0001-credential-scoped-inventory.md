# Credential-scoped inventory in the node-driver controller

**Security gate:** the qualification cluster is Rancher v2.15.2, which is in
the affected range for [GHSA-x82x-2qj6-5rm2](https://github.com/rancher/rancher/security/advisories/GHSA-x82x-2qj6-5rm2).
Do not validate or deploy new meta-proxy inventory calls there until Rancher is
upgraded to v2.15.3 or later in the supported 2.15 line.

Placement configuration needs automatic Proxmox inventory without exposing API
tokens to the browser. We will use one node-driver controller for authorization
and narrowly scoped, read-only inventory, rather than a separate inventory
service or a general-purpose proxy. Every request must authenticate its Rancher
user and authorize use of the selected credential before retrieving its secret;
service-proxy access alone is not authorization. The Rancher-to-controller
identity and per-credential authorization path must be proven before this design
is enabled for use, and discovery must fail closed if it cannot be proven.

The implementation candidate is a Kubernetes aggregated API. It accepts user
identity only over a TLS connection verified against the cluster's
`extension-apiserver-authentication` request-header CA and allowed client names,
then submits a `SubjectAccessReview` for `get` on the exact Secret in
`cattle-global-data` before reading it. The cluster configuration supports this
pattern, but ordinary-user allow/deny behavior through the Rancher UI remains an
end-to-end verification gate; the inventory API stays opt-in until that test
passes. Proxmox target hostnames are separately allow-listed. The inventory
controller reads the PVE NodeDriver's `spec.whitelistDomains` and may also use
an operator-provided static host baseline. The UI can request an explicit
whitelist update, subject to Rancher authorization to edit that NodeDriver.

Discovered inventory is advisory. Provisioning independently revalidates
placement evidence, while preserving template networking and requiring
verifiable dynamic addressing. Existing VMs and legacy provisioning are not
changed by this decision.
