# cluster-autoscaler-provider

Bootstrap for an out-of-tree AWS cluster-autoscaler cloudprovider service using the upstream external-gRPC wrapper.

## First commit scope

This repository currently does one thing:

- exposes the upstream AWS cluster-autoscaler cloudprovider through an external-gRPC service binary in `cmd/cluster-autoscaler-provider`.

The initial goal is only to produce a buildable service binary that matches the example deployment model while keeping local divergence from upstream minimal. Multi-region ASG awareness and non-EKS behavior changes come next.

## Initial plan

1. Keep the upstream AWS provider behavior intact as the starting point.
2. Keep this repository as a thin external-gRPC integration layer until an infra-specific provider change is required.
3. Add multiregion-aware ASG discovery and scale-up logic for clusters whose nodes span regions.
4. Remove or adapt EKS-specific assumptions as they show up in discovery, template generation, or cache lookups.
5. Fork only the upstream AWS provider code that must actually diverge.

## Build

From the repository root:

```bash
nix develop
go build ./cmd/cluster-autoscaler-provider
```

This module depends on the upstream `k8s.io/autoscaler/cluster-autoscaler` module and documents the pinned autoscaler tag in `go.mod`.
