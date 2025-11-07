# Keda-Kaito-Scaler Helm Charts

[![License](https://img.shields.io/badge/license-Apache%202-4EB1BA.svg)](https://www.apache.org/licenses/LICENSE-2.0.html)
[![Go Report Card](https://goreportcard.com/report/github.com/kaito-project/keda-kaito-scaler)](https://goreportcard.com/report/github.com/kaito-project/keda-kaito-scaler)

## Usage

[Helm](https://helm.sh) must be installed to use the charts.
Please refer to Helm's [documentation](https://helm.sh/docs/) to get started.

Once Helm is set up properly, add the repo as follows:

```console
helm repo add keda-kaito-scaler https://kaito-project.github.io/keda-kaito-scaler
```

If you have already added this repo earlier, run `helm repo update` to retrieve the latest versions of the packages.
You can then run `helm search repo keda-kaito-scaler` to see the charts.

### Install `keda-kaito-scaler`

```shell
helm upgrade --install keda-kaito-scaler -n kaito-workspace kaito-project/keda-kaito-scaler --create-namespace
```
