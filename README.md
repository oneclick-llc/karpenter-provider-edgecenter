# Karpenter provider for EdgeCenter

This provider creates Talos Linux workers in EdgeCenter Cloud. It uses the standard Karpenter `NodePool` and `NodeClaim` APIs.
The provider adds one infrastructure resource: `EdgeCenterNodeClass`.

The provider supports dynamic pools and static pools with `spec.replicas`.
Terraform can continue to manage the control plane, permanent workers, networks, gateways, and shared security groups.
Existing Terraform nodes do not enter provider ownership.

## Versions

- Karpenter core: `v1.15.0`
- Talos machinery: `v1.14.2`
- Go: `1.26.9`
- Image: `ghcr.io/oneclick-llc/karpenter-provider-edgecenter`

The module pins CEL to `v0.30.0`. The Talos module requests `v0.31.0`, but that tag was unavailable during this build.

## Resource ownership

Each NodeClass defines one region, a flavor catalog, and an address range.
Karpenter selects a flavor from that catalog. Prices use one consistent currency or relative cost unit across all NodeClasses.
The example price is a placeholder. It is not a billing estimate.

A ConfigMap records each launch in the operator namespace.
Its name identifies the project, region, network, and private IP.
Atomic ConfigMap creation prevents two claims from reserving the same slot.
The journal stores the template snapshot, cloud task IDs, and resource IDs. It does not store secret values.

The provider writes cloud request intent before each creation request.
After a restart, it resumes the recorded cloud task.
VM metadata permits discovery after a lost creation response.
An uncertain port creation stops further requests until an operator resolves the journal.
This behavior prevents duplicate cloud resources.

The provider deletes its VM, boot disk, and reserved port after Karpenter drains the node.
It checks resource ownership before deletion. It does not delete application PVC volumes.
A background controller cleans up journals without a NodeClaim. It stops if a corresponding Kubernetes Node still exists.

## Talos configuration

The recommended `UserData` mode sends the initial worker configuration through the OpenStack config drive.
The worker starts with the existing cluster identity and a stable provider ID derived from the NodeClaim UID.
The launch journal maps that provider ID to the EdgeCenter VM UUID.

The provider reads the worker's Talos version through public control-plane endpoints with mTLS.
The load balancer passes TCP through. Talos verifies the client certificate and proxies the request to the worker.
The worker maintenance API does not need public access.

`SSHGateway` remains available for clusters without the public mTLS path.
This mode requires a pinned Ed25519 gateway host key and TCP forwarding to the worker's private Talos API.

The worker base must use the existing cluster identity and enable the required network configuration.
The provider supplies the hostname, provider ID, node labels, taints, private IP, and KubeSpan endpoints.
It never bootstraps etcd or creates a control-plane node.

The address calculation is `CIDR + hostOffset + slot`.
The public KubeSpan port is `kubeSpanPortBase + slot`.
Both gateways must contain the corresponding DNAT and SNAT rules before the pool can create nodes.
Terraform and Karpenter must use separate slot ranges.

## Install

1. Set the target context:

   ```sh
   export KUBE_CONTEXT=your-context
   ```

2. Install the CRDs:

   ```sh
   kubectl --context "$KUBE_CONTEXT" apply --server-side -k deploy/crds
   kubectl --context "$KUBE_CONTEXT" create namespace karpenter-edgecenter
   ```

3. Create these Secrets in `karpenter-edgecenter`:

   | Secret | Required keys |
   |---|---|
   | `edgecenter-api` | `token` |
   | `talos-worker` | `worker.yaml`, `talosconfig` |
   | `talos-gateway` (SSHGateway only) | `ssh-privatekey`, `known_hosts` |

   `talosconfig` needs permission to read the Talos version. An `os:reader` credential is sufficient.
   In SSHGateway mode, the SSH key needs TCP forwarding to private port `50000`. It does not need shell access.
   Secret references always resolve within the operator namespace.

4. Set `EC_CLUSTER_ID` in `deploy/controller/resources.yaml`.
5. Install the controller:

   ```sh
   kubectl --context "$KUBE_CONTEXT" apply -k deploy/controller
   ```

6. Replace the placeholders in `deploy/examples/nodeclass.yaml`.
7. Set the same cluster ID in the NodeClass and controller.
8. Apply the NodeClass and a NodePool:

   ```sh
   kubectl --context "$KUBE_CONTEXT" apply -f deploy/examples/nodeclass.yaml
   kubectl --context "$KUBE_CONTEXT" apply -f deploy/examples/dynamic.yaml
   ```

The deployment requires a node without the `karpenter.sh/nodepool` label.
This keeps the controller on the permanent cluster capacity.

## Static pools

`deploy/examples/static.yaml` sets `spec.replicas: 1`.
The controller enables Karpenter's `StaticCapacity` feature gate.
Static pools create new provider-owned nodes. They do not adopt existing Terraform nodes.
Karpenter does not permit removal of `spec.replicas` after a pool enters static mode.

## Smoke tests

Run the local lifecycle smoke:

```sh
make smoke
make vet
```

The smoke covers creation, restart recovery, duplicate prevention, ownership checks, and resource cleanup with a simulated cloud API.

For a live smoke, use a separate tainted NodePool and at most two address slots.
The sample workload requests a new node through its node selector.
If Cilium installs a startup taint, include that taint in `NodePool.spec.template.spec.startupTaints`.

```sh
kubectl --context "$KUBE_CONTEXT" create namespace karpenter-smoke
kubectl --context "$KUBE_CONTEXT" apply -f deploy/examples/workload.yaml
kubectl --context "$KUBE_CONTEXT" -n karpenter-smoke rollout status deployment/capacity-smoke --timeout=15m
kubectl --context "$KUBE_CONTEXT" -n karpenter-smoke exec deployment/capacity-smoke -- nslookup kubernetes.default.svc.cluster.local
```

After the workload stops, check that Karpenter deletes the node and its cloud resources.
Check the fixed replica count separately with the static example.

## Operational boundaries

- The flavor catalog and prices are explicit configuration. The provider does not import billing prices or GPU resources.
- The provider supports amd64 Talos workers and on-demand instances.
- An uncertain port creation or failed cloud task requires inspection before further creation.
- Changes to the NodeClass specification cause drift. Use Karpenter disruption budgets to control replacements.
- Node repair and in-place reboot are not enabled.
- The launch journals must survive controller upgrades. Do not delete the operator namespace while it owns cloud resources.
- Removing a NodePool invokes Karpenter termination. It is not an orphan operation.

## Build and publish

```sh
make build
git tag v0.1.0
git push origin v0.1.0
```

The tag workflow runs the smoke tests and vet. It publishes amd64 and arm64 controller images to GHCR.
The controller image architecture does not change the worker architecture.
The workflow uses the repository `GITHUB_TOKEN` and does not require a registry password.

## Upstream code

Karpenter CRDs in `deploy/crds` come from `kubernetes-sigs/karpenter v1.15.0` under the Apache License 2.0.
