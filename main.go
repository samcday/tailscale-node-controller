package main

import (
	"context"
	"flag"
	"fmt"
	"k8s.io/client-go/tools/record"
	"net/netip"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"tailscale.com/ipn"
	"time"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"tailscale.com/client/tailscale"
)

var (
	scheme = runtime.NewScheme()
	log    = ctrl.Log.WithName("tailscalenode")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var nodeName string
	flag.StringVar(&nodeName, "node", "", "Name of the local Node to watch in apiserver")

	logOpts := zap.Options{
		Development: os.Getenv("DEBUG") != "",
		TimeEncoder: zapcore.ISO8601TimeEncoder,
	}
	logOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	lc := tailscale.LocalClient{}
	status, err := lc.Status(context.Background())
	if err != nil {
		log.Error(err, "error checking tailscale daemon status")
		os.Exit(1)
	}
	if status.CurrentTailnet != nil {
		log.Info("Tailscale daemon is connected", "tailnet", status.CurrentTailnet.Name)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOpts)))

	options := ctrl.Options{Scheme: scheme}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&Reconciler{
		Client: mgr.GetClient(),
		lc:     lc,
	}).SetupWithManager(nodeName, mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "fipsetup")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

type Reconciler struct {
	client.Client
	lc       tailscale.LocalClient
	recorder record.EventRecorder
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	status, err := r.lc.Status(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("error checking tailscale daemon status: %w", err)
	}
	if status.BackendState != "Running" {

		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}

	node := corev1.Node{}
	err = r.Get(ctx, req.NamespacedName, &node)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to lookup Node: %w", err)
	}

	prefs, err := r.lc.GetPrefs(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get current routes from local tailscale daemon: %w", err)
	}

	var addedCIDRs []netip.Prefix
	for _, cidrRaw := range node.Spec.PodCIDRs {
		cidr, err := netip.ParsePrefix(cidrRaw)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to parse Node CIDR %s: %w", cidrRaw, err)
		}
		found := false
		for _, route := range prefs.AdvertiseRoutes {
			if route == cidr {
				found = true
				break
			}
		}
		if !found {
			addedCIDRs = append(addedCIDRs, cidr)
		}
	}

	if len(addedCIDRs) > 0 {
		prefs.AdvertiseRoutes = append(prefs.AdvertiseRoutes, addedCIDRs...)
		maskedPrefs := ipn.MaskedPrefs{
			AdvertiseRoutesSet: true,
			Prefs: ipn.Prefs{
				AdvertiseRoutes: prefs.AdvertiseRoutes,
			},
		}
		_, err := r.lc.EditPrefs(ctx, &maskedPrefs)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to add advertise new CIDRs %v: %w", addedCIDRs, err)
		}
		r.recorder.Eventf(&node, corev1.EventTypeNormal, "PodCIDRsAdvertised", "PodCIDR(s) %v advertised from local Tailscale daemon", addedCIDRs)
	}

	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(name string, mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor("tailscalenode")
	return ctrl.
		NewControllerManagedBy(mgr).
		For(&corev1.Node{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}, predicate.NewPredicateFuncs(func(object client.Object) bool {
			return object.GetName() == name
		}))).
		Complete(r)
}
