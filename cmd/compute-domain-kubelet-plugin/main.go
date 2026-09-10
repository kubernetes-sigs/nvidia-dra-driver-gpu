/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urfave/cli/v2"

	"k8s.io/component-base/logs"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	_ "k8s.io/component-base/metrics/prometheus/restclient" // for client metric registration
	_ "k8s.io/component-base/metrics/prometheus/workqueue"  // register work queues in the default legacy registry

	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/info"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/metrics"
)

const (
	DriverName                              = "compute-domain.nvidia.com"
	DriverPluginCheckpointFileBasename      = "checkpoint.json"
	IMEXDaemonsWithDNSNamesMinDriverVersion = "570.158.01"

	// defaultIMEXHostSocketPath is the default location, under the driver
	// root, of the Unix domain socket the host nvidia-imex daemon's
	// command/control service listens on when host-managed IMEX is enabled.
	// It corresponds to the host's IMEX_CMD_UNIX_DOMAIN_PATH setting in
	// /etc/nvidia-imex/config.cfg (see site/content/docs/prerequisites.md).
	defaultIMEXHostSocketPath = "/etc/nvidia-imex/imex_ctrl.sock"
)

type Flags struct {
	kubeClientConfig pkgflags.KubeClientConfig

	nodeName                      string
	httpEndpoint                  string
	metricsPath                   string
	namespace                     string
	cdiRoot                       string
	containerDriverRoot           string
	hostDriverRoot                string
	nvidiaCDIHookPath             string
	kubeletRegistrarDirectoryPath string
	kubeletPluginsDirectoryPath   string
	healthcheckPort               int
	klogVerbosity                 int
	gpuCliqueLabelEnabled         bool
	imexMode                      string
	imexIsolation                 string
	imexHostSocketPath            string
}

type Config struct {
	flags      *Flags
	clientsets pkgflags.ClientSets
	imexConfig imex.Config
}

