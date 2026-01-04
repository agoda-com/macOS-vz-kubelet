# CLAUDE.md — macOS-vz-kubelet Extension Guide

## Kubernetes-Native macOS VM Orchestration with CI/CD, iOS Device Testing, and Fleet Management

This development guide provides architecture decisions, implementation steps, and a phased roadmap for extending Agoda's macOS-vz-kubelet with Tart image support, iOS device brokering, and Mac host fleet management.

---

## Project overview and strategic context

The macOS-vz-kubelet transforms macOS hosts into Kubernetes nodes using Apple's Virtualization.framework, enabling native macOS VM orchestration with near-bare-metal performance. This extension adds:

- **Tart OCI image compatibility** — Use pre-built Cirrus Labs images
- **iOS device brokering** — Physical device testing via K8s CRDs
- **Mac host fleet management** — Manage hosts themselves as K8s resources

**Core constraints to understand upfront:**

| Constraint | Implication |
|------------|-------------|
| 2 macOS VMs per host max | Apple Virtualization.framework hard limit |
| No iOS USB passthrough to VMs | Devices must connect to host directly |
| VMNet requires Apple entitlement | NAT networking for MVP (sufficient for CI/CD) |

### Target use cases

- CI/CD pipelines for iOS/macOS app builds
- iOS Simulator testing at scale (fully supported in VMs)
- Physical iOS device testing (via host-level device broker)
- Hybrid workloads combining macOS VMs with Docker sidecars
- Fleet-wide Mac host management (updates, restarts, maintenance)

---

## Architecture overview

