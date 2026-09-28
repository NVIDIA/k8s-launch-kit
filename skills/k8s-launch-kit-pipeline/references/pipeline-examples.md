# Pipeline Examples

Eight common l8k invocation patterns, each shown with both local and container commands.

---

## 1. Spectrum-X Full Pipeline

Discover hardware, generate Spectrum-X manifests, and deploy. Obtain an
approved `topology.json` for the **same workers** being discovered; its host
endpoint names must match the Kubernetes worker names. These commands assume
a 2-tier fabric. Use the topology scheme approved for your actual fabric.

**Local:**

```bash
./build/l8k \
  --discover-cluster-config \
  --save-cluster-config ./cluster-config.yaml \
  --fabric ethernet --spectrum-x RA2.1 --network-operator-release 26.1 \
  --topology-scheme 2-tier --topology-file ./topology.json \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./output \
  --deploy \
  --kubeconfig ~/.kube/config
```

**Container:**

```bash
docker run --net=host \
  -v ~/.kube:/kube:ro \
  -v /tmp/l8k-output:/output \
  -v /path/to/topology.json:/input/topology.json:ro \
  nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 \
    --discover-cluster-config \
    --save-cluster-config /output/cluster-config.yaml \
    --fabric ethernet --spectrum-x RA2.1 --network-operator-release 26.1 \
    --topology-scheme 2-tier --topology-file /input/topology.json \
    --deployment-type sriov \
    --multirail \
    --save-deployment-files /output/manifests \
    --deploy \
    --kubeconfig /kube/config
```

---

## 2. InfiniBand SR-IOV Pipeline

Full pipeline for InfiniBand clusters with SR-IOV.

**Local:**

```bash
./build/l8k \
  --discover-cluster-config \
  --save-cluster-config ./cluster-config.yaml \
  --fabric infiniband \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./output \
  --deploy \
  --kubeconfig ~/.kube/config
```

**Container:**

```bash
docker run --net=host \
  -v ~/.kube:/kube:ro \
  -v /tmp/l8k-output:/output \
  nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 \
    --discover-cluster-config \
    --save-cluster-config /output/cluster-config.yaml \
    --fabric infiniband \
    --deployment-type sriov \
    --multirail \
    --save-deployment-files /output/manifests \
    --deploy \
    --kubeconfig /kube/config
```

---

## 3. Heterogeneous Cluster with --groups

When a cluster has multiple hardware groups (e.g., A100 nodes and H100 nodes), target a
specific group:

**Local:**

```bash
./build/l8k \
  --user-config cluster-config.yaml \
  --groups group-0 \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./output-group-0 \
  --deploy \
  --kubeconfig ~/.kube/config
```

**Container:**

```bash
docker run --net=host \
  -v ~/.kube:/kube:ro \
  -v /path/to/cluster-config.yaml:/config/cluster-config.yaml:ro \
  -v /tmp/l8k-output:/output \
  nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 \
    --user-config /config/cluster-config.yaml \
    --groups group-0 \
    --fabric ethernet \
    --deployment-type sriov \
    --multirail \
    --save-deployment-files /output \
    --deploy \
    --kubeconfig /kube/config
```

Repeat with `--groups group-1` for the second hardware group. Each group may use a
different fabric or deployment type.

---

## 4. Base Config + Discovery

Pin the Network Operator version and static settings in a base config, while refreshing
hardware details from the live cluster:

**Local:**

```bash
./build/l8k \
  --user-config base-config.yaml \
  --discover-cluster-config \
  --save-cluster-config ./merged-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./output \
  --deploy \
  --kubeconfig ~/.kube/config
```

**Container:**

```bash
docker run --net=host \
  -v ~/.kube:/kube:ro \
  -v /path/to/base-config.yaml:/config/base-config.yaml:ro \
  -v /tmp/l8k-output:/output \
  nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 \
    --user-config /config/base-config.yaml \
    --discover-cluster-config \
    --save-cluster-config /output/merged-config.yaml \
    --fabric ethernet \
    --deployment-type sriov \
    --multirail \
    --save-deployment-files /output/manifests \
    --deploy \
    --kubeconfig /kube/config
```

---

## 5. CI/CD with JSON Output

Pin the target image/build and preserve both the CLI exit status and diagnostic stderr. This local example uses explicit files; review the generated bundle and apply scope before allowing `--deploy` in a job:

```bash
status=0
./build/l8k --user-config ./cluster-config.yaml \
  --save-deployment-files ./output --deploy \
  --kubeconfig ~/.kube/config --output json \
  >pipeline.json 2>pipeline.log || status=$?
if [ "$status" -ne 0 ]; then
  cat pipeline.log >&2
  exit "$status"
fi
jq . pipeline.json
```

For a container job, mount the approved source config and kubeconfig read-only, and the output directory read-write. Use the same capture rule:

```bash
status=0
docker run --net=host \
  -v ~/.kube:/kube:ro \
  -v /path/to/cluster-config.yaml:/config/cluster-config.yaml:ro \
  -v /tmp/l8k-output:/output \
  nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 \
    --user-config /config/cluster-config.yaml \
    --save-deployment-files /output/manifests --deploy \
    --kubeconfig /kube/config --output json \
    >pipeline.json 2>pipeline.log || status=$?
if [ "$status" -ne 0 ]; then
  cat pipeline.log >&2
  exit "$status"
fi
jq . pipeline.json
```

The image tag is an example; pin the approved image digest and the matching documentation for production. The process status remains authoritative if JSON is partial or absent.

---

## 6. Dry-Run Validation Pipeline

A dry run contacts the cluster API for server-side checks. It does not apply resources or establish controller readiness or traffic. Reuse the approved input and capture pattern above, replacing `--deploy` with `--deploy --dry-run` in the intended local or container command. Do not treat this as a disconnected CI check; it needs kubeconfig and target-cluster API access.

---

## 7. Generate Only (No Deploy, Just Save Files)

Produce manifests for review or GitOps without touching the cluster:

**Local:**

```bash
./build/l8k \
  --user-config cluster-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./output
```

**Container:**

```bash
docker run \
  -v /path/to/cluster-config.yaml:/config/cluster-config.yaml:ro \
  -v /tmp/l8k-output:/output \
  nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 \
    --user-config /config/cluster-config.yaml \
    --fabric ethernet \
    --deployment-type sriov \
    --multirail \
    --save-deployment-files /output
```

No `--kubeconfig` or `--deploy` flag is needed. The manifests in `./output/` can be
committed to a Git repository and applied by ArgoCD, Flux, or another GitOps tool.