func (c Config) DriverPluginPath() string {
	return filepath.Join(c.flags.kubeletPluginsDirectoryPath, DriverName)
}

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	loggingConfig := pkgflags.NewLoggingConfig()
	featureGateConfig := pkgflags.NewFeatureGateConfig()
	flags := &Flags{}

	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "node-name",
			Usage:       "The name of the node to be worked on.",
			Required:    true,
			Destination: &flags.nodeName,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name:        "namespace",
			Usage:       "The namespace used for the custom resources.",
			Value:       "default",
			Destination: &flags.namespace,
			EnvVars:     []string{"NAMESPACE"},
		},
		&cli.StringFlag{
			Name:        "http-endpoint",
			Usage:       "The TCP network `address` where the metrics HTTP server will listen (example: `:8080`). The default is the empty string, which means the server is disabled.",
			Destination: &flags.httpEndpoint,
			EnvVars:     []string{"HTTP_ENDPOINT"},
		},
		&cli.StringFlag{
			Name:        "metrics-path",
			Usage:       "The HTTP `path` where Prometheus metrics are exposed, disabled if empty.",
			Value:       "/metrics",
			Destination: &flags.metricsPath,
			EnvVars:     []string{"METRICS_PATH"},
		},
		&cli.StringFlag{
			Name:        "cdi-root",
			Usage:       "Absolute path to the directory where CDI files will be generated.",
			Value:       "/etc/cdi",
			Destination: &flags.cdiRoot,
			EnvVars:     []string{"CDI_ROOT"},
		},
		&cli.StringFlag{
			Name:        "nvidia-driver-root",
			Aliases:     []string{"host_driver-root"},
			Value:       "/",
			Usage:       "the root path for the NVIDIA driver installation on the host (typical values are '/' or '/run/nvidia/driver')",
			Destination: &flags.hostDriverRoot,
			EnvVars:     []string{"NVIDIA_DRIVER_ROOT", "HOST_DRIVER_ROOT"},
		},
		&cli.StringFlag{
			Name:        "container-driver-root",
			Value:       "/driver-root",
			Usage:       "the path where the NVIDIA driver root is mounted in the container; used for generating CDI specifications",
			Destination: &flags.containerDriverRoot,
			EnvVars:     []string{"DRIVER_ROOT_CTR_PATH"},
		},
		&cli.StringFlag{
			Name:        "nvidia-cdi-hook-path",
			Usage:       "Absolute path to the nvidia-cdi-hook executable in the host file system. Used in the generated CDI specification.",
			Destination: &flags.nvidiaCDIHookPath,
			EnvVars:     []string{"NVIDIA_CDI_HOOK_PATH"},
		},
		&cli.StringFlag{
			Name:        "kubelet-registrar-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin registrations.",
			Value:       kubeletplugin.KubeletRegistryDir,
			Destination: &flags.kubeletRegistrarDirectoryPath,
			EnvVars:     []string{"KUBELET_REGISTRAR_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name:        "kubelet-plugins-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin data.",
			Value:       kubeletplugin.KubeletPluginsDir,
			Destination: &flags.kubeletPluginsDirectoryPath,
			EnvVars:     []string{"KUBELET_PLUGINS_DIRECTORY_PATH"},
		},
		&cli.IntFlag{
			Name:        "healthcheck-port",
			Usage:       "Port to start a gRPC healthcheck service. When positive, a literal port number. When zero, a random port is allocated. When negative, the healthcheck service is disabled.",
			Value:       -1,
			Destination: &flags.healthcheckPort,
			EnvVars:     []string{"HEALTHCHECK_PORT"},
		},
		&cli.BoolFlag{
			Name:        "gpu-clique-label-enabled",
			Usage:       "Set the nvidia.com/gpu.clique node label, historically set by gpu-feature-discovery.",
			Value:       false,
			Destination: &flags.gpuCliqueLabelEnabled,
			EnvVars:     []string{"GPU_CLIQUE_LABEL_ENABLED"},
		},
		&cli.StringFlag{
			Name:        "imex-mode",
			Usage:       "IMEX deployment mode: driverManaged or hostManaged.",
			Value:       string(imex.ModeDriverManaged),
			Destination: &flags.imexMode,
			EnvVars:     []string{"IMEX_MODE"},
		},
		&cli.StringFlag{
			Name:        "imex-isolation",
			Usage:       "IMEX isolation strategy: domain (default; all workloads in the same IMEX domain share channel 0) or channel (not yet supported).",
			Value:       string(imex.IsolationIMEXDomain),
			Destination: &flags.imexIsolation,
			EnvVars:     []string{"IMEX_ISOLATION"},
		},
		&cli.StringFlag{
			Name:        "imex-host-socket-path",
			Usage:       "Path (under the nvidia-driver-root) to the Unix domain socket the host nvidia-imex daemon's command/control service listens on. Only used when imex-mode=hostManaged, to validate host IMEX readiness during channel allocation.",
			Value:       defaultIMEXHostSocketPath,
			Destination: &flags.imexHostSocketPath,
			EnvVars:     []string{"IMEX_HOST_SOCKET_PATH"},
		},
	}
	cliFlags = append(cliFlags, flags.kubeClientConfig.Flags()...)
	cliFlags = append(cliFlags, featureGateConfig.Flags()...)
	cliFlags = append(cliFlags, loggingConfig.Flags()...)

	app := &cli.App{
		Name:            "compute-domain-kubelet-plugin",
		Usage:           "compute-domain-kubelet-plugin implements a DRA driver plugin for NVIDIA compute domains.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			// `loggingConfig` must be applied before doing any logging
			err := loggingConfig.Apply()

			// Store klog's log verbosity setting in this program's config for
			// later runtime inspection (it's otherwise not accessible anymore
			// because we do not expose the raw `cliFlags`.
			flags.klogVerbosity = int(loggingConfig.Config.Verbosity)
			pkgflags.LogStartupConfig(flags, loggingConfig)
			return err
		},
		Action: func(c *cli.Context) error {
			// Validate feature gate dependencies
			if err := featuregates.ValidateFeatureGates(); err != nil {
				return fmt.Errorf("feature gate validation failed: %w", err)
			}

			imexConfig := imex.Config{Mode: imex.Mode(flags.imexMode), Isolation: imex.Isolation(flags.imexIsolation)}
			if err := imexConfig.Validate(featuregates.Enabled(featuregates.HostManagedIMEXDaemon)); err != nil {
				return fmt.Errorf("imex configuration validation failed: %w", err)
			}

			clientSets, err := flags.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			config := &Config{
				flags:      flags,
				clientsets: clientSets,
				imexConfig: imexConfig,
			}

			return RunPlugin(c.Context, config)
		},
		After: func(c *cli.Context) error {
			// Runs after `Action` (regardless of success/error). In urfave cli
			// v2, the final error reported will be from either Action, Before,
			// or After (whichever is non-nil and last executed).
			klog.Infof("shutdown")
			logs.FlushLogs()
			return nil
		},
		Version: info.GetVersionString(),
	}

	// We remove the -v alias for the version flag so as to not conflict with the -v flag used for klog.
	f, ok := cli.VersionFlag.(*cli.BoolFlag)
	if ok {
		f.Aliases = nil
	}

	return app
}

