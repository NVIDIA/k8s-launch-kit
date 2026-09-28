<!--
SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Use the network in an application

Start from an [accepted deployment](validation.md#acceptance-outcomes) and a reviewed `cluster-config.yaml`. A Launch Kit custom workload is an operational Kubernetes resource; the built-in `*example*.yaml` DaemonSets are temporary connectivity fixtures. This walkthrough uses a one-replica client as a complete rendering example. Its BusyBox image can inspect interfaces and send ICMP; it does **not** prove RDMA application traffic. Replace it with a site-approved, digest-pinned image with the tools your application needs before production use.

## Identify the network and target nodes

Inspect the generated `SriovNetwork` or other secondary-network CR, its workload namespace and resulting NetworkAttachmentDefinition, the device-plugin resource name, and the selected worker nodes. Confirm image access and pod security in that namespace. In this example, use the same reviewed source configuration and profile inputs as the accepted network deployment; do not infer resource names from a different cluster or generation run.

## Preserve validation fixtures before replacing them

Generation replaces its output directory. Render a standard baseline in a separate directory and keep its example fixtures outside both generated directories:

```bash
l8k generate --user-config ./cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --save-deployment-files ./baseline-deployment
mkdir -p ./validation-fixtures
cp ./baseline-deployment/network-operator/*example*.yaml ./validation-fixtures/
```

Review the copied fixtures: namespace, labels/selectors, network attachments, node placement, and generated resource names must match the network to be deployed. If the glob matches nothing, stop and resolve the profile or path mismatch.

## Render and review the application

The repository's complete [sample Deployment](../examples/workloads/rdma-client.yaml) has one named container and a versioned image. It leaves network annotation, node affinity, and device resources for Launch Kit to inject. Use a separate output directory:

```bash
l8k generate --user-config ./cluster-config.yaml \
  --fabric ethernet --deployment-type sriov \
  --workload-manifest ./docs/examples/workloads/rdma-client.yaml \
  --save-deployment-files ./application-deployment
cp ./validation-fixtures/*example*.yaml ./application-deployment/network-operator/
```

Check `application-deployment/.l8k/resolved-config.yaml`, `network-operator/90-workload-*.yaml`, and the copied fixtures before deployment. Launch Kit sets the first network namespace, adds a group suffix, Multus annotation, requested device resources to the **first** container, and required group node affinity. On Kubernetes, the workload renders once per source group into the first network namespace; standard networks can still span multiple namespaces. On OpenShift, it renders per shared network bucket and workload namespace. Check that the resource prefix is appropriate for the flavor and the image meets local admission rules.

The example fixtures must have the **same intended networks, workers, rails, and resource identities** as the application bundle. If they differ, regenerate the baseline from the exact final inputs. Keep `validation-fixtures/` separately for future regeneration; copying fixtures into the bundle alone is not a durable source.

## Deploy, verify, and remove the application

Preview and apply the reviewed bundle using the [deployment procedure](../advanced/deployment.md#preflight). `l8k deploy` applies `90-workload-*.yaml` and skips `*example*.yaml`. Verify the resulting Deployment and pods in the chosen namespace, their node placement, allocated device resource, attached secondary interface, and application-specific traffic:

```bash
kubectl get deployment,pods -n <workload-namespace> -o wide
kubectl describe pod -n <workload-namespace> <pod-name>
kubectl exec -n <workload-namespace> <pod-name> -- ip addr
```

BusyBox may not include the `ip` command in every build; if unavailable, use `ifconfig` or the site's approved diagnostic image. An ICMP test to an approved peer address can establish basic reachability, but RDMA acceptance needs the application's own traffic test. Do not assume pod readiness demonstrates that traffic path.

Run `l8k validate --deployment-files ./application-deployment --wait 10m` and review its report. Validation selects only the copied `*example*.yaml` fixtures for connectivity; the custom workload remains operational and is outside that selection. The report covers its stated network checks, not application correctness. When finished with the sample, remove the exact generated Deployment identity in its rendered namespace through the owning deployment process; keep the network resources and retained evidence.
