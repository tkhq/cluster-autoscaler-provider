# cluster-autoscaler-provider

Out-of-tree AWS cluster-autoscaler cloudprovider service built around the upstream external-gRPC wrapper.

## Design

This repository builds a single binary in `cmd/cluster-autoscaler-provider` with two execution modes:

- `provider` mode wraps one regional upstream AWS cloudprovider instance behind the external-gRPC service
- `router` mode fronts multiple provider instances and routes requests to the appropriate region

The current design goal is to preserve the upstream AWS provider implementation as-is for normal regional behavior and keep repository-specific logic in the router and integration layer. That separation is intentional: we want multi-region behavior without carrying an invasive fork of the upstream AWS provider code.

EKS support is available because the provider mode delegates to the upstream AWS provider, but EKS-specific behavior is not the main design target for this repository. The priority is a conservative multi-region deployment model that remains compatible with upstream AWS provider behavior.

## Scope

This repository is responsible for:

1. Keep the upstream AWS provider behavior intact as the starting point.
2. Provide a router mode that aggregates multiple regional provider instances behind one external cloudprovider endpoint.
3. Keep repository-specific multi-region logic outside the upstream provider implementation.
4. Fork only the upstream AWS provider code that must actually diverge.

## Build

From the repository root:

```bash
nix develop
go build ./cmd/cluster-autoscaler-provider
```

This module depends on the upstream `k8s.io/autoscaler/cluster-autoscaler` module and documents the pinned autoscaler tag in `go.mod`.

## Deployment model

The deployment model is a single binary with explicit `router` and `provider` modes backed by a required shared runtime config file.

- Router instances read the shared config file to determine which provider backends should be available.
- Provider instances read the same shared config file and select their own regional configuration from it.
- `--config` or `CLUSTER_AUTOSCALER_PROVIDER_CONFIG` is required for both modes.
- Provider-specific AWS settings such as `--cloud-config`, cluster name, and node group discovery remain pass-through inputs to the upstream AWS provider.

## Runtime config

The shared runtime config file is the source of truth for router/provider topology.

- `router` defines shared router settings such as listen addresses and default backend RPC behavior.
- `providerDefaults` defines provider settings shared across regions.
- `providers` is keyed by region and defines the port plus any per-region overrides.
- router mode reads all configured regions from `providers` and derives its backend list from that map.
- provider mode takes a single `region` selector, reads the same file, and resolves its runtime settings from the matching region entry.

A region-keyed config looks like this:

```yaml
router:
  grpcAddress: :8086
  httpAddress: :8080
  cacheTTL: 15s
  backendRPCTimeout: 5s
providerDefaults:
  clusterName: dev
  nodeGroupAutoDiscovery:
    - asg:tag=k8s.io/cluster-autoscaler/dev
  skipNodesWithLocalStorage: false
  skipNodesWithSystemPods: false
providers:
  us-east-1:
    port: 8081
    rpcTimeout: 5s
  us-west-2:
    port: 8082
    clusterName: dev-west
    skipNodesWithSystemPods: true
```

Notes:

- Use region as the provider key in `providers`.
- Keep the shared file focused on router/provider topology and light per-region runtime overrides such as port or timeout.
- `providerDefaults` can supply shared values for `clusterName`, `nodeGroupAutoDiscovery`, `skipNodesWithLocalStorage`, and `skipNodesWithSystemPods`.
- Any of those YAML fields can be overridden in a specific region entry when a region must diverge.
- Provider runtime settings come from the shared config file. Provider startup only supplies the region selector used to choose an entry from `providers`.
- AWS-provider-specific settings that are not shared runtime topology should stay outside this file unless they are truly common across provider instances.

### Runtime config options

Top-level fields:

| Field | Required | Description |
| --- | --- | --- |
| `router` | no | Router process settings. Omitted fields use flag/env defaults. |
| `providerDefaults` | no | Default AWS provider settings applied to every region unless the region overrides them. |
| `providers` | yes | Region-keyed provider map. Each key must be a non-empty AWS region, and at least one provider is required. |

`router` fields:

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `grpcAddress` | no | `:8086` | Address the router listens on for Cluster Autoscaler external-gRPC calls. |
| `httpAddress` | no | `:8080` | Address the router listens on for `/healthz`, `/ready`, and `/metrics`. |
| `cacheTTL` | no | `15s` | How long the router caches successful `NodeGroups` responses. Must be greater than zero when set. |
| `backendRPCTimeout` | no | `5s` | Default timeout for router-to-provider RPCs. Must be greater than zero when set. |

`providerDefaults` fields:

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `clusterName` | no | empty | Cluster name passed to the upstream AWS cloud provider. |
| `nodeGroupAutoDiscovery` | no | empty | List of AWS node group auto-discovery specs, for example `asg:tag=k8s.io/cluster-autoscaler/dev`. Empty entries are rejected. |
| `skipNodesWithLocalStorage` | no | `false` | Passed through to the upstream AWS provider to control scale-down behavior for nodes with local storage. |
| `skipNodesWithSystemPods` | no | `false` | Passed through to the upstream AWS provider to control scale-down behavior for nodes running system pods. |

`providers.<region>` fields:

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `port` | yes | none | Local gRPC port for the regional provider process. Must be unique across providers and between `1` and `65535`. The router connects to `127.0.0.1:<port>`. |
| `rpcTimeout` | no | `router.backendRPCTimeout` | Per-region router-to-provider RPC timeout. Must be greater than zero when set. |
| `clusterName` | no | `providerDefaults.clusterName` | Per-region override for `clusterName`. |
| `nodeGroupAutoDiscovery` | no | `providerDefaults.nodeGroupAutoDiscovery` | Per-region replacement list for auto-discovery specs. This replaces the default list; it does not append to it. |
| `skipNodesWithLocalStorage` | no | `providerDefaults.skipNodesWithLocalStorage` | Per-region override for local-storage scale-down behavior. |
| `skipNodesWithSystemPods` | no | `providerDefaults.skipNodesWithSystemPods` | Per-region override for system-pod scale-down behavior. |

Durations use Go duration strings such as `5s`, `30s`, or `2m`.

## Cluster Autoscaler external-gRPC config

Cluster Autoscaler also has its own `--cloud-config` file for the `externalgrpc` cloud provider. That file is separate from this repository's runtime config.

Example:

```yaml
address: cluster-autoscaler-provider:8086
grpc_timeout: 30s
```

Supported external-gRPC cloud config fields:

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `address` | yes | none | Router gRPC address that Cluster Autoscaler calls. |
| `grpc_timeout` | no | `5s` | Timeout Cluster Autoscaler applies to each external-gRPC cloud provider call. Increase this above the router/provider timeout budget when remote regions can take longer than five seconds. |
| `cert` | no | empty | Client TLS certificate path used by Cluster Autoscaler when connecting to the router. If omitted, CA uses insecure gRPC. |
| `key` | no | empty | Client TLS private key path. Required when `cert` is set. |
| `cacert` | no | empty | CA certificate path used to verify the router server certificate. Required when `cert` is set. |