```
┌──────────────────────────────────────────────────────────────────────────┐
│  Kubernetes Control Plane                                                 │
│                                                                           │
│  ┌───────────┐ ┌───────────┐ ┌─────────────┐ ┌─────────────────────────┐ │
│  │ MacHost   │ │ iOSDevice │ │ iOSDevice   │ │ MacHostOperation        │ │
│  │ CRD       │ │ CRD       │ │ Claim CRD   │ │ CRD                     │ │
│  └─────┬─────┘ └─────┬─────┘ └──────┬──────┘ └────────────┬────────────┘ │
│        │             │              │                     │              │
│  ┌─────┴─────────────┴──────────────┴─────────────────────┴────────────┐ │
│  │                    Controllers (in-cluster)                         │ │
│  │  - Device Claim Controller (binds devices to pods)                  │ │
│  │  - Fleet Controller (executes host operations)                      │ │
│  └──────────────────────────────────┬──────────────────────────────────┘ │
└─────────────────────────────────────┼────────────────────────────────────┘
                                      │ gRPC / mTLS
                                      ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  Mac Host (Apple Silicon)                                                 │
│                                                                           │
│  ┌──────────────────────────────────────────────────────────────────────┐│
│  │  Unified Host Agent                                                  ││
│  │  ┌────────────────┐  ┌────────────────┐  ┌────────────────────────┐ ││
│  │  │ VM Provider    │  │ Device Agent   │  │ Fleet Agent            │ ││
│  │  │ (vz-kubelet)   │  │ (go-ios)       │  │ (system ops)           │ ││
│  │  │                │  │                │  │                        │ ││
│  │  │ • Pod lifecycle│  │ • USB detection│  │ • Brew updates         │ ││
│  │  │ • Image pull   │  │ • WDA lifecycle│  │ • Xcode installs       │ ││
│  │  │ • kubectl exec │  │ • Device CRDs  │  │ • System restarts      │ ││
│  │  └────────────────┘  └────────────────┘  └────────────────────────┘ ││
│  └──────────────────────────────────────────────────────────────────────┘│
│                                                                           │
│  ┌─────────────┐ ┌─────────────┐    ┌─────────┐ ┌─────────┐ ┌─────────┐ │
│  │  macOS VM   │ │  macOS VM   │    │ iPhone  │ │ iPhone  │ │  iPad   │ │
│  │  (builds)   │ │  (sims)     │    │   14    │ │   15    │ │  Pro    │ │
│  └─────────────┘ └─────────────┘    └────┬────┘ └────┬────┘ └────┬────┘ │
│                                          │ USB       │ USB       │ USB  │
│  ════════════════════════════════════════╧═══════════╧═══════════╧════  │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Architecture decisions

### Decision 1: Native Tart image support via LZ4 decompression

**Chosen approach:** Extend the existing OCI store to natively support Tart images by adding LZ4 decompression and Tart media type handling. No external Tart CLI dependency.

**Rationale:** Tart images are standard OCI artifacts hosted on ghcr.io — the same registry protocol ORAS already speaks. The only differences are compression format and media types:

| Aspect | vz-kubelet (oras-macos-vz) | Tart (Cirrus Labs) |
|--------|---------------------------|---------------------|
| Compression | GZIP (pgzip) | LZ4 (chunked frames) |
| Layer structure | Single compressed blob | Multiple LZ4 layers |
| Config media type | `application/vnd.agoda.macosvz.config.v1+json` | `application/vnd.cirruslabs.tart.config.v1` |
| Disk media type | `application/vnd.agoda.macosvz.disk.image.v1` | `application/vnd.cirruslabs.tart.disk.v2` |
| NVRAM media type | `application/vnd.agoda.macosvz.aux.image.v1` | `application/vnd.cirruslabs.tart.nvram.v1` |

Both produce identical VM artifacts (disk.img, nvram.bin). By extending the existing ORAS-based pipeline with LZ4 support, we avoid runtime dependencies and maintain a single-binary deployment.

**Key constraint:** Tart's LZ4 frames can span layer boundaries, requiring sequential (not parallel) decompression. ORAS handles the parallel download; we decompress sequentially after.

### Decision 2: NAT networking for MVP (no VMNet entitlement required)

**Chosen approach:** NAT mode via `VZNATNetworkDeviceAttachment`.

**Rationale:** VMNet entitlements require Apple approval (2-3+ weeks). NAT is sufficient for CI/CD:

- ✅ Internet access for builds
- ✅ Host-to-VM SSH via `kubectl exec`
- ✅ iOS Simulator testing
- ❌ No inbound external connections (acceptable for MVP)

### Decision 3: Direct host USB for iOS device testing

**Chosen approach:** iOS devices connect to macOS host directly, managed by device agent daemon.

**Rationale:** Apple's Virtualization.framework has **no iOS USB passthrough**—only emulated mass storage. Physical devices must connect to host, with WebDriverAgent exposing network API for test code.

### Decision 4: Treat Mac hosts as Kubernetes resources

**Chosen approach:** `MacHost` CRD represents each physical Mac, with `MacHostOperation` CRD for imperative actions.

**Rationale:** Enables fleet management via kubectl:
- Declarative desired state (packages, versions)
- Automated maintenance windows
- Audit trail via K8s events
- Consistent with how we manage VMs and devices

### Decision 5: Avoid Go-Rust FFI; use microservices if Rust needed

**Chosen approach:** Keep everything in Go. If Rust is required later, deploy as gRPC sidecar.

**Rationale:** CGO breaks cross-compilation and debugging. Go with Code-Hex/vz bindings handles all Virtualization.framework operations.

---

## Implementation roadmap

| Phase | Focus | Key Deliverables |
|-------|-------|------------------|
| 1 | Tart image support | Native LZ4 decompression, Tart media type detection, unified OCI store |
| 2 | iOS device broker | Device CRDs, agent DaemonSet, claim controller |
| 3 | Enhanced VM lifecycle | APFS CoW overlays, graceful shutdown, metrics |
| 4 | Mac host fleet management | MacHost CRD, fleet agent, operations controller |

---

## Phase 1: Native Tart Image Support

### Step 1.1: Add LZ4 dependency

```bash
go get github.com/pierrec/lz4/v4
```

### Step 1.2: Define Tart media types

Extend `pkg/oci/mediatype.go`:

```go
// Tart (Cirrus Labs) media types
const (
    TartConfigMediaType  = "application/vnd.cirruslabs.tart.config.v1"
    TartDiskMediaTypeV1  = "application/vnd.cirruslabs.tart.disk.v1"
    TartDiskMediaTypeV2  = "application/vnd.cirruslabs.tart.disk.v2"
    TartNVRAMMediaType   = "application/vnd.cirruslabs.tart.nvram.v1"
)

func IsTartMediaType(mediaType string) bool {
    return strings.HasPrefix(mediaType, "application/vnd.cirruslabs.tart")
}

func IsTartDiskLayer(mediaType string) bool {
    return mediaType == TartDiskMediaTypeV1 || mediaType == TartDiskMediaTypeV2
}
```

### Step 1.3: Implement LZ4 layer decompressor

Create `pkg/oci/lz4.go`:

```go
package oci

