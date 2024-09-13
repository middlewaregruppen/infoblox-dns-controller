[![Go](https://github.com/middlewaregruppen/infoblox-dns-controller/actions/workflows/go.yaml/badge.svg)](https://github.com/middlewaregruppen/infoblox-dns-controller/actions/workflows/go.yaml) [![Release](https://github.com/middlewaregruppen/infoblox-dns-controller/actions/workflows/release.yaml/badge.svg)](https://github.com/middlewaregruppen/infoblox-dns-controller/actions/workflows/release.yaml)

# Infoblox DNS Controller

Manage Host records in Infoblox DNS through Kubernetes Ingresses

## Description

Infoblox DNS controller allows for automated DNS management using the native Kubernete Ingress resource. The controller reconciles host records in Infoblox DNS using host rules in Ingresses. The IPv4 address used in the Host record is whatever IP provided in the Ingress status field.

## Getting Started

You’ll need a Kubernetes cluster to run against. You can use [KIND](https://sigs.k8s.io/kind) to get a local cluster for testing, or run against a remote cluster.
**Note:** Your controller will automatically use the current context in your kubeconfig file (i.e. whatever cluster `kubectl cluster-info` shows).

### Running on the cluster

1. Install the controller using the provided kustomizations

```sh
kubectl apply -k config/default
```

2. Create a Secret containing credentials to Infoblox DNS

```sh
kubectl create secret generic infoblox-server-credentials \
    --from-literal INFOBLOX_USERNAME=<username> \
    --from-literal INFOBLOX_PASSWORD=<password> \
    --from-literal INFOBLOX_SERVER=<server> \
    --from-literal INFOBLOX_VIEW=<netview> \
    --from-literal INFOBLOX_ZONE=<dnsview>
```

3. Annotate your Ingress to have it managed by the controller

```
kubectl annotate ingress <your ingress> infoblox-dns-controller/manage=true
```

## Contributing

You are welcome to contribute to this project by opening PR's. Create an Issue if you have feedback

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
