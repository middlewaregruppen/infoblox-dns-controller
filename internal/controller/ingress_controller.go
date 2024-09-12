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
	"strings"
	"time"

	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ibclient "github.com/infobloxopen/infoblox-go-client/v2"
)

const ingressFinalizers = "infoblox-dns-controller.k8s.mdlwr.io/finalizer"

var (
	ErrObjectNotFound = errors.New("requested object not found")
	ErrNotFound       = errors.New("not found")
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

// getHostRecord finds and returns a slice of all host records matching the provided ipv4 address.
// We need this because the infoblox object manager library currently doesn't have a way of searching for host records by IP address.
func getHostRecord(conn *ibclient.Connector, netview, dnsview, ipv4addr string) ([]ibclient.HostRecord, error) {
	var res []ibclient.HostRecord

	recordHost := ibclient.NewEmptyHostRecord()
	sf := map[string]string{}

	if netview != "" {
		sf["network_view"] = netview
	}
	if dnsview != "" {
		sf["view"] = dnsview
	}
	if ipv4addr != "" {
		sf["ipv4addr"] = ipv4addr
	}
	queryParams := ibclient.NewQueryParams(false, sf)
	err := conn.GetObject(recordHost, "", queryParams, &res)
	if err != nil || res == nil || len(res) == 0 {
		return nil, err
	}
	return res, err
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
	val, ok := ingress.Annotations["middlewaregruppen.se/managed-dns"]
	if !ok {
		return ctrl.Result{}, nil
	}
	if strings.Compare(strings.ToLower(val), "true") != 0 {
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

	for _, host := range hosts {

		// Check if Host record exists. If it's not found then a new host record is created
		// rec, err := objMgr.GetHostRecord(r.cfg.View, r.cfg.Zone, host, ipaddress, "")
		recs, err := getHostRecord(r.conn, r.cfg.View, r.cfg.Zone, ipaddress)
		if err != nil {

			// Create record since it's not found
			// if errors.Is(err, ErrNotFound) || errors.AIs(err, ErrObjectNotFound) {
			var notfound *ibclient.NotFoundError
			if errors.As(err, &notfound) {
				_, err = objMgr.CreateHostRecord(
					true,       // enabledns
					false,      // enabledhcp
					host,       // recordName
					r.cfg.View, // DNS View
					r.cfg.Zone, // DNS Zone
					"",         // ipv4cidr
					"",         // ipv6cidr
					ipaddress,  // ipv4Addr
					"",         // ipv6Addr
					"",         // macAddr
					"",
					true, // useTTL
					30,
					"",
					nil,
					[]string{},
				)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("error creating host record %s for ingress %s: %v", host, namespacedName, err)
				}
				// Record was created, requeue so we can check again
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}

			// Return the error if any other errors are returned
			return ctrl.Result{}, fmt.Errorf("error fetching host %s for ingress %s: %v", host, namespacedName, err)
		}

		// Check so that we only get exactly 1 host record since two records pointing to the same ip address is probably a bad thing
		if len(recs) > 1 {
			return ctrl.Result{}, fmt.Errorf("multiple host records with IP address %s found. Manual intervention is required", ipaddress)
		}
		rec := recs[0]

		// Check if resource is marked to be deleted
		if ingress.GetDeletionTimestamp() != nil {
			if controllerutil.ContainsFinalizer(ingress, ingressFinalizers) {

				_, err := r.conn.DeleteObject(rec.Ref)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("error deleting host record %s: %v", host, err)
				}
				l.Info("deleted host record", "host", host, "ingress", namespacedName)

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

		// Update the record since it already exists if we get here
		_, err = objMgr.UpdateHostRecord(rec.Ref,
			true,
			false,
			host,
			r.cfg.View,
			r.cfg.Zone,
			"",
			"",
			ipaddress,
			"",
			"",
			"",
			true,
			30,
			"",
			nil,
			[]string{},
		)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("error updating host record %s: %v", host, err)
		}
		l.Info("updated host record successfully", "host", host, "ingress", namespacedName)

	}

	return ctrl.Result{}, nil
}

func getIngressHosts(ing *netv1.Ingress) ([]string, error) {
	var hosts []string
	for _, rule := range ing.Spec.Rules {
		hosts = append(hosts, rule.Host)
	}
	return hosts, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager, conn *ibclient.Connector, cfg *InfobloxConfig) error {
	r.conn = conn
	r.cfg = cfg
	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}).
		Complete(r)
}