import (
    "io"
    "github.com/pierrec/lz4/v4"
    ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// DecompressTartDisk handles Tart's multi-layer LZ4 format.
// Layers must be processed sequentially as LZ4 frames can span boundaries.
func DecompressTartDisk(ctx context.Context, layers []ocispec.Descriptor, fetcher content.Fetcher, destPath string) error {
    dest, err := os.Create(destPath)
    if err != nil {
        return fmt.Errorf("create dest file: %w", err)
    }
    defer dest.Close()

    // Create LZ4 reader that will be fed concatenated layer data
    pr, pw := io.Pipe()
    lz4Reader := lz4.NewReader(pr)

    // Decompress in background
    var decompressErr error
    done := make(chan struct{})
    go func() {
        defer close(done)
        _, decompressErr = io.Copy(dest, lz4Reader)
    }()

    // Feed layers sequentially (order matters for LZ4 frame continuity)
    for _, layer := range layers {
        rc, err := fetcher.Fetch(ctx, layer)
        if err != nil {
            pw.CloseWithError(err)
            return fmt.Errorf("fetch layer %s: %w", layer.Digest, err)
        }
        if _, err := io.Copy(pw, rc); err != nil {
            rc.Close()
            pw.CloseWithError(err)
            return fmt.Errorf("copy layer %s: %w", layer.Digest, err)
        }
        rc.Close()
    }
    pw.Close()

    <-done
    return decompressErr
}
```

### Step 1.4: Extend OCI processor for format detection

Update `pkg/oci/oci_processor.go`:

```go
func (s *Store) processContent(ctx context.Context, desc ocispec.Descriptor, manifest ocispec.Manifest) error {
    // Detect image format from config media type
    if IsTartMediaType(manifest.Config.MediaType) {
        return s.processTartImage(ctx, manifest)
    }
    // Existing ORAS/vz-kubelet format
    return s.processOrasImage(ctx, manifest)
}

func (s *Store) processTartImage(ctx context.Context, manifest ocispec.Manifest) error {
    // Separate disk layers from NVRAM layer
    var diskLayers []ocispec.Descriptor
    var nvramLayer *ocispec.Descriptor

    for _, layer := range manifest.Layers {
        switch {
        case IsTartDiskLayer(layer.MediaType):
            diskLayers = append(diskLayers, layer)
        case layer.MediaType == TartNVRAMMediaType:
            nvramLayer = &layer
        }
    }

    // Decompress disk (sequential LZ4)
    diskPath := filepath.Join(s.rootPath, "disk.img")
    if err := DecompressTartDisk(ctx, diskLayers, s, diskPath); err != nil {
        return fmt.Errorf("decompress tart disk: %w", err)
    }

    // Decompress NVRAM (single LZ4 layer)
    if nvramLayer != nil {
        nvramPath := filepath.Join(s.rootPath, "nvram.bin")
        if err := decompressLZ4Layer(ctx, *nvramLayer, s, nvramPath); err != nil {
            return fmt.Errorf("decompress tart nvram: %w", err)
        }
    }

    // Parse Tart config for hardware model
    return s.processTartConfig(ctx, manifest.Config)
}
```

### Step 1.5: Map Tart config to VMImage

```go
// TartConfig matches Cirrus Labs config structure
type TartConfig struct {
    OS               string `json:"os"`
    Arch             string `json:"arch"`
    CPUCount         int    `json:"cpuCount"`
    MemorySize       uint64 `json:"memorySize"`
    HardwareModel    []byte `json:"hardwareModel"`    // Base64 in JSON
    MachineID        []byte `json:"ecid"`             // Machine identifier
    MacAddress       string `json:"macAddress"`
}

func (s *Store) processTartConfig(ctx context.Context, configDesc ocispec.Descriptor) error {
    rc, err := s.Fetch(ctx, configDesc)
    if err != nil {
        return err
    }
    defer rc.Close()

    var tartConfig TartConfig
    if err := json.NewDecoder(rc).Decode(&tartConfig); err != nil {
        return fmt.Errorf("decode tart config: %w", err)
    }

    // Map to our config format
    s.config = &Config{
        HardwareModel:     tartConfig.HardwareModel,
        MachineIdentifier: tartConfig.MachineID,
        SourceFormat:      "tart",
    }
    return s.writeConfig()
}
```

### Step 1.6: Integration test

```bash
# Pull Cirrus Labs image directly
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-tart
spec:
  nodeName: mac-node-01
  containers:
  - name: macos
    image: ghcr.io/cirruslabs/macos-sequoia-xcode:16
EOF

kubectl wait --for=condition=Ready pod/test-tart --timeout=600s
kubectl exec test-tart -- sw_vers
# Expected: macOS 15.x (Sequoia)
```

---

## Phase 2: iOS Device Broker

### Step 2.1: Define CRDs

```yaml
# iOSDevice - represents a physical device
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: iosdevices.devices.vz-kubelet.io
spec:
  group: devices.vz-kubelet.io
  names:
    kind: iOSDevice
    plural: iosdevices
    shortNames: [iosd]
  scope: Cluster
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              udid:
                type: string
              model:
                type: string
              osVersion:
                type: string
              providerNode:
                type: string
          status:
            type: object
            properties:
              phase:
                type: string
                enum: [Available, Reserved, InUse, Offline]
              wdaEndpoint:
                type: string
              batteryLevel:
                type: integer
```

```yaml
# iOSDeviceClaim - request for a device
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: iosdeviceclaims.devices.vz-kubelet.io
spec:
  group: devices.vz-kubelet.io
  names:
    kind: iOSDeviceClaim
    plural: iosdeviceclaims
    shortNames: [iosdc]
  scope: Namespaced
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              requirements:
                type: object
                properties:
                  modelPattern:
                    type: string
                  minOSVersion:
                    type: string
              duration:
                type: string
          status:
            type: object
            properties:
              phase:
                type: string
                enum: [Pending, Bound, Released]
              boundDevice:
                type: string
              wdaEndpoint:
                type: string
```

### Step 2.2: Device agent implementation

```go
// cmd/device-agent/main.go
package main

import (
    "github.com/danielpaulus/go-ios/ios"
)

type DeviceAgent struct {
    nodeName  string
    k8sClient client.Client
    devices   map[string]*ManagedDevice
}

func (a *DeviceAgent) Run(ctx context.Context) error {
    // Listen for USB events
    deviceChan, _ := ios.Listen()
    
    for {
        select {
        case event := <-deviceChan:
            switch event.MessageType {
            case "Attached":
                a.onDeviceAttached(ctx, event)
            case "Detached":
                a.onDeviceDetached(ctx, event.SerialNumber)
            }
        case <-ctx.Done():
            return nil
        }
    }
}

func (a *DeviceAgent) onDeviceAttached(ctx context.Context, event ios.DeviceAttachedEvent) {
    device, _ := ios.GetDevice(event.SerialNumber)
    
    // Create iOSDevice CR
    iosDevice := &v1alpha1.iOSDevice{
        ObjectMeta: metav1.ObjectMeta{
            Name: sanitize(device.SerialNumber),
        },
        Spec: v1alpha1.iOSDeviceSpec{
            UDID:         device.SerialNumber,
            Model:        device.ProductType,
            OSVersion:    device.ProductVersion,
            ProviderNode: a.nodeName,
        },
    }
    a.k8sClient.Create(ctx, iosDevice)
}
```

### Step 2.3: WebDriverAgent lifecycle

```go
func (a *DeviceAgent) startWDA(device *ManagedDevice) error {
    // Find available port
    port := a.allocatePort()
    
    // Start WDA using go-ios
    cmd := exec.Command("ios", "runwda",
        "--udid", device.UDID,
        "--port", strconv.Itoa(port),
    )
    cmd.Start()
    
    device.WDAPort = port
    device.WDAProcess = cmd.Process
    
    // Update device status with endpoint
    device.Status.WDAEndpoint = fmt.Sprintf("http://%s:%d", a.hostIP, port)
    return nil
}
```

---

## Phase 3: Enhanced VM Lifecycle

### Step 3.1: APFS copy-on-write overlays

```go
// pkg/vm/overlay.go
/*
#include <sys/clonefile.h>
*/
import "C"

func CreateOverlay(basePath, overlayPath string) error {
    ret := C.clonefile(
        C.CString(basePath),
        C.CString(overlayPath),
        C.CLONE_NOFOLLOW,
    )
    if ret != 0 {
        return copyFile(basePath, overlayPath) // Fallback
    }
    return nil
}
```

### Step 3.2: Graceful VM shutdown

```go
func (vm *Instance) GracefulStop(ctx context.Context, timeout time.Duration) error {
    // Send ACPI power button
    vm.VirtualMachine.RequestStop()
    
    deadline := time.After(timeout)
    for {
        select {
        case <-deadline:
            return vm.VirtualMachine.Stop() // Force
        default:
            if vm.VirtualMachine.State() == vz.VirtualMachineStateStopped {
                return nil
            }
            time.Sleep(500 * time.Millisecond)
        }
    }
}
```

### Step 3.3: Prometheus metrics

```go
var (
    VMsRunning = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "vz_kubelet_vms_running",
    })
    
    ImagePullDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "vz_kubelet_image_pull_seconds",
        Buckets: prometheus.ExponentialBuckets(10, 2, 10),
    }, []string{"format"})
    
    DevicesTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "vz_kubelet_ios_devices",
    }, []string{"phase"})
)
```

---

## Phase 4: Mac Host Fleet Management

### Overview

Treat Mac hosts as first-class Kubernetes resources, enabling fleet-wide management via `kubectl`:

```bash
# View all hosts
kubectl get machosts

