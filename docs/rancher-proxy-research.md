# Rancher proxy and Proxmox credentials

## Conclusion

Rancher's existing meta proxy is **not shown to be a general-purpose,
credential-backed Proxmox proxy** for a node-driver extension. It can proxy to
allowlisted external HTTPS hosts, and it has credential-backed signer flows
for credential types implemented by Rancher. In Rancher v2.15.3, the source
does not provide a generic extension hook to read an arbitrary cloud-credential
Secret, construct a Proxmox authorization header, and make the request on the
extension's behalf. Nor does the inspected proxy path expose a per-request
option to skip Proxmox TLS certificate verification.

Therefore, using `/meta/proxy` alone does not currently appear to satisfy all
three requirements: keep the Proxmox secret out of browser state, authorize
access to the exact credential, and honor the credential's insecure-TLS option.
This is a source-based assessment, not a live test against the project's
Rancher instance. It does not rule out an undocumented or differently
configured deployment path.

## What the meta proxy does

Rancher v2.15.3's [`pkg/httpproxy/proxy.go`](https://github.com/rancher/rancher/blob/v2.15.3/pkg/httpproxy/proxy.go)
rewrites requests to external destinations and checks the destination hostname
against Rancher's proxy allowlist (see `proxy` and `isAllowed`, around lines
180–203). Allowlist entries are populated through Rancher's
[`ProxyEndpoint` management resource](https://github.com/rancher/rancher/blob/v2.15.3/pkg/apis/management.cattle.io/v3/proxy_types.go)
which describes HTTPS domain routes.

The proxy also recognizes Rancher's special `X-API-CattleAuth-Header`. It
removes that header from the forwarded request, resolves a built-in signer,
and lets the signer obtain a credential via `secretGetter` (around lines
211–245). The getter uses the authenticated request user and Kubernetes
authorization to check access to the exact Secret before reading it (around
lines 248–279). Rancher additionally has indirect access checks for supported
cluster/credential associations (around lines 281–330).

This is a useful existing pattern, but the credential signing behavior is
implemented by Rancher's own signer types; it is not a generic facility for an
extension to supply an arbitrary credential format or authorization header.
The source comments specifically describe supported indirect-access cases
(EKS and provisioning clusters) and say GKE/AKS use their specific proxies.
No Proxmox signer or documented extension registration API was found in the
v2.15.3 source.

## TLS behavior

The meta proxy uses Go's `httputil.ReverseProxy` (around lines 147–163). Its
outbound transport is not configured in this path to skip certificate
verification, and the proxy API does not expose an insecure-TLS setting for an
individual destination or credential. Thus a Proxmox endpoint with an
untrusted/self-signed certificate should be treated as unsupported through
this path unless the certificate is trusted by Rancher's outbound TLS
configuration. Do not infer that a Rancher-wide outbound TLS setting, if
present, is safely or specifically applicable to this proxy.

Rancher's Steve `/k8s/clusters/{clusterID}/v1` proxy is a separate path for
Kubernetes cluster APIs; it is not a general proxy to Proxmox. See
[`pkg/api/steve/proxy/proxy.go`](https://github.com/rancher/rancher/blob/v2.15.3/pkg/api/steve/proxy/proxy.go).

## Recommendation

Do not replace the controller/API design with `/meta/proxy` on the assumption
that it can use Proxmox cloud credentials securely. Before removing the
controller, either:

1. verify with Rancher maintainers that v2.15 has a supported extension point
   for a Proxmox signer and per-credential TLS behavior; or
2. prototype against the exact Rancher v2.15.3 deployment, confirming the
   extension can request inventory without receiving the Secret, users without
   access to the selected Secret are denied, and both trusted and explicitly
   insecure Proxmox TLS cases behave as intended.

The existing controller remains a separate component because it can implement
Proxmox-specific TLS behavior and exact-Secret authorization without sending
the Proxmox token to the browser. The chart enables its aggregated inventory
API by default for Rancher v2.15.3 and later. Ordinary-user allow/deny behavior
should still be verified end to end in each supported Rancher configuration.

## Primary sources

- Rancher v2.15.3 meta proxy implementation: [`pkg/httpproxy/proxy.go`](https://github.com/rancher/rancher/blob/v2.15.3/pkg/httpproxy/proxy.go)
- Rancher v2.15.3 proxy endpoint API type: [`pkg/apis/management.cattle.io/v3/proxy_types.go`](https://github.com/rancher/rancher/blob/v2.15.3/pkg/apis/management.cattle.io/v3/proxy_types.go)
- Rancher v2.15.3 Steve Kubernetes API proxy: [`pkg/api/steve/proxy/proxy.go`](https://github.com/rancher/rancher/blob/v2.15.3/pkg/api/steve/proxy/proxy.go)
- Rancher v2.15.3 cloud credential access-role creation: [`pkg/controllers/management/cloudcredential/controller.go`](https://github.com/rancher/rancher/blob/v2.15.3/pkg/controllers/management/cloudcredential/controller.go)
