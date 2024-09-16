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

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	ibclient "github.com/infobloxopen/infoblox-go-client/v2"
)

const ingressFinalizers = "infoblox-dns-controller.k8s.mdlwr.io/finalizer"

var (
	ErrObjectNotFound   = errors.New("requested object not found")
	ErrNotFound         = errors.New("not found")
	ErrIngressWithoutIP = errors.New("ingress has no IP Address")

	// Acceptable annotation keys
	allowedAnnotations = []string{"dns-managed-by/infoblox-dns-webhook", "infoblox-dns-controller/manage"}

	counterRecordsAdded = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "infoblox_records_added_total",
			Help: "Number of records added",
		},
		[]string{"net_view", "dns_view", "host", "ipv4address", "ipv6address"},
	)
	counterRecordsRemoved = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "infoblox_records_removed_total",
			Help: "Number of records removed",
		},
		[]string{"net_view", "dns_view", "host", "ipv4address", "ipv6address"},
	)
)

// IngressReconciler reconciles a Ingress object
type IngressReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	conn   *ibclient.Connector
	cfg    *InfobloxConfig
}

type InfobloxConfig struct {
	View    string
	Zone    string
	Version string
}

func init() {
	metrics.Registry.MustRegister(counterRecordsAdded, counterRecordsRemoved)
}

