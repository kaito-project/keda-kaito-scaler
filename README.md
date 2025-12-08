# KEDA Kaito Scaler

A specialized KEDA external scaler for automatically scaling GPU inference workloads in [Kaito](https://github.com/kaito-project/kaito) without requiring external dependencies like Prometheus.

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

## Quick Start

### Prerequisites

- install KEDA on a Kubernetes cluster
   - follow guide [here](https://github.com/kedacore/keda?tab=readme-ov-file#deploying-keda)

### Deploy KEDA Kaito Scaler on your cluster

   ```bash
   helm repo add keda-kaito-scaler https://kaito-project.github.io/keda-kaito-scaler/charts/kaito-project
   helm upgrade --install keda-kaito-scaler -n kaito-workspace keda-kaito-scaler/keda-kaito-scaler --create-namespace
   ```

### Create a Kaito InferenceSet for your inference workloads
 - the following example creates an inference service for the phi-4-mini model, annotations with the prefix `scaledobject.kaito.sh/` are used to provide parameter inputs for the KEDA Kaito Scaler.

```bash
cat <<EOF | kubectl apply -f -
apiVersion: kaito.sh/v1alpha1
kind: InferenceSet
metadata:
  annotations:
    scaledobject.kaito.sh/auto-provision: "true"
    scaledobject.kaito.sh/max-replicas: "5"
    scaledobject.kaito.sh/threshold: "10"
  name: phi-4
  namespace: default
spec:
  labelSelector:
    matchLabels:
      apps: phi-4
  nodeCountLimit: 10
  replicas: 1
  template:
    inference:
      preset:
        accessMode: public
        name: phi-4-mini-instruct
    resource:
      instanceType: Standard_NC24ads_A100_v4
EOF
```

That's it! Your Kaito workspace will now automatically scale based on the number of waiting inference request(`vllm:num_requests_waiting`).

## License

This project is licensed under the Apache 2.0 License - see the [LICENSE](LICENSE) file for details.

## Related Projects

- [Kaito](https://github.com/kaito-project/kaito) - Kubernetes AI Toolchain Operator
- [KEDA](https://github.com/kedacore/keda) - Kubernetes Event-driven Autoscaling
- [vLLM](https://github.com/vllm-project/vllm) - Fast and easy-to-use library for LLM inference
