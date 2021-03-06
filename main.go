/*
Copyright 2022.

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
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	urlshortenerv1alpha1 "github.com/av0de/urlshortener/api/v1alpha1"
	shortlinkclient "github.com/av0de/urlshortener/pkg/client"
	urlshortenercontroller "github.com/av0de/urlshortener/pkg/controller"
	urlshortenerrouter "github.com/av0de/urlshortener/pkg/router"
	urlshortenertrace "github.com/av0de/urlshortener/pkg/tracing"
	"github.com/go-logr/logr"

	//+kubebuilder:scaffold:imports

	"github.com/av0de/urlshortener/controllers"
)

var (
	scheme         = runtime.NewScheme()
	setupLog       = ctrl.Log.WithName("setup")
	serviceName    = "github.com/av0de/urlshortener"
	serviceVersion = "1.0.0"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(urlshortenerv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var bindAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&bindAddr, "bind-address", ":8443", "The address the service binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for urlshortener. "+
			"Enabling this will ensure there is only one active urlshortener.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	shutdownLog := ctrl.Log.WithName("shutdown")

	tracer, tp, err := urlshortenertrace.InitTracer(serviceName, serviceVersion)
	if err != nil {
		setupLog.Error(err, "failed initializing tracer")
		os.Exit(1)
	}
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			shutdownLog.Error(err, "Error shutting down tracer provider: %v")
		}
	}()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		MetricsBindAddress:            metricsAddr,
		Port:                          9443,
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "a9a252fc.av0.de",
		LeaderElectionReleaseOnCancel: false,
	})
	if err != nil {
		setupLog.Error(err, "unable to start urlshortener")
		os.Exit(1)
	}

	shortlinkClient := shortlinkclient.NewShortlinkClient(
		mgr.GetClient(),
		ctrl.Log,
		tracer,
	)

	if err = (&controllers.ShortLinkReconciler{
		ShortlinkClient: shortlinkClient,
		Scheme:          mgr.GetScheme(),
		Log:             &ctrl.Log,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ShortLink")
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

	// run our urlshortener mgr in a separate go routine
	go func() {
		setupLog.Info("starting urlshortener")
		if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
			setupLog.Error(err, "problem running urlshortener")
			os.Exit(1)
		}
	}()

	shortlinkController := urlshortenercontroller.NewShortlinkController(&ctrl.Log, tracer, shortlinkClient)

	// Init Gin Framework
	router, srv := urlshortenerrouter.NewGinGonicHTTPServer(&setupLog, bindAddr)

	setupLog.Info("Load API routes")
	urlshortenerrouter.Load(
		router,
		&ctrl.Log,
		tracer,
		shortlinkController,
	)

	// run our gin server mgr in a separate go routine
	go func() {
		if err := srv.ListenAndServe(); err != nil && errors.Is(err, http.ErrServerClosed) {
			setupLog.Error(err, "listen\n")
		}
	}()

	handleShutdown(srv, &shutdownLog)

	shutdownLog.Info("Server exiting")
}

// handleShutdown waits for interupt signal and then tries to gracefully
// shutdown the server with a timeout of 5 seconds.
func handleShutdown(srv *http.Server, shutdownLog *logr.Logger) {
	quit := make(chan os.Signal, 1)

	signal.Notify(
		quit,
		syscall.SIGINT,  // kill -2 is syscall.SIGINT
		syscall.SIGTERM, // kill (no param) default send syscall.SIGTERM
		// kill -9 is syscall.SIGKILL but can't be caught
	)

	// wait (and block) until shutdown signal is received
	<-quit
	shutdownLog.Info("Shutting down server...")

	// The context is used to inform the server it has 5 seconds to finish
	// the request it is currently handling
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// try to shut down the http server gracefully. If ctx deadline exceeds
	// then srv.Shutdown(ctx) will return an error, causing us to force
	// the shutdown
	if err := srv.Shutdown(ctx); err != nil {
		shutdownLog.Error(err, "Server forced to shutdown")
		os.Exit(1)
	}
}
