# macOS Virtualization Kubelet

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
![Platform](https://img.shields.io/badge/platform-macOS%20%C2%B7%20Apple%20Silicon-lightgrey.svg)

**Run native macOS virtual machines as Kubernetes pods.**

`macOS-vz-kubelet` is a [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) provider that
turns an Apple Silicon Mac into a Kubernetes node. Each pod's first container boots as a native macOS VM on
Apple's [Virtualization framework](https://developer.apple.com/documentation/virtualization); optional Docker
side-car containers run in the same pod for logging, monitoring, or artifact handling.

It targets teams scaling macOS CI/CD (GitLab runners, build and test farms) on a cluster, at near-native Apple
Silicon performance and without QEMU/KVM.

## Contents

- [Overview](#overview)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Getting started](#getting-started)
- [Networking](#networking)
- [Imaging](#imaging)
- [Feature overview](#feature-overview)
- [Configuration](#configuration)
- [Examples](#examples)
- [Roadmap](#roadmap)
- [Contributing](#contributing)
- [Related projects](#related-projects)
- [License](#license)

## Overview

Running macOS under Kubernetes traditionally means QEMU/KVM on Linux, which cannot fully use Apple Silicon
hardware. `macOS-vz-kubelet` instead runs VMs natively through Virtualization.framework on the Mac itself, so
guests run at near-native speed.

The project is designed to:

- Run macOS VMs as first-class pods in a Kubernetes cluster.
- Support hybrid pods that combine a macOS VM with Docker side-car containers.
- Manage VM images with a custom, compressed, OCI-compliant format.
- Integrate with Kubernetes scheduling, resource requests, and lifecycle.

For exactly what is and is not supported, see the [Feature overview](#feature-overview).

## How it works

The kubelet runs on the Mac host as a Virtual Kubelet provider and orchestrates two backends from one pod
spec.

- **Virtualization framework** provisions and runs the macOS VM (pod container index 0), directly on Apple
  Silicon.
- **Custom OCI image format** distributes VM images. Images are built with Virtualization.framework-compatible
  tooling and pushed/pulled as compressed OCI artifacts; the kubelet pulls them via the
  [oras-go](https://github.com/oras-project/oras-go) library. Packaging/push uses a dedicated ORAS CLI fork,
  [oras-macos-vz](https://github.com/agoda-com/oras-macos-vz).
- **Hybrid pods.** Container 0 is always the macOS VM. Containers 1..N run on the local Docker daemon as
  regular containers.
- **Networking** is wired automatically: local NAT by default, or bridged for routable IPs. See
  [Networking](#networking).
- **Resource and lifecycle.** CPU/memory requests size the VM. Pod create and delete are supported; updates
  require recreating the pod.

Typical flow:

1. A pod manifest references a macOS VM image in the custom OCI format.
2. The kubelet pulls the image from the registry (cached locally, see [Imaging](#imaging)).
3. The VM boots; any side-car containers start alongside it.
4. The kubelet reports pod status, IPs, and lifecycle back to the control plane.

## Requirements

- Apple Silicon Mac (M-series). The binary builds for `darwin/arm64` only.
- A macOS host with Virtualization.framework support.
- A Kubernetes cluster to join the node to, plus a client certificate and key the kubelet uses to
  authenticate to the API server.
- A macOS VM base image in the custom OCI format, pushed to an OCI registry (see [Imaging](#imaging)).
- SSH enabled inside the VM image. `kubectl exec`/`attach` into the VM, the readiness probe, postStart hooks,
  and stats all run over SSH.
- A Docker daemon on the host (for example Colima) if you run side-car containers.
- Code signing with Virtualization.framework entitlements. Ad-hoc signing is enough for NAT; bridged
  networking needs Apple-approved vmnet entitlements (see [Networking](#networking)).

## Getting started

### Build and sign

Tooling versions are pinned in `.mise.toml` (Go, goreleaser, Ruby). Builds run on macOS and produce a
`MacOSVK.app` bundle.

```shell
make snapshot   # local development build
make release    # signed release build (set RELEASE_CERTIFICATE_NAME, RELEASE_PROVISION_PROFILE_PATH)
```

The binary needs Virtualization.framework entitlements. To ad-hoc sign a locally built binary:

```shell
codesign --entitlements resources/vz.entitlements -s - <YOUR BINARY PATH>
```

Bridged networking requires the additional vmnet entitlements in `resources/release.entitlements`, which need
Apple approval. See [Networking](#networking).

### Run a workload

1. **Create a base VM image.** Use a Virtualization.framework-compatible tool such as
   [macosvm](https://github.com/s-u/macosvm).
2. **Package and push it** in the custom OCI format with
   [oras-macos-vz](https://github.com/agoda-com/oras-macos-vz):

   ```shell
   oras-macos-vz push -h
   ```

3. **Write a pod manifest** that references the OCI image. See [Examples](#examples).
4. **Run the pod.** The kubelet picks it up, pulls the image, and boots the VM. Status is reported back as for
   a regular container.
5. **Interact** with `kubectl exec` (and `attach`) into the VM or any side-car container.

Configure the kubelet with the [flags](#command-line-flags) and [environment variables](#environment-variables)
below.

## Networking

Networking runs in one of two modes.

### NAT (default)

- The VM gets a local IP via NAT.
- Fits most cases where external reachability is unnecessary.
- `kubectl exec`/`attach` reach the VM over SSH on its local IP. The VM IP is discovered from the host ARP
  table.

### Bridged

- Gives the VM a routable IP by attaching it to a host network interface (typically a tagged VLAN with DHCP).
- Each VM interface gets a generated MAC so its lease can be tracked.
- The kubelet discovers the VM IP by sniffing for that MAC with libpcap (tcpdump-style capture) and reports it
  to Kubernetes.

Enable bridged mode by setting `VZ_BRIDGE_INTERFACE` to the host interface name. Bridged mode requires the
VMNet and VM Networking capabilities, which need Apple approval; see this
[Apple Developer Forums thread](https://developer.apple.com/forums/thread/656411) for how to request them.
After approval, generate a Mac Development certificate, an App ID with those capabilities, and a provisioning
profile, then build a release binary signed with `resources/release.entitlements`.

## Imaging

VM images use a custom, OCI-compliant format. See the [OCI manifest example](example/oci_manifest.json).

### Compression

macOS images are large (tens of gigabytes with Xcode and simulators preinstalled). The packaging step
compresses image layers (parallel gzip) so they store and transfer at a fraction of the raw size, and the
kubelet decompresses on pull.

### Copy-on-write overlays

A host runs at most two VMs at once (`MaxVirtualMachines`, enforced by a semaphore; a third pod waits for a
free slot). This reflects a Virtualization.framework limit. To let concurrent VMs share one base image while
keeping independent state, each pod gets a copy-on-write
[clone](https://github.com/apple/darwin-xnu/blob/main/bsd/sys/clonefile.h) of the disk and auxiliary storage.
Overlays are removed when the VM stops; the base image is never mutated.

### Digest validation

Local image files carry a recorded digest to guarantee integrity:

- On first download, a digest is computed and stored alongside the image file.
- On every pod start, the digest is checked against the expected digest from the registry manifest.
- If the digest file is missing or is not newer than the image file, the digest is recomputed from the image;
  a mismatch invalidates the local cache and the image is re-pulled.

## Feature overview

The tables below list supported Kubernetes features. Anything not listed is unsupported.

### Node

| Feature                  | Supported | Comments          |
|--------------------------|:---------:|-------------------|
| Node addresses           | ✅        |                   |
| Node capacity            | ✅        |                   |
| Node daemon endpoints    | ✅        |                   |
| Operating system         | ✅        | Darwin macOS only |

### Pod

| Feature                          | Supported | Comments                                                                                              |
|----------------------------------|:---------:|------------------------------------------------------------------------------------------------------|
| Create and delete pods           | ✅        |                                                                                                      |
| Update pods                      | ❌        | Recreate the pod instead.                                                                             |
| Get pod, pods, and pod status    | ✅        |                                                                                                      |
| Security policies                | ❌        |                                                                                                      |
| Init containers                  | ❌        | On the short list.                                                                                   |
| Regular containers               | ✅        | Container 0 must be the macOS VM; every later container runs on the Docker daemon.                    |

### Containers

| Feature                              | Supported | Comments                                                                                                                          |
|--------------------------------------|:---------:|----------------------------------------------------------------------------------------------------------------------------------|
| Container logs                       | ⚠️        | Docker side-cars only.                                                                                                            |
| Container exec                       | ✅        | VM exec needs `VZ_SSH_USER` plus one of `VZ_SSH_PRIVATE_KEY_BASE64` / `VZ_SSH_PRIVATE_KEY_PATH` / `VZ_SSH_PASSWORD`. Side-car exec works by default. |
| Container attach                     | ✅        | Docker attach; macOS VM via an interactive SSH shell.                                                                             |
| Container metrics                    | ✅        | Both backends: macOS VM (CPU and memory) and Docker side-cars.                                                                    |
| Resource requests                    | ⚠️        | Size the macOS VM. Docker containers ignore them.                                                                                 |
| Resource limits                      | ❌        | Ignored; VMs are fixed-size.                                                                                                      |
| Liveness / readiness / startup probes | ❌       | Pod-spec probes are not evaluated. See the readiness note below.                                                                  |
| Lifecycle hooks (postStart, preStop) | ⚠️        | Exec-shaped hooks only. A successful postStart hook gates pod Ready; preStop runs within the grace period.                        |

**Readiness gating.** Pod-spec probes are not evaluated, but every macOS pod is held NotReady until an internal
SSH readiness check succeeds (the guest sshd must answer), bounded by `VZ_SSH_READINESS_TIMEOUT`. If a pod
defines an exec postStart hook, Ready is gated on that hook finishing successfully (bounded by
`VZ_POSTSTART_TIMEOUT`); a permanently unreachable VM fails the pod rather than hanging.

### Storage

| Feature                  | Supported | Comments                                   |
|--------------------------|:---------:|--------------------------------------------|
| Host volumes             | ✅        |                                            |
| Empty dir volumes        | ✅        | `sizeLimit` is not enforced.               |
| Persistent volumes       | ❌        | Unsupported by Virtual Kubelet in general. |
| Config map volumes       | ❌        | On the short list.                         |
| Secret volumes           | ❌        | On the short list.                         |
| Projected volumes        | ⚠️        | See below.                                 |

A [projected volume](https://kubernetes.io/docs/concepts/storage/projected-volumes) maps several volume sources
into one directory. Kubernetes adds one by default carrying the service account token, API server CA, and
namespace, so a pod can call the API server.

| Source                | Supported | Comments                                                       |
|-----------------------|:---------:|----------------------------------------------------------------|
| configMap             | ✅        |                                                                |
| serviceAccountToken   | ⚠️        | No rotation. Expires per Kubernetes default (~3607s).          |
| downwardAPI           | ⚠️        | `metadata.namespace` only.                                     |
| secret                | ❌        | Projected secret sources are not mounted.                      |
| clusterTrustBundle    | ❌        |                                                                |

### Image pull secrets

The kubelet resolves registry credentials from the pod's `spec.imagePullSecrets` and from the pod
ServiceAccount's `imagePullSecrets` in the same namespace.

Supported secret types:

- `kubernetes.io/dockerconfigjson` (preferred), requires `.dockerconfigjson` data.
- `kubernetes.io/dockercfg` (legacy), requires `.dockercfg` data.

Secrets of other types are ignored with a warning event. A referenced secret that is missing or invalid fails
pod creation.

## Configuration

### Command-line flags

All flags are optional. Duration flags take Go duration strings (for example `30s`, `1m`).

| Flag                                            | Type     | Default                          | Description                                              |
|-------------------------------------------------|----------|----------------------------------|----------------------------------------------------------|
| `--nodename`                                    | string   | host name                        | Node name in the cluster.                                |
| `--provider-id`                                 | string   | (empty)                          | Provider ID reported to the API server.                  |
| `--startup-timeout`                             | duration | `0`                              | How long to wait for the kubelet to start.               |
| `--disable-taint`                               | bool     | `false`                          | Disable the node taint.                                  |
| `--log-level`                                   | string   | `info`                           | Log level.                                               |
| `--pod-sync-workers`                            | int      | `10`                             | Number of pod synchronization workers.                   |
| `--full-resync-period`                          | duration | `1m`                             | Interval between full pod resyncs.                        |
| `--client-verify-ca`                            | string   | `$APISERVER_CA_CERT_LOCATION`    | CA cert used to verify client requests.                  |
| `--no-verify-clients`                           | bool     | `false`                          | Do not require client certificate validation.            |
| `--authentication-token-webhook`                | bool     | `false`                          | Use the TokenReview API for bearer-token authentication. |
| `--authentication-token-webhook-cache-ttl`      | duration | `0`                              | Cache TTL for webhook token authentication responses.    |
| `--authorization-webhook-cache-authorized-ttl`  | duration | `0`                              | Cache TTL for authorized webhook responses.              |
| `--authorization-webhook-cache-unauthorized-ttl`| duration | `0`                              | Cache TTL for unauthorized webhook responses.            |
| `--trace-sample-rate`                           | string   | always sample                    | Trace sampling probability.                              |

Standard klog logging flags are also available under a `--klog.` prefix (for example `--klog.v`).

### Environment variables

#### Kubernetes connection

| Variable                     | Required | Default          | Description                                                       |
|------------------------------|:--------:|------------------|-------------------------------------------------------------------|
| `KUBECONFIG`                 |          | `~/.kube/config` | Path to the kubeconfig.                                            |
| `APISERVER_CERT_LOCATION`    | ✓        |                  | Client cert for authenticating to the API server.                 |
| `APISERVER_KEY_LOCATION`     | ✓        |                  | Client key for authenticating to the API server.                  |
| `APISERVER_CA_CERT_LOCATION` |          |                  | CA cert to verify the API server (default for `--client-verify-ca`). |
| `KUBELET_PORT`               |          | `10250`          | Kubelet API listen port.                                          |
| `VKUBELET_POD_IP`            |          |                  | Pod IP for the kubelet. For debugging.                            |
| `VKUBELET_TAINT_KEY`         |          | `virtual-kubelet.io/provider` | Node taint key.                                      |
| `VKUBELET_TAINT_EFFECT`      |          | `NoSchedule`     | Node taint effect.                                                |
| `VKUBELET_TAINT_VALUE`       |          | `macos-vz`       | Node taint value.                                                 |

#### macOS VM access (SSH)

Used for `kubectl exec`/`attach` into the VM, the readiness probe, postStart hooks, stats, and graceful
shutdown. Provide one of the key variables or `VZ_SSH_PASSWORD`.

| Variable                        | Required | Default           | Description                                                                 |
|---------------------------------|:--------:|-------------------|-----------------------------------------------------------------------------|
| `VZ_SSH_USER`                   | ✓        |                   | SSH username on the macOS VM.                                               |
| `VZ_SSH_PRIVATE_KEY_BASE64`     |          |                   | Base64-encoded PEM private key. Preferred when set.                         |
| `VZ_SSH_PRIVATE_KEY_PATH`       |          |                   | Private key file path. Used when `VZ_SSH_PRIVATE_KEY_BASE64` is unset.      |
| `VZ_SSH_PRIVATE_KEY_PASSPHRASE` |          |                   | Passphrase for the private key.                                            |
| `VZ_SSH_PASSWORD`               |          |                   | Password authentication (fallback when no key is set).                      |
| `VZ_SSH_KEX_ALGORITHMS`         |          | Go SSH defaults   | Comma-separated KEX algorithm override (advanced troubleshooting).          |
| `VZ_SSH_DIAL_TIMEOUT`           |          | `5s`              | Cap on TCP connect plus SSH handshake. Keeps `exec`/`attach` off an unreachable VM. |
| `VZ_SSH_READINESS_TIMEOUT`      |          | `60s`             | Overall cap on the post-start SSH readiness loop. On expiry the pod fails.  |
| `VZ_POSTSTART_TIMEOUT`          |          | `10s`             | Timeout for the postStart hook exec only.                                   |
| `TERM`                          |          | `xterm-256color`  | Terminal type for interactive `exec`/`attach`.                             |

#### Networking and side-cars

| Variable              | Required | Default                | Description                                                              |
|-----------------------|:--------:|------------------------|--------------------------------------------------------------------------|
| `VZ_BRIDGE_INTERFACE` |          | (empty, NAT)           | Host interface for bridged networking. Requires vmnet entitlements.      |
| `DOCKER_HOST`         |          | Docker SDK auto-detect | Docker daemon address for side-car containers.                           |

#### Tracing (OpenTelemetry)

| Variable                      | Required | Default          | Description                                |
|-------------------------------|:--------:|------------------|--------------------------------------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` |          | tracing disabled | OTLP trace endpoint.                       |
| `OTEL_EXPORTER_OTLP_INSECURE` |          | `false`          | Disable TLS when exporting traces.         |
| `OTEL_SERVICE_NAME`           |          |                  | Service name reported with traces.         |

### Local cache

The cache directory is `~/Library/Caches/com.agoda.fleet.virtualization`. It holds OCI images and their digest
files, plus pod mount volumes when `emptyDir` volumes are used.

## Examples

The [example](example) directory has ready-to-adapt manifests:

- [`pod.yml`](example/pod.yml) - a minimal macOS VM pod.
- [`pod-gitlab-sidecar.yml`](example/pod-gitlab-sidecar.yml) - a hybrid pod with a macOS VM plus a GitLab
  Runner Docker side-car.
- [`deployment.yml`](example/deployment.yml) - a Deployment of macOS VM pods.
- [`oci_manifest.json`](example/oci_manifest.json) and [`config.json`](example/config.json) - the custom OCI
  image format.

## Roadmap

- Higher test coverage, including an open end-to-end testing approach.
- A public CI/CD pipeline to make contribution easier.

## Contributing

Issues and contributions are welcome on the
[GitHub Issues](https://github.com/agoda-com/macOS-vz-kubelet/issues) page. See [CONTRIBUTING.md](CONTRIBUTING.md)
for development and pull request guidance.

## Related projects

These projects are the foundation `macOS-vz-kubelet` is built on:

- [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) - the Kubernetes kubelet
  implementation this provider plugs into.
- [Code-Hex/vz](https://github.com/Code-Hex/vz) - Go bindings for Apple Virtualization.framework.
- [oras-project/oras-go](https://github.com/oras-project/oras-go) - ORAS Go library for OCI artifacts.
- [oras-macos-vz](https://github.com/agoda-com/oras-macos-vz) - the ORAS CLI fork used to package and push VM
  images.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
