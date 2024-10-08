/*
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
*/

package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strconv"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	ibclient "github.com/infobloxopen/infoblox-go-client/v2"
	"github.com/middlewaregruppen/infoblox-dns-controller/internal/controller"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	infobloxServer   string
	infobloxPort     uint
	infobloxUsername string
	infobloxPassword string
	infobloxVersion  string
	infobloxView     string
	infobloxZone     string
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool

	// Default vars
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false, "If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false, "If set, HTTP/2 will be enabled for the metrics and webhook servers")

	// Our vars
	flag.StringVar(&infobloxServer, "infoblox-server", "localhost", "Host name or IP address of the Infoblox server")
	flag.UintVar(&infobloxPort, "infoblox-port", 443, "The port number of the Infoblox server")
	flag.StringVar(&infobloxUsername, "infoblox-username", "", "Username to authenticate as to the Infoblox server")
	flag.StringVar(&infobloxPassword, "infoblox-password", "", "Password of the user to authenticate as to the Infoblox server")
	flag.StringVar(&infobloxVersion, "infoblox-version", "v2.12.1", "The version of the Infoblox server")
	flag.StringVar(&infobloxView, "infoblox-view", "", "The Infoblox View to use for this controller")
	flag.StringVar(&infobloxZone, "infoblox-zone", "", "The Infoblox Zone to use for this controller")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "infoblox-dns-controller-leader-election",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Setup infoblox connector
	hostConfig := ibclient.HostConfig{
		Host:    infobloxServer,
		Port:    strconv.FormatUint(uint64(infobloxPort), 10),
		Version: infobloxVersion,
	}
	authCfg := ibclient.AuthConfig{
		Username: infobloxUsername,
		Password: infobloxPassword,
	}
	transportConfig := ibclient.NewTransportConfig("false", 20, 10)
	requestBuilder := &ibclient.WapiRequestBuilder{}
	requestor := &ibclient.WapiHttpRequestor{}

	// Replace the 'v' in version attribute
	hostConfig.Version = strings.Replace(hostConfig.Version, "v", "", 1)
	ibsConn, err := ibclient.NewConnector(hostConfig, authCfg, transportConfig, requestBuilder, requestor)
	if err != nil {
		setupLog.Error(err, "unable to create infoblox connector")
		os.Exit(1)
	}

	// Setup controller config
	cfg := &controller.InfobloxConfig{
		View:    infobloxView,
		Zone:    infobloxZone,
		Version: infobloxVersion,
	}

	setupLog.Info("infoblox dns provider",
		"version", infobloxVersion,
		"server", infobloxServer,
		"port", infobloxPort,
		"username", infobloxUsername,
		"view", infobloxView,
		"zone", infobloxZone,
	)

	if err = (&controller.IngressReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr, ibsConn, cfg); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Ingress")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