//+kubebuilder:rbac:groups=networking.k8s.io.my.domain,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io.my.domain,resources=ingresses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io.my.domain,resources=ingresses/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Ingress object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// Fetch Ingress - This ensures that the cluster has resources of type Ingress
	// Stops reconciliation if not found, for example if the CRD's has not been applied
	ingress := &netv1.Ingress{}
	if err := r.Get(ctx, req.NamespacedName, ingress); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Add finalizers that will be handled later during delete events
	if !controllerutil.ContainsFinalizer(ingress, ingressFinalizers) {
		l.Info("adding finalizer", "ingress", ingress.Name)
		if ok := controllerutil.AddFinalizer(ingress, ingressFinalizers); !ok {
			return ctrl.Result{Requeue: true}, nil
		}
		if err := r.Update(ctx, ingress); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check for presence of the annotation and that it's set to true
	if !isManagedByController(ingress) {
		return ctrl.Result{}, nil
	}

	namespacedName := fmt.Sprintf("%s/%s", ingress.Namespace, ingress.Name)

	// Create object manager that we use to manage DNS records in infoblox
	objMgr := ibclient.NewObjectManager(r.conn, "", "")

	// Exit early if Ingress doesn't have any IP addresses in its status field
	if len(ingress.Status.LoadBalancer.Ingress) <= 0 {
		l.Info("ingress doesn't have any IP addresses. Retrying in 10s", "ingress", namespacedName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Just use the first IP address in the list for now
	ipaddress := ingress.Status.LoadBalancer.Ingress[0].IP

	// Get a list of Hosts in the ingress
	hosts, err := getIngressHosts(ingress)
	if err != nil {
		return ctrl.Result{}, err
	}

	var recs []*ibclient.HostRecord

	for _, host := range hosts {

		// Check if Host record exists. If it's not found then a new host record is created
		rec, err := getHostRecord(objMgr, r.cfg, host, ipaddress)
		if err != nil {

			// Create record since it's not found
			var notfound *ibclient.NotFoundError
			if errors.As(err, &notfound) {
				err = createHostRecord(objMgr, r.cfg, host, ipaddress)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("error creating host record %s for ingress %s: %v", host, namespacedName, err)
				}
				// Record was created, requeue so we can check again
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}

			// Return the error if any other errors are returned
			return ctrl.Result{}, fmt.Errorf("error fetching host %s for ingress %s: %v", host, namespacedName, err)
		}

		recs = append(recs, rec)

	}

	// Check if resource is marked to be deleted
	if ingress.GetDeletionTimestamp() != nil {
		if controllerutil.ContainsFinalizer(ingress, ingressFinalizers) {

			for _, rec := range recs {
				_, err = r.conn.DeleteObject(rec.Ref)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("error deleting host record %s: %v", *rec.Name, err)
				}
				l.Info("deleted host record", "host", *rec.Name, "ingress", namespacedName)
			}

			// Get the Project resource again so that we don't encounter any "the object has been modified"-errors
			if err = r.Get(ctx, req.NamespacedName, ingress); err != nil {
				return ctrl.Result{}, err
			}
			// Remove finalizers and update status field of the resource
			if ok := controllerutil.RemoveFinalizer(ingress, ingressFinalizers); !ok {
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.Update(ctx, ingress); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// getHostRecord finds and returns a slice of all host records matching the provided ipv4 address.
// We need this because the infoblox object manager library currently doesn't have a way of searching for host records by IP address.
func getHostRecord(objMgr ibclient.IBObjectManager, cfg *InfobloxConfig, name, ipv4addr string) (*ibclient.HostRecord, error) {
	rec, err := objMgr.GetHostRecord(cfg.View, cfg.Zone, name, ipv4addr, "")
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func createHostRecord(objMgr ibclient.IBObjectManager, cfg *InfobloxConfig, host, ipaddress string) error {
	_, err := objMgr.CreateHostRecord(
		true,      // enabledns
		false,     // enabledhcp
		host,      // recordName
		cfg.View,  // DNS View
		cfg.Zone,  // DNS Zone
		"",        // ipv4cidr
		"",        // ipv6cidr
		ipaddress, // ipv4Addr
		"",        // ipv6Addr
		"",        // macAddr
		"",
		true, // useTTL
		30,
		"",
		nil,
		[]string{},
	)

	// "net_view", "dns_view", "host", "ipv4address", "ipv6address"
	counterRecordsAdded.With(prometheus.Labels{
		"type":        "HOST",
		"net_view":    cfg.View,
		"dns_view":    cfg.Zone,
		"host":        host,
		"ipv4address": ipaddress,
		"ipv6address": "",
	}).Inc()
	return err
}

func deleteHostRecord(conn *ibclient.Connector, rec *ibclient.HostRecord, cfg *InfobloxConfig, host, ipaddress string) error {
	_, err := conn.DeleteObject(rec.Ref)
	if err != nil {
		return err
	}
	counterRecordsRemoved.With(prometheus.Labels{
		"type":        "HOST",
		"net_view":    cfg.View,
		"dns_view":    cfg.Zone,
		"host":        host,
		"ipv4address": ipaddress,
		"ipv6address": "",
	}).Inc()
	return nil
}

func isManagedByController(ing *netv1.Ingress) bool {
	for k, v := range ing.Annotations {
		if slices.Contains(allowedAnnotations, k) {
			if strings.Compare(strings.ToLower(v), "true") != 0 {
				return true
			}
		}
	}
	return true
}

func getIngressHosts(ing *netv1.Ingress) ([]string, error) {
	var hosts []string
	for _, rule := range ing.Spec.Rules {
		hosts = append(hosts, rule.Host)
	}
	return hosts, nil
}

func handleObject(obj client.Object) (*netv1.Ingress, error) {
	ingress, ok := obj.(*netv1.Ingress)
	if !ok {
		return nil, fmt.Errorf("expected Ingress, but got %T", obj)
	}
	return ingress, nil
}

// Returns the first IP address in the Ingress status field.
// Returns an ErrIngressWithoutIP if Ingress doens't have any IP addresses in its status field
func mustHaveIPAddress(ing *netv1.Ingress) (string, error) {
	// Exit early if Ingress doesn't have any IP addresses in its status field
	if len(ing.Status.LoadBalancer.Ingress) <= 0 {
		return "", ErrIngressWithoutIP
	}

	// Just use the first IP address in the list for now
	return ing.Status.LoadBalancer.Ingress[0].IP, nil
}

func hostsToRemove(oldIng, newIng *netv1.Ingress) ([]string, error) {
	var removed []string
	oldHosts, err := getIngressHosts(oldIng)
	if err != nil {
		return nil, err
	}
	newHosts, err := getIngressHosts(newIng)
	for _, oldHost := range oldHosts {
		if !slices.Contains(newHosts, oldHost) {
			removed = append(removed, oldHost)
		}
	}
	return removed, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager, conn *ibclient.Connector, cfg *InfobloxConfig) error {
	r.conn = conn
	r.cfg = cfg

	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}).
		Watches(
			&netv1.Ingress{},
			&handler.Funcs{
				CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.RateLimitingInterface) {
					q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(e.Object)})
				},
				UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.RateLimitingInterface) {
					q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(e.ObjectNew)})
					l := log.FromContext(ctx)

					oldIng, err := handleObject(e.ObjectOld)
					if err != nil {
						l.Error(err, "couldn't handle ObjectOld as Ingress")
						return
					}

					newIng, err := handleObject(e.ObjectNew)
					if err != nil {
						l.Error(err, "couldn't handle ObjectNew as Ingress")
						return
					}

					ip, err := mustHaveIPAddress(oldIng)
					if err != nil {
						l.Error(err, "couldn't get IP address from Ingress")
						return
					}

					removed, err := hostsToRemove(oldIng, newIng)
					if err != nil {
						l.Error(err, "couldn't determin hosts to remove")
						return
					}

					objMgr := ibclient.NewObjectManager(r.conn, "", "")

					for _, remove := range removed {
						rec, err := getHostRecord(objMgr, r.cfg, remove, ip)
						if err != nil {
							l.Error(err, "couldn't get host record")
							return
						}

						err = deleteHostRecord(r.conn, rec, cfg, remove, ip)
						if err != nil {
							l.Error(err, "couldn't delete host record")
							return
						}
					}
				},
				DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.RateLimitingInterface) {
					q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(e.Object)})
				},
			},
		).
		Complete(r)
}
