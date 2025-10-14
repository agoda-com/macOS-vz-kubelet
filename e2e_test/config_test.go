package e2e_test

import (
	"flag"
	"fmt"
	"os"
	"time"
)

const (
	taintKey    = "virtual-kubelet.io/provider"
	taintValue  = "macos-vz-e2e"
	taintEffect = "NoSchedule"

	daemonEndpointPort   = 10253
	macosProofFilePath   = "/tmp/macos_poststart_proof.txt"
	busyboxProofFilePath = "/tmp/busybox_poststart_proof.txt"

	registryContainerName = "virtual-kubelet-e2e-registry"

	postStartPollInterval = time.Second
)

var (
	nodeName = flag.String("node-name", fmt.Sprintf("macos-vz-kubelet-e2e-test-node-%d", time.Now().Unix()), "Node name")
	// namespace defaults to the namespace of the pod running the tests when executed inside-cluster.
	namespace          = flag.String("namespace", os.Getenv("POD_NAMESPACE"), "Namespace scope for requests")
	macOSImage         = flag.String("macos-image", "localhost:5000/macos:latest", "Image to use for the pod")
	macOSImageDir      = flag.String("macos-image-dir", "", "Path to an unpacked macOS OCI image directory to upload to the local registry")
	registryUsername   = flag.String("registry-username", "admin", "Username the local registry should require")
	registryPassword   = flag.String("registry-password", "admin", "Password the local registry should require")
	busyboxImage       = flag.String("busybox-image", "busybox:latest", "Image to use for the pod")
	podCreationTimeout = flag.Duration("pod-creation-timeout", 10*time.Minute, "Timeout for waiting for pod readiness")
	// Poll interval defaults to a conservative 5 seconds to avoid overwhelming the API server in long-running scenarios.
	podCreationPollInterval = flag.Duration("pod-creation-poll-interval", 5*time.Second, "Polling interval for pod readiness")

	dockerSocketPath = flag.String("docker-socket-path", "unix:///var/run/docker.sock", "Path to the Docker socket")
	clientCACert     = flag.String("client-verify-ca", os.Getenv("APISERVER_CA_CERT_LOCATION"), "CA cert to use to verify client requests")
	certPath         = flag.String("cert-path", os.Getenv("APISERVER_CERT_LOCATION"), "Path to the certificate file")
	keyPath          = flag.String("key-path", os.Getenv("APISERVER_KEY_LOCATION"), "Path to the key file")
)