# Check host details
kubectl describe machost mac-mini-07

# Trigger maintenance
kubectl apply -f restart-host.yaml

# Fleet-wide update
kubectl apply -f update-all-xcode.yaml
```

### Step 4.1: MacHost CRD

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: machosts.fleet.vz-kubelet.io
spec:
  group: fleet.vz-kubelet.io
  names:
    kind: MacHost
    plural: machosts
    shortNames: [mh]
  scope: Cluster
  versions:
  - name: v1alpha1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              desiredState:
                type: string
                enum: [active, maintenance, drain, shutdown]
                default: active
              packages:
                type: object
                properties:
                  homebrew:
                    type: array
                    items:
                      type: string
              xcode:
                type: object
                properties:
                  version:
                    type: string
                  installPath:
                    type: string
              maintenance:
                type: object
                properties:
                  schedule:
                    type: string
                    description: "Cron expression for maintenance window"
                  autoReboot:
                    type: boolean
                    default: false
                  maxUnavailable:
                    type: integer
                    default: 1
          status:
            type: object
            properties:
              phase:
                type: string
                enum: [Active, Maintenance, Draining, Offline, Error]
              systemInfo:
                type: object
                properties:
                  macosVersion:
                    type: string
                  xcodeVersion:
                    type: string
                  xcodePath:
                    type: string
                  chip:
                    type: string
                  serialNumber:
                    type: string
              resources:
                type: object
                properties:
                  cpuCores:
                    type: integer
                  memoryGB:
                    type: integer
                  diskTotalGB:
                    type: integer
                  diskFreeGB:
                    type: integer
              workloads:
                type: object
                properties:
                  vmsRunning:
                    type: integer
                  vmsMax:
                    type: integer
                    default: 2
                  devicesAttached:
                    type: integer
              packages:
                type: array
                items:
                  type: object
                  properties:
                    name:
                      type: string
                    version:
                      type: string
              conditions:
                type: array
                items:
                  type: object
                  properties:
                    type:
                      type: string
                    status:
                      type: string
                    reason:
                      type: string
                    message:
                      type: string
                    lastTransitionTime:
                      type: string
              lastHeartbeat:
                type: string
                format: date-time
    additionalPrinterColumns:
    - name: State
      type: string
      jsonPath: .status.phase
    - name: macOS
      type: string
      jsonPath: .status.systemInfo.macosVersion
    - name: Xcode
      type: string
      jsonPath: .status.systemInfo.xcodeVersion
    - name: VMs
      type: string
      jsonPath: .status.workloads.vmsRunning
    - name: Devices
      type: integer
      jsonPath: .status.workloads.devicesAttached
    - name: Disk Free
      type: string
      jsonPath: .status.resources.diskFreeGB
```

