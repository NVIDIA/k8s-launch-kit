# Pipeline Examples

Eight common l8k invocation patterns, each shown with both local and container commands.

---

## 1. Spectrum-X Full Pipeline

Discover hardware, generate Spectrum-X manifests, and deploy.

**Local:**

```bash
./build/l8k \
  --discover-cluster-config \
  --save-cluster-config ./cluster-config.yaml \
  --fabric spectrum-x \
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
    --fabric spectrum-x \
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

## 3. Heterogeneous Cluster with --group

When a cluster has multiple hardware groups (e.g., A100 nodes and H100 nodes), target a
specific group:

**Local:**

```bash
./build/l8k \
  --user-config cluster-config.yaml \
  --group group-0 \
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
    --group group-0 \
    --fabric ethernet \
    --deployment-type sriov \
    --multirail \
    --save-deployment-files /output \
    --deploy \
    --kubeconfig /kube/config
```

Repeat with `--group group-1` for the second hardware group. Each group may use a
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

Full pipeline in a CI job, capturing structured JSON for downstream processing:

**Local:**

```bash
OUTPUT=$(./build/l8k \
  --discover-cluster-config \
  --save-cluster-config ./cluster-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./output \
  --deploy \
  --kubeconfig ~/.kube/config \
  --output json --yes 2>/dev/null)

EXIT_CODE=$?
echo "$OUTPUT" | jq .

if [ $EXIT_CODE -ne 0 ]; then
  echo "Pipeline failed with exit code $EXIT_CODE"
  echo "$OUTPUT" | jq '.errors[]?'
  exit $EXIT_CODE
fi
```

**Container:**

```bash
OUTPUT=$(docker run --net=host \
  -v ~/.kube:/kube:ro \
  -v /tmp/l8k-output:/output \
  nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 \
    --discover-cluster-config \
    --save-cluster-config /output/cluster-config.yaml \
    --fabric ethernet \
    --deployment-type sriov \
    --multirail \
    --save-deployment-files /output/manifests \
    --deploy \
    --kubeconfig /kube/config \
    --output json --yes 2>/dev/null)

echo "$OUTPUT" | jq .
```

---

## 6. Dry-Run Validation Pipeline

Validate the full pipeline without applying anything to the cluster. Useful in PR checks:

**Local:**

```bash
./build/l8k \
  --discover-cluster-config \
  --save-cluster-config ./cluster-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./output \
  --deploy \
  --dry-run \
  --kubeconfig ~/.kube/config \
  --output json --yes 2>/dev/null | jq .
```

**Container:**

```bash
docker run --net=host \
  -v ~/.kube:/kube:ro \
  -v /tmp/l8k-output:/output \
  nvcr.io/nvidia/cloud-native/k8s-launch-kit:v26.1.0 \
    --discover-cluster-config \
    --save-cluster-config /output/cluster-config.yaml \
    --fabric ethernet \
    --deployment-type sriov \
    --multirail \
    --save-deployment-files /output/manifests \
    --deploy \
    --dry-run \
    --kubeconfig /kube/config \
    --output json --yes 2>/dev/null | jq .
```

Dry-run performs server-side validation against the cluster API without creating or
modifying any resources.

---

## 7. LLM-Assisted Profile Selection Pipeline

Let the user describe their intent in natural language, then use the JSON output to
drive profile selection:

**Step 1: Discover and save config:**

```bash
./build/l8k \
  --discover-cluster-config \
  --save-cluster-config ./cluster-config.yaml \
  --kubeconfig ~/.kube/config \
  --output json --yes 2>/dev/null > discovery-output.json
```

**Step 2: Feed discovery output to an LLM agent to select fabric and deployment type.**

The agent reads the hardware groups from `discovery-output.json` and recommends
appropriate flags (e.g., `--fabric ethernet --deployment-type sriov --multirail`).

**Step 3: Generate and deploy with the selected profile:**

```bash
./build/l8k \
  --user-config ./cluster-config.yaml \
  --fabric ethernet \
  --deployment-type sriov \
  --multirail \
  --save-deployment-files ./output \
  --deploy \
  --kubeconfig ~/.kube/config \
  --output json --yes 2>/dev/null | jq .
```

---

## 8. Generate Only (No Deploy, Just Save Files)

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
