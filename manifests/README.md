# region-cluster-autoscaler manifests

This directory contains a baseline deployment for:

- the external gRPC provider service from this repo
- upstream cluster-autoscaler configured with `--cloud-provider=externalgrpc`
- cert-manager resources for mTLS between the two

## mTLS with cert-manager

Yes, cluster-autoscaler still needs its own certificate and private key when you use mTLS.

The trust model is:

- the service presents a server certificate to cluster-autoscaler
- cluster-autoscaler presents a client certificate to the service
- both certificates are signed by the same CA
- both sides trust that CA via `ca.crt`

In this manifest set:

- `region-cluster-autoscaler-server-tls` is mounted by the service
- `region-cluster-autoscaler-client-tls` is mounted by cluster-autoscaler
- both certs are issued by the same cert-manager `Issuer`
- the issuer is a local CA issuer so the target secrets include a usable `ca.crt`

## Files

- `00-cert-manager.yaml`: bootstrap a local CA and issue server/client certs
- `10-external-grpc.yaml`: ServiceAccount, Service, and Deployment for this repo
- `20-cluster-autoscaler-cloud-config.yaml`: externalgrpc client config consumed by CA
- `30-cluster-autoscaler-rbac.yaml`: upstream CA RBAC
- `40-cluster-autoscaler.yaml`: upstream CA Deployment configured for externalgrpc
- `kustomization.yaml`: convenience entrypoint

## Required edits before apply

- Replace `IMAGE` in `10-external-grpc.yaml`
- Replace `AWS_REGION` in `10-external-grpc.yaml`
- Replace `CLUSTER_NAME` in `10-external-grpc.yaml`
- Add your IRSA annotation to the `region-cluster-autoscaler` ServiceAccount in `10-external-grpc.yaml`

Optional edits:

- If you already have a cert-manager issuer you want to use, replace `00-cert-manager.yaml`
  Make sure it also gives you a trustable CA bundle for both peers. ACME-style issuers are usually the wrong fit for this internal mTLS case.
- If you need manual node group config, replace `--node-group-auto-discovery=...` with one or more `--nodes=min:max:asg-name`
- If you need multi-region, add repeated `--aws-region=<region>` flags and do not rely on a single `AWS_REGION`
- If you need AWS endpoint overrides, mount a cloud config file and add `--cloud-config=/config/cloud.conf`

## Apply

```bash
kubectl apply -k manifests/
```