### Step 4.2: MacHostOperation CRD

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: machostoperations.fleet.vz-kubelet.io
spec:
  group: fleet.vz-kubelet.io
  names:
    kind: MacHostOperation
    plural: machostoperations
    shortNames: [mhop]
  scope: Cluster
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            required: [hostRef, operation]
            properties:
              hostRef:
                type: string
                description: "Name of MacHost to operate on"
              hostSelector:
                type: object
                description: "Select multiple hosts by label"
                properties:
                  matchLabels:
                    type: object
                    additionalProperties:
                      type: string
              operation:
                type: string
                enum:
                - restart
                - shutdown
                - drain
                - uncordon
                - brewUpdate
                - brewUpgrade
                - brewInstall
                - xcodeInstall
                - xcodeSelect
                - softwareUpdate
                - clearCaches
                - runScript
                - syncTime
              parameters:
                type: object
                properties:
                  packages:
                    type: array
                    items:
                      type: string
                  xcodeVersion:
                    type: string
                  script:
                    type: string
                  gracePeriod:
                    type: integer
                    default: 300
                  force:
                    type: boolean
                    default: false
              rollout:
                type: object
                properties:
                  maxUnavailable:
                    type: integer
                    default: 1
                  pauseBetweenHosts:
                    type: integer
                    default: 60
          status:
            type: object
            properties:
              phase:
                type: string
                enum: [Pending, Running, Completed, Failed]
              startTime:
                type: string
                format: date-time
              completionTime:
                type: string
                format: date-time
              hostsTotal:
                type: integer
              hostsCompleted:
                type: integer
              hostsFailed:
                type: integer
              results:
                type: array
                items:
                  type: object
                  properties:
                    host:
                      type: string
                    status:
                      type: string
                    message:
                      type: string
                    duration:
                      type: string
```

### Step 4.3: Fleet agent implementation

The fleet agent runs as part of the unified host agent:

```go
// pkg/fleet/agent.go
package fleet

type FleetAgent struct {
    nodeName  string
    k8sClient client.Client
    hostInfo  *HostInfo
}