// RunPlugin initializes and runs the compute domain kubelet plugin.
func RunPlugin(ctx context.Context, config *Config) error {
	common.StartDebugSignalHandlers()

	// Create the plugin directory
	err := os.MkdirAll(config.DriverPluginPath(), 0750)
	if err != nil {
		return err
	}

	// Setup nvidia-cdi-hook binary
	if err := config.setNvidiaCDIHookPath(); err != nil {
		return fmt.Errorf("error setting up nvidia-cdi-hook: %w", err)
	}

	// Initialize CDI root directory
	info, err := os.Stat(config.flags.cdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		err := os.MkdirAll(config.flags.cdiRoot, 0750)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for cdi file generation is not a directory: '%v'", config.flags.cdiRoot)
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancel()

	metrics.InitializeDRARequestMetrics(DriverName)

	if config.flags.httpEndpoint != "" {
		if err := metrics.RunPrometheusMetricsServer(ctx, config.flags.httpEndpoint, config.flags.metricsPath); err != nil {
			return fmt.Errorf("setup metrics endpoint: %w", err)
		}
	}

	// Create and start the driver
	driver, err := NewDriver(ctx, config)
	if err != nil {
		return fmt.Errorf("error creating driver: %w", err)
	}

	<-ctx.Done()
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		// A canceled context is the normal case here when the process receives
		// a signal. Only log the error for more interesting cases.
		klog.Errorf("error from context: %v", err)
	}

	err = driver.Shutdown()
	if err != nil {
		klog.Errorf("unable to cleanly shutdown driver: %v", err)
	}

	return nil
}

// setNvidiaCDIHookPath ensures the proper flag is set with the host path for the nvidia-cdi-hook binary.
// If 'f.nvidiaCDIHookPath' is already set (from the command line), do nothing.
// If 'f.nvidiaCDIHookPath' is empty, it copies the nvidia-cdi-hook binary from
// /usr/bin/nvidia-cdi-hook to DriverPluginPath and sets 'f.nvidiaCDIHookPath'
// to this path. The /usr/bin/nvidia-cdi-hook is present in the current
// container image because it is copied from the toolkit image into this
// container at build time.
func (c Config) setNvidiaCDIHookPath() error {
	if c.flags.nvidiaCDIHookPath != "" {
		return nil
	}

	sourcePath := "/usr/bin/nvidia-cdi-hook"
	targetPath := filepath.Join(c.DriverPluginPath(), "nvidia-cdi-hook")

	input, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("error reading nvidia-cdi-hook: %w", err)
	}

	if err := os.WriteFile(targetPath, input, 0755); err != nil {
		return fmt.Errorf("error copying nvidia-cdi-hook: %w", err)
	}

	c.flags.nvidiaCDIHookPath = targetPath

	return nil
}
