package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/client"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/provider"
	"github.com/mitchellh/go-homedir"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/apiserver/pkg/server/options"
	"k8s.io/klog"
)

var (
	buildVersion = "N/A"
	k8sVersion   = "v1.29.1" // This should follow the version of k8s.io we are importing

	taintKey    = envOrDefault("VKUBELET_TAINT_KEY", "virtual-kubelet.io/provider")
	taintEffect = envOrDefault("VKUBELET_TAINT_EFFECT", string(v1.TaintEffectNoSchedule))
	taintValue  = envOrDefault("VKUBELET_TAINT_VALUE", "macOS-vz")

	logLevel = "info"
	// traceSampleRate string

	// k8s
	kubeConfigPath  = os.Getenv("KUBECONFIG")
	startupTimeout  time.Duration
	disableTaint    bool
	numberOfWorkers               = 10
	resync          time.Duration = 1 * time.Minute

	certPath       = os.Getenv("APISERVER_CERT_LOCATION")
	keyPath        = os.Getenv("APISERVER_KEY_LOCATION")
	clientCACert   string
	clientNoVerify bool

	webhookAuth                  bool
	webhookAuthnCacheTTL         time.Duration
	webhookAuthzUnauthedCacheTTL time.Duration
	webhookAuthzAuthedCacheTTL   time.Duration
	nodeName                     = "vk-macOS-vz-test"
	listenPort                   = 10250
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	binaryName := filepath.Base(os.Args[0])
	desc := binaryName + " implements a node on a Kubernetes cluster using Virtualization.framework to run macOS Virtual Machines as pods."

	if kubeConfigPath == "" {
		home, _ := homedir.Dir()
		if home != "" {
			kubeConfigPath = filepath.Join(home, ".kube", "config")
		}
	}

	withTaint := func(cfg *nodeutil.NodeConfig) error {
		if disableTaint {
			return nil
		}

		taint := v1.Taint{
			Key:   taintKey,
			Value: taintValue,
		}
		switch taintEffect {
		case "NoSchedule":
			taint.Effect = v1.TaintEffectNoSchedule
		case "NoExecute":
			taint.Effect = v1.TaintEffectNoExecute
		case "PreferNoSchedule":
			taint.Effect = v1.TaintEffectPreferNoSchedule
		default:
			return errdefs.InvalidInputf("taint effect %q is not supported", taintEffect)
		}
		cfg.NodeSpec.Spec.Taints = append(cfg.NodeSpec.Spec.Taints, taint)
		return nil
	}
	withVersion := func(cfg *nodeutil.NodeConfig) error {
		cfg.NodeSpec.Status.NodeInfo.KubeletVersion = strings.Join([]string{k8sVersion, "vk-macOS-vz", buildVersion}, "-")
		return nil
	}
	configureRoutes := func(cfg *nodeutil.NodeConfig) error {
		mux := http.NewServeMux()
		cfg.Handler = mux
		return nodeutil.AttachProviderRoutes(mux)(cfg)
	}
	withWebhookAuth := func(cfg *nodeutil.NodeConfig) error {
		if !webhookAuth {
			cfg.Handler = api.InstrumentHandler(nodeutil.WithAuth(nodeutil.NoAuth(), cfg.Handler))
			return nil
		}

		auth, err := nodeutil.WebhookAuth(cfg.Client, nodeName,
			func(cfg *nodeutil.WebhookAuthConfig) error {
				var err error

				cfg.AuthzConfig.WebhookRetryBackoff = options.DefaultAuthWebhookRetryBackoff()

				if webhookAuthnCacheTTL > 0 {
					cfg.AuthnConfig.CacheTTL = webhookAuthnCacheTTL
				}
				if webhookAuthzAuthedCacheTTL > 0 {
					cfg.AuthzConfig.AllowCacheTTL = webhookAuthzAuthedCacheTTL
				}
				if webhookAuthzUnauthedCacheTTL > 0 {
					cfg.AuthzConfig.AllowCacheTTL = webhookAuthzUnauthedCacheTTL
				}
				if clientCACert != "" {
					ca, err := dynamiccertificates.NewDynamicCAContentFromFile("client-ca", clientCACert)
					if err != nil {
						return err
					}
					cfg.AuthnConfig.ClientCertificateCAContentProvider = ca
					go ca.Run(ctx, 1)
				}
				return err
			})

		if err != nil {
			return err
		}
		cfg.TLSConfig.ClientAuth = tls.RequestClientCert
		cfg.Handler = api.InstrumentHandler(nodeutil.WithAuth(auth, cfg.Handler))
		return nil
	}
	withCA := func(cfg *tls.Config) error {
		if clientCACert == "" {
			return nil
		}
		if err := nodeutil.WithCAFromPath(clientCACert)(cfg); err != nil {
			return fmt.Errorf("error getting CA from path: %w", err)
		}
		if clientNoVerify {
			cfg.ClientAuth = tls.NoClientCert
		}
		return nil
	}
	k8sClient, err := nodeutil.ClientsetFromEnv(kubeConfigPath)
	if err != nil {
		log.G(ctx).Fatal(err)
	}
	withClient := func(cfg *nodeutil.NodeConfig) error {
		return nodeutil.WithClient(k8sClient)(cfg)
	}

	run := func(ctx context.Context) error {
		// if err := configureTracing(nodeName, traceSampleRate); err != nil {
		// 	return err
		// }

		node, err := nodeutil.NewNode(nodeName,
			func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
				if port := os.Getenv("KUBELET_PORT"); port != "" {
					kubeletPort, err := strconv.ParseInt(port, 10, 32)
					if err != nil {
						return nil, nil, err
					}
					listenPort = int(kubeletPort)
				}
				// p, err := azproviderv2.NewACIProvider(ctx, cfgPath, azConfig, azACIAPIs, cfg,
				// 	nodeName, operatingSystem, os.Getenv("VKUBELET_POD_IP"),
				// 	int32(listenPort), clusterDomain, k8sClient)
				platform, _, _, err := host.PlatformInformationWithContext(ctx)
				if err != nil {
					return nil, nil, err
				}

				p, err := provider.NewMacOSVZProvider(ctx, cfg.Pods, nodeName, platform, int32(listenPort))
				if err != nil {
					return nil, nil, err
				}
				p.ConfigureNode(ctx, cfg.Node)
				return p, nil, nil
			},
			withClient,
			withTaint,
			withVersion,
			nodeutil.WithTLSConfig(nodeutil.WithKeyPairFromPath(certPath, keyPath), withCA),
			withWebhookAuth,
			configureRoutes,
			func(cfg *nodeutil.NodeConfig) error {
				cfg.InformerResyncPeriod = resync
				cfg.NumWorkers = numberOfWorkers
				cfg.HTTPListenAddr = fmt.Sprintf(":%d", listenPort)
				return nil
			},
		)
		if err != nil {
			return err
		}

		go func() error {
			err = node.Run(ctx)
			if err != nil {
				return fmt.Errorf("error running the node: %w", err)
			}
			return nil
		}()

		if err := node.WaitReady(ctx, startupTimeout); err != nil {
			return fmt.Errorf("error waiting for node to be ready: %w", err)
		}

		<-node.Done()
		return node.Err()
	}

	cmd := &cobra.Command{
		Use:   binaryName,
		Short: desc,
		Long:  desc,
		Run: func(cmd *cobra.Command, args []string) {
			logger := logrus.StandardLogger()
			lvl, err := logrus.ParseLevel(logLevel)
			if err != nil {
				logrus.WithError(err).Fatal("Error parsing log level")
			}
			logger.SetLevel(lvl)

			log.L = logruslogger.FromLogrus(logrus.NewEntry(logger))
			ctx := log.WithLogger(cmd.Context(), log.G(ctx))

			if err := run(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.G(ctx).Fatal(err)
				}
				log.G(ctx).Debug(err)
			}
		},
	}
	flags := cmd.Flags()

	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	klogFlags.VisitAll(func(f *flag.Flag) {
		f.Name = "klog." + f.Name
		flags.AddGoFlag(f)
	})

	hostName, err := os.Hostname()
	if err != nil {
		log.G(ctx).Fatal(err)
	}
	// lowercase RFC 1123 subdomain
	hostName = strings.ToLower(hostName)

	flags.StringVar(&nodeName, "nodename", hostName, "kubernetes node name")
	// flags.StringVar(&cfgPath, "provider-config", cfgPath, "cloud provider configuration file")
	// flags.StringVar(&clusterDomain, "cluster-domain", clusterDomain, "kubernetes cluster-domain")
	flags.DurationVar(&startupTimeout, "startup-timeout", startupTimeout, "How long to wait for the virtual-kubelet to start")
	flags.BoolVar(&disableTaint, "disable-taint", disableTaint, "disable the node taint")
	// flags.StringVar(&operatingSystem, "os", operatingSystem, "Operating System (Linux/Windows)")
	flags.StringVar(&logLevel, "log-level", logLevel, "log level.")
	flags.IntVar(&numberOfWorkers, "pod-sync-workers", numberOfWorkers, `set the number of pod synchronization workers`)
	flags.DurationVar(&resync, "full-resync-period", resync, "how often to perform a full resync of pods between kubernetes and the provider")

	flags.StringVar(&clientCACert, "client-verify-ca", os.Getenv("APISERVER_CA_CERT_LOCATION"), "CA cert to use to verify client requests")
	flags.BoolVar(&clientNoVerify, "no-verify-clients", clientNoVerify, "Do not require client certificate validation")
	flags.BoolVar(&webhookAuth, "authentication-token-webhook", webhookAuth, ""+
		"Use the TokenReview API to determine authentication for bearer tokens.")
	flags.DurationVar(&webhookAuthnCacheTTL, "authentication-token-webhook-cache-ttl", webhookAuthnCacheTTL,
		"The duration to cache responses from the webhook token authenticator.")
	flags.DurationVar(&webhookAuthzAuthedCacheTTL, "authorization-webhook-cache-authorized-ttl", webhookAuthzAuthedCacheTTL,
		"The duration to cache 'authorized' responses from the webhook authorizer.")
	flags.DurationVar(&webhookAuthzUnauthedCacheTTL, "authorization-webhook-cache-unauthorized-ttl", webhookAuthzUnauthedCacheTTL,
		"The duration to cache 'unauthorized' responses from the webhook authorizer.")

	if err := cmd.ExecuteContext(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			logrus.WithError(err).Fatal("Error running command")
		}
	}
}

func envOrDefault(key string, defaultValue string) string {
	v, set := os.LookupEnv(key)
	if set {
		return v
	}
	return defaultValue
}
