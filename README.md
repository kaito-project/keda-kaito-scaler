# KEDA Kaito Scaler

A dedicated KEDA external scaler designed to automatically scale GPU inference workloads in [Kaito](https://github.com/kaito-project/kaito), eliminating the need for external dependencies such as Prometheus.

## Overview

The KEDA Kaito Scaler provides intelligent autoscaling for vLLM inference workloads by directly collecting metrics from inference pods. It offers a simplified, user-friendly alternative to complex Prometheus-based scaling solutions while maintaining the same powerful scaling capabilities.

### Key Features

- **🚀 Zero Dependencies**: No Prometheus stack required - directly scrapes metrics from inference pods
- **⚡ Simple Configuration**: Minimal YAML configuration with intelligent defaults
- **🎯 GPU-Optimized**: Conservative scaling policies designed for expensive GPU resources
- **🔒 Secure by Default**: Built-in TLS authentication between components
- **📊 Smart Fallback**: Intelligent handling of missing metrics to prevent scaling flapping
- **🔧 Minimal Maintenance**: Self-managing certificates and authentication

## Architecture

![keda-kaito-scaler-arch](./docs/images/keda-kaito-scaler-arch.png)

## Prerequisites

### Enable InferenceSet Controller during KAITO install

To enable autoscaling of KAITO GPU inference workloads, the `InferenceSet` custom resource must be utilized in KAITO, and the InferenceSet Controller should be activated during the KAITO installation. The `InferenceSet` feature was introduced in KAITO version `v0.8.0` as an alpha feature.

```bash
export CLUSTER_NAME=kaito

helm repo add kaito https://kaito-project.github.io/kaito/charts/kaito
helm repo update
helm upgrade --install kaito-workspace kaito/workspace \
  --namespace kaito-workspace \
  --create-namespace \
  --set clusterName="$CLUSTER_NAME" \
  --set featureGates.enableInferenceSetController=true \
  --wait
```

### install KEDA

> the following example demonstrates how to install KEDA using Helm chart. For instructions on installing KEDA through other methods, please refer to the [KEDA deployment guide](https://github.com/kedacore/keda#deploying-keda).

```bash
helm repo add kedacore https://kedacore.github.io/charts
helm install keda kedacore/keda --namespace keda --create-namespace
```

## Quick Start

### Deploy KEDA Kaito Scaler

> autoscaling of KAITO GPU inference workloads requires KEDA Kaito Scaler version v0.3.3 or higher.

```bash
helm repo add keda-kaito-scaler https://kaito-project.github.io/keda-kaito-scaler/charts/kaito-project
helm repo update
```

Check available versions:

```bash
helm search repo -l keda-kaito-scaler
```

```text
NAME                                    CHART VERSION   APP VERSION   DESCRIPTION
keda-kaito-scaler/keda-kaito-scaler     0.3.3           v0.3.3        A Helm chart for Kaito keda-kaito-scaler compon...
keda-kaito-scaler/keda-kaito-scaler     0.3.0           v0.3.0        A Helm chart for Kaito keda-kaito-scaler compon...
keda-kaito-scaler/keda-kaito-scaler     0.2.0           v0.2.0        A Helm chart for Kaito keda-kaito-scaler compon...
keda-kaito-scaler/keda-kaito-scaler     0.0.1           v0.0.1        A Helm chart for Kaito keda-kaito-scaler compon...
```

```bash
helm upgrade --install keda-kaito-scaler -n keda keda-kaito-scaler/keda-kaito-scaler
```

> **keda-kaito-scaler must be installed in the same namespace as KEDA**
> (`keda` in the command above). The chart's `ClusterTriggerAuthentication`
> references TLS secrets created in the scaler's namespace, and KEDA only
> resolves those secrets when it can read them from its own namespace.
>
> **Required CRDs.** At startup keda-kaito-scaler waits for every CRD it relies
> on to be installed: `InferenceSet` (`kaito.sh/v1beta1`), `ScaledObject`
> (`keda.sh/v1alpha1`), and `ClusterTriggerAuthentication` (`keda.sh/v1alpha1`).
> It polls for them rather than failing fast, so keda-kaito-scaler can be
> deployed in any order relative to KEDA and the KAITO `InferenceSet` controller
> (see [Prerequisites](#prerequisites)) — it simply blocks until all three CRDs
> are present.

### Create a Kaito InferenceSet for running inference workloads

You can drive autoscaling in two ways:

1. **Auto-provision (recommended)** — annotate the `InferenceSet` with one
   or more metrics (waiting-queue length, queue latency, ...) and let
   keda-kaito-scaler create and reconcile the `ScaledObject` for you. Configuring
   a single metric is the simplest way to get started; adding more combines them
   under a conservative AND policy. See
   [Auto-provision](#option-1-auto-provision-recommended).
2. **Manual mode** — author the `ScaledObject` yourself and point its `external`
   trigger at the keda-kaito-scaler service.

#### Option 1: Auto-provision (recommended)

Adding the `metrics` annotation (together with `auto-provision: "true"`) enables
auto-provisioning: keda-kaito-scaler builds and reconciles a `ScaledObject` whose
KEDA `scalingModifiers` formula applies a conservative **AND** policy — scale up by
one replica only when *every* configured metric is above its
up-threshold (and newly added pods are ready), scale down by one replica only
when *every* metric is below its down-threshold, otherwise hold. Each scale
step is capped at ±1 replica per cooldown to protect expensive GPU capacity.
Configuring a single metric is fully supported; it just yields a one-signal
formula.

The per-metric configuration lives in a single `scaledobject.kaito.sh/metrics`
annotation, whose value is a YAML (or JSON) list — one entry per metric. Global
settings stay as their own `scaledobject.kaito.sh/` annotations. Any Prometheus
metric name is accepted.

Metric values are served from an in-memory cache that a background poller keeps
fresh: every scaler replica continuously scrapes each tracked `InferenceSet`, so
any replica can answer KEDA from a complete local window without scraping on the
request path. Histogram metrics are reduced to the **average observation over a
rolling cache window** (`ΔSum / ΔCount`), which stays responsive to sudden
changes instead of being diluted by the full cumulative history.

Each entry in the `metrics` list accepts the following fields:

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `name` | yes | – | Prometheus metric name. |
| `type` | yes | – | Aggregation: `gauge` → per-replica average across pods; `histogram` → average over the metric cache window. Both are replica-count independent. |
| `source` | no | `modelpod` | Where the metric is scraped. Only `modelpod` (the model-serving pods behind the `InferenceSet`'s workspace `Service`s) is supported. |
| `upthreshold` | yes | – | Scale-up threshold (float). |
| `downthreshold` | yes | – | Scale-down threshold (float). Must be `<= upthreshold`. |
| `metriccachewindow` | no | `300` | Rolling cache window in **seconds** over which a `histogram` metric is averaged. Each histogram metric may set its own; rejected on `gauge` metrics. |

The remaining global annotations:

| Annotation (`scaledobject.kaito.sh/`) | Required | Default | Description |
| --- | --- | --- | --- |
| `auto-provision` | yes | – | Must be `"true"` to enable auto-provisioning. |
| `metrics` | yes | – | YAML/JSON list of metric entries (see fields above). At least one entry is required. |
| `combinepolicy` | no | `AND` | How per-metric conditions are combined. Only `AND` is currently supported. |
| `evaluationwindow` | no | `60` | Scale-up stabilization window (seconds). |
| `scaleupcooldown` | no | `300` | Minimum seconds between scale-up steps. |
| `scaledowncooldown` | no | `300` | Minimum seconds between scale-down steps. |
| `min-replicas` | no | `1` | Minimum replica count. Values `<= 1` collapse to `1`. |
| `max-replicas` | no | derived from `spec.nodeCountLimit` | Maximum replica count. Must be `> 1` and `>= min-replicas`; if absent, `spec.nodeCountLimit` must be set. |

##### Single-metric example

The simplest way to get started is to configure a single metric. The following
`InferenceSet` scales on just the vLLM waiting-queue length (a `gauge`, averaged
per replica):

```bash
cat <<EOF | kubectl apply -f -
apiVersion: kaito.sh/v1beta1
kind: InferenceSet
metadata:
  annotations:
    scaledobject.kaito.sh/auto-provision: "true"

    # A single metric: vLLM waiting-queue length (modelpod, gauge -> per-replica average)
    scaledobject.kaito.sh/metrics: |
      - name: vllm:num_requests_waiting
        type: gauge
        upthreshold: 10
        downthreshold: 1
  name: phi-4
  namespace: default
spec:
  labelSelector:
    matchLabels:
      apps: phi-4
  replicas: 1
  nodeCountLimit: 5
  template:
    inference:
      preset:
        accessMode: public
        name: phi-4-mini-instruct
    resource:
      instanceType: Standard_NC24ads_A100_v4
EOF
```

With a single metric the AND policy collapses to a one-signal formula: scale up by
one replica when the **per-replica waiting-queue length** is above its up-threshold
(and newly added pods are ready), scale down by one replica when it is below its
down-threshold, otherwise hold.

<details>
<summary>Rendered <code>ScaledObject</code> for the single-metric example above</summary>

keda-kaito-scaler translates the auto-provision annotations into the following
`ScaledObject` (assuming the scaler is deployed as Service `keda-kaito-scaler` in
namespace `keda-kaito-scaler` on gRPC port `9443`). One external trigger is emitted
for the metric — named after the sanitized metric name — plus a `readiness_gate`
trigger, and the single-signal policy is encoded in the `scalingModifiers.formula`:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: phi-4
  namespace: default
  annotations:
    scaledobject.kaito.sh/managed-by: keda-kaito-scaler
  ownerReferences:
    - apiVersion: kaito.sh/v1beta1
      kind: InferenceSet
      name: phi-4
      controller: true
      blockOwnerDeletion: true
spec:
  scaleTargetRef:
    apiVersion: kaito.sh/v1beta1
    kind: InferenceSet
    name: phi-4
  pollingInterval: 15
  minReplicaCount: 1
  maxReplicaCount: 5
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 60     # evaluationwindow
          selectPolicy: Max
          policies:
            - type: Pods
              value: 1
              periodSeconds: 300             # scaleupcooldown
          tolerance: "0.1"
        scaleDown:
          stabilizationWindowSeconds: 300    # scaledowncooldown
          selectPolicy: Max
          policies:
            - type: Pods
              value: 1
              periodSeconds: 300             # scaledowncooldown
          tolerance: "0.1"
    scalingModifiers:
      target: "1"
      metricType: Value
      formula: >-
        (readiness_gate == 1 && vllm_num_requests_waiting > 10) ? 2.0 :
        ((vllm_num_requests_waiting < 1) ? 0.5 : 1.0)
  triggers:
    # Metric 0: gauge -> service-avg
    - type: external
      name: vllm_num_requests_waiting
      metricType: Value
      authenticationRef:
        name: keda-kaito-scaler-creds
        kind: ClusterTriggerAuthentication
      metadata:
        inferenceSetName: phi-4
        inferenceSetNamespace: default
        scalerAddress: keda-kaito-scaler.keda-kaito-scaler.svc.cluster.local:9443
        metricName: vllm:num_requests_waiting
        metricSource: modelpod
        aggregation: service-avg
    # Readiness gate: no scrape, reports 1 once all replicas are ready, 0 otherwise
    - type: external
      name: readiness_gate
      metricType: Value
      authenticationRef:
        name: keda-kaito-scaler-creds
        kind: ClusterTriggerAuthentication
      metadata:
        inferenceSetName: phi-4
        inferenceSetNamespace: default
        scalerAddress: keda-kaito-scaler.keda-kaito-scaler.svc.cluster.local:9443
        metricName: readiness_gate
        aggregation: gate
```

</details>

##### Multi-metric example

Adding more metrics combines them under the conservative AND policy. The following
`InferenceSet` scales on both the waiting-queue length and the p95 request queue
time:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: kaito.sh/v1beta1
kind: InferenceSet
metadata:
  annotations:
    scaledobject.kaito.sh/auto-provision: "true"

    # Two metrics combined under the conservative AND policy:
    #   - vLLM waiting-queue length      (modelpod, gauge -> per-replica average)
    #   - avg request queue time (window) (modelpod, histogram -> windowed average)
    scaledobject.kaito.sh/metrics: |
      - name: vllm:num_requests_waiting
        type: gauge
        upthreshold: 10
        downthreshold: 1
      - name: vllm:request_queue_time_seconds
        type: histogram
        upthreshold: 30.0
        downthreshold: 1.0
        metriccachewindow: 300

    # Optional global tuning (defaults shown)
    scaledobject.kaito.sh/combinepolicy: "AND"
    scaledobject.kaito.sh/evaluationwindow: "60"
    scaledobject.kaito.sh/scaleupcooldown: "300"
    scaledobject.kaito.sh/scaledowncooldown: "300"
  name: phi-4
  namespace: default
spec:
  labelSelector:
    matchLabels:
      apps: phi-4
  replicas: 1
  nodeCountLimit: 5
  template:
    inference:
      preset:
        accessMode: public
        name: phi-4-mini-instruct
    resource:
      instanceType: Standard_NC24ads_A100_v4
EOF
```

With this configuration the `InferenceSet` scales up by one replica only when the
**per-replica waiting-queue length** *and* the **p95 request queue time** are both
above their up-thresholds (and newly added pods are ready), and scales down by one
replica only when both are below their down-thresholds — a deliberately
conservative policy well suited to expensive GPU capacity.

<details>
<summary>Rendered <code>ScaledObject</code> for the example above</summary>

keda-kaito-scaler translates the auto-provision annotations into the
following `ScaledObject` (assuming the scaler is deployed as Service `keda-kaito-scaler`
in namespace `keda-kaito-scaler` on gRPC port `9443`). One external trigger is
emitted per metric — named after the sanitized metric name — plus a
`readiness_gate` trigger, and the AND policy is encoded in the
`scalingModifiers.formula`:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: phi-4
  namespace: default
  annotations:
    scaledobject.kaito.sh/managed-by: keda-kaito-scaler
  ownerReferences:
    - apiVersion: kaito.sh/v1beta1
      kind: InferenceSet
      name: phi-4
      controller: true
      blockOwnerDeletion: true
spec:
  scaleTargetRef:
    apiVersion: kaito.sh/v1beta1
    kind: InferenceSet
    name: phi-4
  pollingInterval: 15
  minReplicaCount: 1
  maxReplicaCount: 5
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 60     # evaluationwindow
          selectPolicy: Max
          policies:
            - type: Pods
              value: 1
              periodSeconds: 300             # scaleupcooldown
          tolerance: "0.1"
        scaleDown:
          stabilizationWindowSeconds: 300    # scaledowncooldown
          selectPolicy: Max
          policies:
            - type: Pods
              value: 1
              periodSeconds: 300             # scaledowncooldown
          tolerance: "0.1"
    scalingModifiers:
      target: "1"
      metricType: Value
      formula: >-
        (readiness_gate == 1 && vllm_num_requests_waiting > 10 && vllm_request_queue_time_seconds > 30) ? 2.0 :
        ((vllm_num_requests_waiting < 1 && vllm_request_queue_time_seconds < 1) ? 0.5 : 1.0)
  triggers:
    # Metric 0: gauge -> service-avg
    - type: external
      name: vllm_num_requests_waiting
      metricType: Value
      authenticationRef:
        name: keda-kaito-scaler-creds
        kind: ClusterTriggerAuthentication
      metadata:
        inferenceSetName: phi-4
        inferenceSetNamespace: default
        scalerAddress: keda-kaito-scaler.keda-kaito-scaler.svc.cluster.local:9443
        metricName: vllm:num_requests_waiting
        metricSource: modelpod
        aggregation: service-avg
    # Metric 1: histogram -> windowed-avg (average over the metric cache window)
    - type: external
      name: vllm_request_queue_time_seconds
      metricType: Value
      authenticationRef:
        name: keda-kaito-scaler-creds
        kind: ClusterTriggerAuthentication
      metadata:
        inferenceSetName: phi-4
        inferenceSetNamespace: default
        scalerAddress: keda-kaito-scaler.keda-kaito-scaler.svc.cluster.local:9443
        metricName: vllm:request_queue_time_seconds
        metricSource: modelpod
        aggregation: windowed-avg
        metricCacheWindow: "300"
    # Readiness gate: no scrape, reports 1 once all replicas are ready, 0 otherwise
    - type: external
      name: readiness_gate
      metricType: Value
      authenticationRef:
        name: keda-kaito-scaler-creds
        kind: ClusterTriggerAuthentication
      metadata:
        inferenceSetName: phi-4
        inferenceSetNamespace: default
        scalerAddress: keda-kaito-scaler.keda-kaito-scaler.svc.cluster.local:9443
        metricName: readiness_gate
        aggregation: gate
```

</details>

> **How the `scalingModifiers.formula` works.** KEDA evaluates all trigger
> values in one expression and multiplies the current replica count by its
> result (HPA `metricType: Value`, `target: "1"` → `desired = ceil(current ×
> result)`). The formula is a nested ternary:
>
> ```text
> (readiness_gate == 1 && <every metric above its upthreshold>)   ? 2.0   // scale up
>   : (<every metric below its downthreshold>                     ? 0.5   // scale down
>                                                                 : 1.0)  // hold
> ```
>
> - **Scale up → `2.0`** fires only when `readiness_gate == 1` (all current
>   replicas are Ready, so we don't pile on more while pods are still warming up)
>   **and** *every* metric is above its `upthreshold`. The `2.0` asks HPA to
>   double, but the `scaleUp` behavior policy caps the real move at **+1 replica**
>   per `scaleupcooldown`.
> - **Scale down → `0.5`** fires when *every* metric is below its `downthreshold`.
>   The `0.5` asks HPA to halve, capped at **−1 replica** per `scaledowncooldown`.
> - **Hold → `1.0`** is the default for any mixed or in-between state; multiplying
>   by 1 leaves the replica count unchanged.
>
> This is the AND policy: a single metric can *veto* a scale up (if it's below its
> up-threshold) or a scale down (if it's above its down-threshold). Because each
> metric is compared against a *fixed* threshold, its value must be
> **replica-count independent** — that's why `gauge` is averaged per replica and
> `histogram` is averaged over its metric cache window.

#### Option 2: Manage the ScaledObject yourself

If you prefer to author the `ScaledObject` directly (e.g. to plug in custom
scaling policies or additional triggers), point an `external` trigger at the
keda-kaito-scaler service and reuse the chart-installed
`ClusterTriggerAuthentication` for mTLS.

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: phi-4
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: kaito.sh/v1beta1
    kind: InferenceSet
    name: phi-4
  minReplicaCount: 1
  maxReplicaCount: 5
  triggers:
    - type: external
      name: keda-kaito-scaler
      metricType: AverageValue
      authenticationRef:
        kind: ClusterTriggerAuthentication
        name: keda-kaito-scaler-creds
      metadata:
        # Required
        scalerAddress: "keda-kaito-scaler-svc.keda.svc.cluster.local:10450"
        inferenceSetName: phi-4
        inferenceSetNamespace: default
        metricName: "vllm:num_requests_waiting"
        threshold: "10"

        # Optional — defaults shown below match Kaito's current vLLM exposure
        # (Workspace ClusterIP Service on port 80, plain HTTP). Override only
        # if you have customized the inference server.
        # metricProtocol: "http"     # http | https
        # metricPort: "80"
        # metricPath: "/metrics"
        # scrapeTimeout: "3s"
```

The trigger metadata fields map 1:1 to the scaler's gRPC payload:

| Field | Required | Default | Notes |
| --- | --- | --- | --- |
| `scalerAddress` | yes | – | Used by KEDA to dial the scaler. Format: `keda-kaito-scaler-svc.<scaler-namespace>.svc.cluster.local:10450`. |
| `inferenceSetName` | yes | – | Target `InferenceSet` name. |
| `inferenceSetNamespace` | yes | – | Target `InferenceSet` namespace. May differ from the `ScaledObject` namespace; a single keda-kaito-scaler instance serves all namespaces in the cluster. |
| `metricName` | yes | – | Prometheus metric family name exposed by each workspace pod. |
| `threshold` | yes | – | Per-replica target (float). HPA: `desired = ceil(sum / threshold)`. |
| `metricProtocol` | no | `http` | `http` or `https`. |
| `metricPort` | no | `80` | Workspace `Service` port. |
| `metricPath` | no | `/metrics` | HTTP path of the Prometheus endpoint. |
| `scrapeTimeout` | no | `3s` | Per-service scrape timeout (Go duration). |

> **metricType must be `AverageValue`.** The scaler returns the cluster-wide
> sum of the metric, so HPA divides by `threshold` (per-replica target) to get
> the desired replica count. Using `Value` would couple desired replicas to the
> current replica count and break scale-from/to-1 transitions.

That's it! Your KAITO workloads will now automatically scale based on the
configured metric (default: `vllm:num_requests_waiting`).

## Release Process

Releases are driven by two manual GitHub Actions workflows. A release produces a multi-arch container image (`ghcr.io/kaito-project/keda-kaito-scaler:<X.Y.Z>`), a Helm chart (`https://kaito-project.github.io/keda-kaito-scaler/charts/kaito-project`), and a GitHub Release with binaries and changelog.

To publish `vX.Y.Z`:

1. Open a PR against `main` that bumps the version in:
   - [`charts/keda-kaito-scaler/Chart.yaml`](charts/keda-kaito-scaler/Chart.yaml) — `version` and `appVersion`
   - [`charts/keda-kaito-scaler/values.yaml`](charts/keda-kaito-scaler/values.yaml) — `image.tag`
   - [`Makefile`](Makefile) — `VERSION ?=` (optional, keeps local-build default aligned)

2. After the PR is merged, run **Actions → "Publish Keda-Kaito-Scaler image(manually)"** with `release_version=vX.Y.Z`. This creates the Git tag, pushes the image, and auto-publishes the Helm chart to `gh-pages`.

3. Run **Actions → "Create release(manually)"** with the same `release_version`. This runs GoReleaser against the tag and publishes the GitHub Release.

Notes:

- Use the same `vX.Y.Z` value for both workflows. Git tags / Release names are prefixed with `v`; image tags are not (`0.3.0`).
- Step 2 must finish before Step 3 (Step 3 checks out the tag created by Step 2).
- The image workflow runs in the `preset-env` environment and may require approval.
- Release branches are not needed for normal releases. Only cut a `release-vX.Y` branch (e.g. `release-v0.3`) when `main` has moved on to the next minor and you still need to ship patch releases for the older line; then run the publish workflows against that branch.

## License

This project is licensed under the Apache 2.0 License - see the [LICENSE](LICENSE) file for details.

## Related Projects

- [Kaito](https://github.com/kaito-project/kaito) - Kubernetes AI Toolchain Operator
- [KEDA](https://github.com/kedacore/keda) - Kubernetes Event-driven Autoscaling
- [vLLM](https://github.com/vllm-project/vllm) - Fast and easy-to-use library for LLM inference
