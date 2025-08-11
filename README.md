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

- Kubernetes cluster with KEDA installed
- Kaito workspace with vLLM inference workloads
- RBAC permissions for the scaler to access Kaito resources

### Installation

1. **Deploy the KEDA Kaito Scaler**:

   ```bash
   kubectl apply -f https://raw.githubusercontent.com/kaito-project/keda-kaito-scaler/main/deploy/keda-kaito-scaler.yaml
   ```

2. **Create a ScaledObject for your Kaito Workspace**:

   ```yaml
   apiVersion: keda.sh/v1alpha1
   kind: ScaledObject
   metadata:
     name: kaito-vllm-workspace-scaler
     namespace: kaito-workloads
   spec:
     scaleTargetRef:
       apiVersion: kaito.sh/v1alpha1
       kind: Workspace
       name: my-vllm-workspace
     
     minReplicas: 1
     maxReplicas: 10
     
     triggers:
     - type: external
       metadata:
         scalerName: keda-kaito-scaler
         threshold: "10"
   ```

3. **Apply the configuration**:

   ```bash
   kubectl apply -f scaledobject.yaml
   ```

That's it! Your Kaito workspace will now automatically scale based on the number of waiting inference request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Related Projects

- [Kaito](https://github.com/kaito-project/kaito) - Kubernetes AI Toolchain Operator
- [KEDA](https://github.com/kedacore/keda) - Kubernetes Event-driven Autoscaling
- [vLLM](https://github.com/vllm-project/vllm) - Fast and easy-to-use library for LLM inference