type HostInfo struct {
    MacOSVersion  string
    XcodeVersion  string
    XcodePath     string
    Chip          string
    SerialNumber  string
    CPUCores      int
    MemoryGB      int
    DiskTotalGB   int
    DiskFreeGB    int
}

func (a *FleetAgent) Run(ctx context.Context) error {
    // Create/update MacHost CR on startup
    if err := a.registerHost(ctx); err != nil {
        return err
    }
    
    // Start heartbeat loop
    go a.heartbeatLoop(ctx)
    
    // Watch for operations targeting this host
    return a.watchOperations(ctx)
}

func (a *FleetAgent) registerHost(ctx context.Context) error {
    info := a.collectHostInfo()
    
    host := &v1alpha1.MacHost{
        ObjectMeta: metav1.ObjectMeta{
            Name: a.nodeName,
            Labels: map[string]string{
                "fleet.vz-kubelet.io/chip": info.Chip,
            },
        },
        Status: v1alpha1.MacHostStatus{
            Phase: "Active",
            SystemInfo: v1alpha1.SystemInfo{
                MacOSVersion:  info.MacOSVersion,
                XcodeVersion:  info.XcodeVersion,
                Chip:          info.Chip,
                SerialNumber:  info.SerialNumber,
            },
            Resources: v1alpha1.HostResources{
                CPUCores:    info.CPUCores,
                MemoryGB:    info.MemoryGB,
                DiskTotalGB: info.DiskTotalGB,
                DiskFreeGB:  info.DiskFreeGB,
            },
            LastHeartbeat: metav1.Now(),
        },
    }
    
    return a.k8sClient.Create(ctx, host)
}

func (a *FleetAgent) collectHostInfo() *HostInfo {
    info := &HostInfo{}
    
    // macOS version
    out, _ := exec.Command("sw_vers", "-productVersion").Output()
    info.MacOSVersion = strings.TrimSpace(string(out))
    
    // Xcode version
    out, _ = exec.Command("xcodebuild", "-version").Output()
    lines := strings.Split(string(out), "\n")
    if len(lines) > 0 {
        info.XcodeVersion = strings.TrimPrefix(lines[0], "Xcode ")
    }
    
    // Xcode path
    out, _ = exec.Command("xcode-select", "-p").Output()
    info.XcodePath = strings.TrimSpace(string(out))
    
    // Chip info
    out, _ = exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
    info.Chip = strings.TrimSpace(string(out))
    
    // Serial number
    out, _ = exec.Command("system_profiler", "SPHardwareDataType").Output()
    // Parse serial from output...
    
    // Memory
    out, _ = exec.Command("sysctl", "-n", "hw.memsize").Output()
    memBytes, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
    info.MemoryGB = int(memBytes / 1024 / 1024 / 1024)
    
    // CPU cores
    out, _ = exec.Command("sysctl", "-n", "hw.ncpu").Output()
    info.CPUCores, _ = strconv.Atoi(strings.TrimSpace(string(out)))
    
    // Disk space
    var stat syscall.Statfs_t
    syscall.Statfs("/", &stat)
    info.DiskTotalGB = int(stat.Blocks * uint64(stat.Bsize) / 1024 / 1024 / 1024)
    info.DiskFreeGB = int(stat.Bavail * uint64(stat.Bsize) / 1024 / 1024 / 1024)
    
    return info
}
```

### Step 4.4: Operation handlers

```go
// pkg/fleet/operations.go
package fleet

type OperationHandler interface {
    Execute(ctx context.Context, params map[string]interface{}) error
}

type RestartHandler struct {
    vmProvider    *vm.Provider
    deviceAgent   *device.Agent
}

func (h *RestartHandler) Execute(ctx context.Context, params map[string]interface{}) error {
    gracePeriod := 300 * time.Second
    if gp, ok := params["gracePeriod"].(int); ok {
        gracePeriod = time.Duration(gp) * time.Second
    }
    
    // 1. Drain workloads
    log.Info("Draining VMs...")
    if err := h.vmProvider.DrainAll(ctx, gracePeriod); err != nil {
        return fmt.Errorf("failed to drain VMs: %w", err)
    }
    
    log.Info("Releasing devices...")
    if err := h.deviceAgent.ReleaseAll(ctx); err != nil {
        return fmt.Errorf("failed to release devices: %w", err)
    }
    
    // 2. Schedule restart
    log.Info("Scheduling restart...")
    cmd := exec.Command("sudo", "shutdown", "-r", "+1", "Kubernetes fleet restart")
    return cmd.Run()
}

type BrewUpgradeHandler struct{}

