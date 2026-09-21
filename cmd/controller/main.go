package main

import (
	"flag"
	"os"

	arc "forgejo-arc/api/v1alpha1"
	"forgejo-arc/internal/controller"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var leader bool
	flag.BoolVar(&leader, "leader-elect", true, "Use Kubernetes leader election")
	options := zap.Options{}
	options.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options)))
	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	must(arc.AddToScheme(scheme))
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme, LeaderElection: leader, LeaderElectionID: "forgejo-arc.arc.forgejo.org", HealthProbeBindAddress: ":8081", Metrics: metricsserver.Options{BindAddress: ":8080"}})
	must(err)
	must((&controller.Reconciler{Client: mgr.GetClient(), Scheme: scheme}).SetupWithManager(mgr))
	must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(mgr.AddReadyzCheck("readyz", healthz.Ping))
	must(mgr.Start(ctrl.SetupSignalHandler()))
}

func must(err error) {
	if err != nil {
		ctrl.Log.Error(err, "fatal")
		os.Exit(1)
	}
}