func (h *BrewUpgradeHandler) Execute(ctx context.Context, params map[string]interface{}) error {
    // Update Homebrew
    if err := exec.CommandContext(ctx, "brew", "update").Run(); err != nil {
        return fmt.Errorf("brew update failed: %w", err)
    }
    
    // Upgrade specific packages or all
    packages, _ := params["packages"].([]string)
    if len(packages) == 0 {
        return exec.CommandContext(ctx, "brew", "upgrade").Run()
    }
    
    args := append([]string{"upgrade"}, packages...)
    return exec.CommandContext(ctx, "brew", args...).Run()
}

type XcodeInstallHandler struct{}

func (h *XcodeInstallHandler) Execute(ctx context.Context, params map[string]interface{}) error {
    version, ok := params["xcodeVersion"].(string)
    if !ok {
        return fmt.Errorf("xcodeVersion required")
    }
    
    // Use xcodes CLI for installation
    // First, install specified version
    cmd := exec.CommandContext(ctx, "xcodes", "install", version)
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("xcode install failed: %w", err)
    }
    
    // Then select it
    return exec.CommandContext(ctx, "xcodes", "select", version).Run()
}

type ClearCachesHandler struct{}

func (h *ClearCachesHandler) Execute(ctx context.Context, params map[string]interface{}) error {
    cacheLocations := []string{
        "~/Library/Developer/Xcode/DerivedData",
        "~/Library/Caches/com.apple.dt.Xcode",
        "~/.tart/cache",
        "~/Library/Caches/com.agoda.fleet.virtualization",
    }
    
    for _, loc := range cacheLocations {
        expanded := expandPath(loc)
        log.Infof("Clearing %s", expanded)
        os.RemoveAll(expanded)
    }
    
    return nil
}

// Operation registry
var handlers = map[string]OperationHandler{
    "restart":        &RestartHandler{},
    "shutdown":       &ShutdownHandler{},
    "drain":          &DrainHandler{},
    "uncordon":       &UncordonHandler{},
    "brewUpdate":     &BrewUpdateHandler{},
    "brewUpgrade":    &BrewUpgradeHandler{},
    "brewInstall":    &BrewInstallHandler{},
    "xcodeInstall":   &XcodeInstallHandler{},
    "xcodeSelect":    &XcodeSelectHandler{},
    "softwareUpdate": &SoftwareUpdateHandler{},
    "clearCaches":    &ClearCachesHandler{},
    "runScript":      &RunScriptHandler{},
    "syncTime":       &SyncTimeHandler{},
}
```

### Step 4.5: Fleet controller (runs in-cluster)

```go
// controllers/machostoperation_controller.go
package controllers

type MacHostOperationReconciler struct {
    client.Client
    Scheme *runtime.Scheme
}

func (r *MacHostOperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var op v1alpha1.MacHostOperation
    if err := r.Get(ctx, req.NamespacedName, &op); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }
    
    switch op.Status.Phase {
    case "", "Pending":
        return r.handlePending(ctx, &op)
    case "Running":
        return r.handleRunning(ctx, &op)
    }
    
    return ctrl.Result{}, nil
}

func (r *MacHostOperationReconciler) handlePending(ctx context.Context, op *v1alpha1.MacHostOperation) (ctrl.Result, error) {
    // Resolve target hosts
    hosts, err := r.resolveHosts(ctx, op)
    if err != nil {
        return ctrl.Result{}, err
    }
    
    // Check rollout constraints
    unavailable := r.countUnavailable(ctx, hosts)
    maxUnavailable := 1
    if op.Spec.Rollout != nil {
        maxUnavailable = op.Spec.Rollout.MaxUnavailable
    }
    
    if unavailable >= maxUnavailable {
        // Wait for other operations to complete
        return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
    }
    
    // Start operation
    op.Status.Phase = "Running"
    op.Status.StartTime = &metav1.Time{Time: time.Now()}
    op.Status.HostsTotal = len(hosts)
    
    // Signal host agents via annotation on MacHost
    for _, host := range hosts {
        host.Annotations["fleet.vz-kubelet.io/pending-operation"] = op.Name
        r.Update(ctx, &host)
    }
    
    return ctrl.Result{RequeueAfter: 10 * time.Second}, r.Status().Update(ctx, op)
}
```

### Step 4.6: Usage examples

```yaml
# View fleet status
# kubectl get machosts
NAME           STATE    MACOS   XCODE   VMS   DEVICES   DISK FREE
mac-mini-01    Active   15.2    16.0    2     3         245Gi
mac-mini-02    Active   15.2    16.0    1     2         312Gi
mac-mini-03    Maint    15.1    15.4    0     0         189Gi

---
# Restart a single host
apiVersion: fleet.vz-kubelet.io/v1alpha1
kind: MacHostOperation
metadata:
  name: restart-mac-mini-01
spec:
  hostRef: mac-mini-01
  operation: restart
  parameters:
    gracePeriod: 300

---
# Update Xcode on all hosts (rolling)
apiVersion: fleet.vz-kubelet.io/v1alpha1
kind: MacHostOperation
metadata:
  name: xcode-16-1-rollout
spec:
  hostSelector:
    matchLabels:
      fleet: ci-builders
  operation: xcodeInstall
  parameters:
    xcodeVersion: "16.1"
  rollout:
    maxUnavailable: 1
    pauseBetweenHosts: 300

---
# Clear caches on low-disk hosts
apiVersion: fleet.vz-kubelet.io/v1alpha1
kind: MacHostOperation
metadata:
  name: clear-caches-low-disk
spec:
  hostSelector:
    matchExpressions:
    - key: fleet.vz-kubelet.io/disk-pressure
      operator: Exists
  operation: clearCaches

---
# Run custom script
apiVersion: fleet.vz-kubelet.io/v1alpha1
kind: MacHostOperation
metadata:
  name: custom-maintenance
spec:
  hostRef: mac-mini-07
  operation: runScript
  parameters:
    script: |
      #!/bin/bash
      set -e
      killall Simulator || true
      xcrun simctl shutdown all
      xcrun simctl erase all
```

---

## MVP feature summary

| Phase | Feature | Priority |
|-------|---------|----------|
| 1 | Native LZ4 decompression | P0 |
| 1 | Tart media type detection | P0 |
| 1 | Tart config mapping | P0 |
| 2 | iOSDevice CRD | P0 |
| 2 | iOSDeviceClaim CRD | P0 |
| 2 | Device agent (go-ios) | P0 |
| 2 | Claim controller | P0 |
| 2 | WDA lifecycle | P1 |
| 3 | APFS CoW overlays | P1 |
| 3 | Graceful VM shutdown | P1 |
| 3 | Prometheus metrics | P1 |
| 4 | MacHost CRD | P1 |
| 4 | MacHostOperation CRD | P1 |
| 4 | Fleet agent | P1 |
| 4 | Operation handlers | P2 |
| 4 | Fleet controller | P2 |

---

## Development environment setup

```bash
# Prerequisites (note: tart CLI not required - native LZ4 support built-in)
brew install go go-ios kubectl kubebuilder xcodes

# Clone and build
git clone https://github.com/YOUR_FORK/macOS-vz-kubelet.git
cd macOS-vz-kubelet
go build -o bin/vz-kubelet ./cmd/vz-kubelet
go build -o bin/host-agent ./cmd/host-agent

# Sign with entitlements
codesign --entitlements resources/vz.entitlements -s - bin/vz-kubelet
codesign --entitlements resources/vz.entitlements -s - bin/host-agent

# Run locally
export KUBECONFIG=~/.kube/config
./bin/host-agent --nodename=$(hostname)
```

---

## Testing strategy

```bash
# Unit tests
go test ./pkg/...

# Integration: Tart image pull
kubectl apply -f test/e2e/tart-pod.yaml
kubectl wait --for=condition=Ready pod/tart-test --timeout=600s

# Integration: Device claim
kubectl apply -f test/e2e/device-claim.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Bound iosdeviceclaim/test-claim

# Integration: Host operation
kubectl apply -f test/e2e/host-restart.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Completed machostoperation/test-restart
```

---

## Key file locations

| Path | Purpose |
|------|---------|
| `pkg/image/` | ImageProvider interface and implementations |
| `pkg/image/tart/` | Tart image provider |
| `pkg/vm/` | VM lifecycle management |
| `pkg/device/` | iOS device agent |
| `pkg/fleet/` | Fleet agent and operation handlers |
| `controllers/` | Kubernetes controllers |
| `config/crd/` | CRD definitions |
| `cmd/host-agent/` | Unified host agent binary |

---

## FAQ

**Q: Why not USB passthrough for iOS devices?**
A: Apple's Virtualization.framework doesn't support it—only emulated mass storage. Devices must connect to host directly.

**Q: Do I need Apple entitlements for CI/CD?**
A: No. NAT networking works without entitlements and covers most CI/CD use cases.

**Q: Can I run more than 2 VMs per Mac?**
A: No. Apple hard limit. Consider Anka (commercial) for higher density.

**Q: Should I use Rust?**
A: Probably not. Go + Code-Hex/vz handles everything. If needed, use gRPC sidecar.

**Q: How does fleet management differ from Jamf/Kandji?**
A: Those are MDM solutions for managed devices. This is infrastructure-as-code for CI/CD hosts—declarative, K8s-native, developer-focused.
