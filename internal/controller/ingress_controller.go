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
	"sort"
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

	// Prometheus metrics
	counterRecordsRetrieved = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "infoblox_records_retrieved_total",
			Help: "Number of records retrieved from infoblox",
		},
		[]string{"type", "net_view", "dns_view", "host", "ipv4address", "ipv6address", "ingress_name"},
	)
	counterRecordsAdded = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "infoblox_records_added_total",
			Help: "Number of records added",
		},
		[]string{"type", "net_view", "dns_view", "host", "ipv4address", "ipv6address", "ingress_name"},
	)
	counterRecordsUpdated = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "infoblox_records_updated_total",
			Help: "Number of records updated",
		},
		[]string{"type", "net_view", "dns_view", "host", "ipv4address", "ipv6address", "ingress_name"},
	)
	counterRecordsRemoved = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "infoblox_records_removed_total",
			Help: "Number of records removed",
		},
		[]string{"type", "net_view", "dns_view", "host", "ipv4address", "ipv6address", "ingress_name"},
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
	View        string
	Zone        string
	Version     string
	AliasSuffix string
}

func init() {
	metrics.Registry.MustRegister(
		counterRecordsRetrieved,
		counterRecordsAdded,
		counterRecordsRemoved,
		counterRecordsUpdated,
	)
}

//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx) // Base logger

	ingress := &netv1.Ingress{}
	if err := r.Get(ctx, req.NamespacedName, ingress); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Add ingress context for subsequent logs
	ingressKey := req.NamespacedName.String()

	if !isManagedByController(ingress) {
		l.V(1).Info("Ingress is not managed by this controller, skipping", "ingress", ingressKey)
		if controllerutil.ContainsFinalizer(ingress, ingressFinalizers) {
			l.Info("Removing finalizer from unmanaged ingress", "ingress", ingressKey)
			if err := r.Get(ctx, req.NamespacedName, ingress); err != nil {
				if client.IgnoreNotFound(err) != nil {
					l.Error(err, "Failed to re-fetch ingress before removing finalizer from unmanaged object", "ingress", ingressKey)
				}
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			if ok := controllerutil.RemoveFinalizer(ingress, ingressFinalizers); !ok {
				l.Info("Finalizer already removed or conflict during removal, retrying", "ingress", ingressKey)
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.Update(ctx, ingress); err != nil {
				l.Error(err, "Failed to remove finalizer from unmanaged ingress", "ingress", ingressKey)
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	l.V(1).Info("Reconciling managed ingress", "ingress", ingressKey)

	ingressHosts, err := getIngressHosts(ingress)
	if err != nil {
		l.Error(err, "Could not get hosts from ingress", "ingress", ingressKey)
		return ctrl.Result{}, err
	}

	for _, h := range ingressHosts {
		hasSuffix := strings.HasSuffix(h, ".k8s.vgregion.se")

		if !hasSuffix {
			l.Error(nil, "Host does not end with \".k8s.vgregion.se\". Please use server-alias to provision hosts outside of the .k8s subdomain. Check user-docs for more information", "ingress", ingressKey, "host", h)
			return ctrl.Result{}, nil
		}
	}

	objMgr := ibclient.NewObjectManager(r.conn, "", "")

	if ingress.GetDeletionTimestamp() != nil {
		if controllerutil.ContainsFinalizer(ingress, ingressFinalizers) {
			l.Info("Handling deletion for ingress", "ingress", ingressKey)

			var ipAddressForDeletion string

			if len(ingress.Status.LoadBalancer.Ingress) > 0 {
				ipAddressForDeletion = ingress.Status.LoadBalancer.Ingress[0].IP
			}

			deletionErrors := false
			for _, host := range ingressHosts {
				rec, err := getHostRecord(objMgr, r.cfg, host, ingress.Name)
				if err != nil {
					var notfound *ibclient.NotFoundError
					if errors.As(err, &notfound) {
						l.Info("Host record not found during deletion, skipping", "ingress", ingressKey, "host", host)
						continue
					}
					l.Error(err, "Failed to get host record for deletion", "ingress", ingressKey, "host", host)
					deletionErrors = true
					continue
				}

				l.Info("Attempting to delete host record", "ingress", ingressKey, "host", host)
				err = deleteHostRecord(r.conn, rec, r.cfg, host, ipAddressForDeletion, ingress.Name)
				if err != nil {
					l.Error(err, "Failed to delete host record", "ingress", ingressKey, "host", host)
					deletionErrors = true
				} else {
					l.Info("Successfully deleted host record", "ingress", ingressKey, "host", host)
				}
			}

			if !deletionErrors {
				l.Info("All known host records handled for deletion, removing finalizer", "ingress", ingressKey)
				if err := r.Get(ctx, req.NamespacedName, ingress); err != nil {
					if client.IgnoreNotFound(err) == nil {
						l.Info("Ingress already deleted, finalizer removal likely succeeded or is irrelevant", "ingress", ingressKey)
						return ctrl.Result{}, nil
					}
					l.Error(err, "Failed to re-fetch ingress before removing finalizer", "ingress", ingressKey)
					return ctrl.Result{}, err
				}
				if controllerutil.RemoveFinalizer(ingress, ingressFinalizers) {
					if err := r.Update(ctx, ingress); err != nil {
						l.Error(err, "Failed to update ingress after removing finalizer", "ingress", ingressKey)
						return ctrl.Result{}, err
					}
					l.Info("Finalizer removed successfully", "ingress", ingressKey)
				} else {
					l.Info("Finalizer already removed or conflict, reconciliation likely complete", "ingress", ingressKey)
				}
			} else {
				l.Error(nil, "Errors occurred during host record deletion, finalizer not removed, requeuing", "ingress", ingressKey)
				return ctrl.Result{Requeue: true}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(ingress, ingressFinalizers) {
		l.Info("Adding finalizer", "ingress", ingressKey)
		if err := r.Get(ctx, req.NamespacedName, ingress); err != nil {
			l.Error(err, "Failed to re-fetch ingress before adding finalizer", "ingress", ingressKey)
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if ok := controllerutil.AddFinalizer(ingress, ingressFinalizers); !ok {
			l.Info("Finalizer already present or conflict during add, retrying", "ingress", ingressKey)
			return ctrl.Result{Requeue: true}, nil
		}
		if err := r.Update(ctx, ingress); err != nil {
			l.Error(err, "Failed to update ingress after adding finalizer", "ingress", ingressKey)
			return ctrl.Result{}, err
		}
		l.Info("Finalizer added, requeueing for main reconcile", "ingress", ingressKey)
		return ctrl.Result{Requeue: true}, nil
	}

	if len(ingress.Status.LoadBalancer.Ingress) == 0 || ingress.Status.LoadBalancer.Ingress[0].IP == "" {
		l.Info("Ingress does not have an IP address yet. Retrying in 30s", "ingress", ingressKey)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	ipaddress := ingress.Status.LoadBalancer.Ingress[0].IP
	l.V(1).Info("Using IP address for DNS records", "ingress", ingressKey, "ipaddress", ipaddress)

	l.V(1).Info("Desired hosts from spec", "ingress", ingressKey, "hosts", ingressHosts)

	serverAliases := getIngressAliases(ingress)
	l.V(1).Info("Desired aliases from annotations", "ingress", ingressKey, "aliases", serverAliases)

	var encounteredError error
	requeueNeeded := false

	for _, host := range ingressHosts {
		rec, err := getHostRecord(objMgr, r.cfg, host, ingress.Name)
		if err != nil {
			var notfound *ibclient.NotFoundError
			if errors.As(err, &notfound) {
				l.Info("Host record not found, creating", "ingress", ingressKey, "host", host, "ip", ipaddress, "aliases", serverAliases)
				err = createHostRecord(objMgr, r.cfg, host, ipaddress, ingress.Name, serverAliases)
				if err != nil {
					l.Error(err, "Failed to create host record", "ingress", ingressKey, "host", host)
					if encounteredError == nil {
						encounteredError = fmt.Errorf("failed to create host record %s for ingress %s: %w", host, req.NamespacedName, err)
					}
				} else {
					l.Info("Successfully created host record", "ingress", ingressKey, "host", host)
					requeueNeeded = true
				}
				continue
			} else {
				l.Error(err, "Failed to get host record from Infoblox", "ingress", ingressKey, "host", host)
				if encounteredError == nil {
					encounteredError = fmt.Errorf("failed to get host record %s for ingress %s: %w", host, req.NamespacedName, err)
				}
				continue
			}
		}

		hostRecordIp, err := objMgr.GetIpAddressFromHostRecord(*rec)
		if err != nil {
			l.Error(err, "Failed to get IP address from existing host record", "ingress", ingressKey, "host", host, "recordRef", rec.Ref)
			if encounteredError == nil {
				encounteredError = fmt.Errorf("failed to get IP from host record %s (%s) for ingress %s: %w", host, rec.Ref, req.NamespacedName, err)
			}
			continue
		}

		if !alisesAreEqual(rec.Aliases, serverAliases) || hostRecordIp != ipaddress {
			l.Info("Updating host record", "ingress", ingressKey, "host", host, "oldIp", hostRecordIp, "newIp", ipaddress, "oldAliases", rec.Aliases, "newAliases", serverAliases, "recordRef", rec.Ref)
			err = updateHostRecord(objMgr, r.cfg, rec.Ref, host, ipaddress, ingress.Name, serverAliases)
			if err != nil {
				l.Error(err, "Failed to update host record", "ingress", ingressKey, "host", host, "recordRef", rec.Ref)
				if encounteredError == nil {
					encounteredError = fmt.Errorf("failed to update host record %s (%s) for ingress %s: %w", host, rec.Ref, req.NamespacedName, err)
				}
			} else {
				l.Info("Successfully updated host record", "ingress", ingressKey, "host", host, "recordRef", rec.Ref)
				requeueNeeded = true
			}
		} else {
			l.V(1).Info("Host record is up-to-date", "ingress", ingressKey, "host", host)
		}
	}

	if encounteredError != nil {
		l.Error(encounteredError, "Errors occurred during reconciliation, requeuing", "ingress", ingressKey)
		return ctrl.Result{Requeue: true}, nil
	}
	if requeueNeeded {
		l.Info("Requeueing shortly to verify changes", "ingress", ingressKey)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	l.V(1).Info("Reconciliation complete for ingress", "ingress", ingressKey)
	return ctrl.Result{}, nil
}

// getHostRecord finds and returns a host record by name.
func getHostRecord(objMgr ibclient.IBObjectManager, cfg *InfobloxConfig, name, ingressName string) (*ibclient.HostRecord, error) {
	rec, err := objMgr.GetHostRecord("", cfg.View, name, "", "")
	if err != nil {
		return nil, err
	}

	counterRecordsRetrieved.With(prometheus.Labels{
		"type":         "HOST",
		"net_view":     "",
		"dns_view":     cfg.View,
		"host":         name,
		"ipv4address":  "",
		"ipv6address":  "",
		"ingress_name": ingressName,
	}).Inc()
	return rec, nil
}

// createHostRecord creates a new host record using the original signature.
func createHostRecord(objMgr ibclient.IBObjectManager, cfg *InfobloxConfig, host, ipaddress, ingressName string, aliases []string) error {
	fqdn := host
	ttl := uint32(30)

	_, err := objMgr.CreateHostRecord(
		true, false, fqdn, "", cfg.View, "", "", ipaddress, "", "", "", true, ttl, "", nil, aliases, false,
	)

	if err == nil {
		counterRecordsAdded.With(prometheus.Labels{
			"type": "HOST", "net_view": "", "dns_view": cfg.View, "host": fqdn, "ipv4address": ipaddress, "ipv6address": "", "ingress_name": ingressName,
		}).Inc()
	}
	return err
}

// updateHostRecord updates an existing host record using the original signature pattern.
func updateHostRecord(objMgr ibclient.IBObjectManager, cfg *InfobloxConfig, hostRef string, host, ipaddress, ingressName string, aliases []string) error {
	fqdn := host
	ttl := uint32(30)

	_, err := objMgr.UpdateHostRecord(
		hostRef, true, false, fqdn, "", cfg.View, "", "", ipaddress, "", "", "", true, ttl, "", nil, aliases, false,
	)

	if err == nil {
		counterRecordsUpdated.With(prometheus.Labels{
			"type": "HOST", "net_view": "", "dns_view": cfg.View, "host": fqdn, "ipv4address": ipaddress, "ipv6address": "", "ingress_name": ingressName,
		}).Inc()
	}
	return err
}

// deleteHostRecord deletes a host record by its reference.
func deleteHostRecord(conn *ibclient.Connector, rec *ibclient.HostRecord, cfg *InfobloxConfig, host, ipaddress, ingressName string) error {
	ref := rec.Ref
	_, err := conn.DeleteObject(ref)
	if err == nil {
		counterRecordsRemoved.With(prometheus.Labels{
			"type": "HOST", "net_view": "", "dns_view": cfg.View, "host": host, "ipv4address": ipaddress, "ipv6address": "", "ingress_name": ingressName,
		}).Inc()
	}
	return err
}

// isManagedByController checks for the presence of specific annotations.
func isManagedByController(ing *netv1.Ingress) bool {
	annotations := ing.GetAnnotations()
	if annotations == nil {
		return false
	}
	for k, v := range annotations {
		if slices.Contains(allowedAnnotations, k) {
			if strings.EqualFold(v, "true") {
				return true
			}
		}
	}
	return false
}

// getIngressHosts extracts hostnames from ingress rules.
func getIngressHosts(ing *netv1.Ingress) ([]string, error) {
	var hosts []string
	if ing.Spec.Rules == nil {
		return hosts, nil
	}
	for _, rule := range ing.Spec.Rules {
		if rule.Host != "" {
			hosts = append(hosts, rule.Host)
		}
	}
	return hosts, nil
}

// getIngressAliases extracts server aliases from common annotations.
func getIngressAliases(ing *netv1.Ingress) []string {
	var serverAliases []string
	annotations := ing.GetAnnotations()
	if annotations == nil {
		return serverAliases
	}
	aliasAnnotationKeys := []string{
		"nginx.ingress.kubernetes.io/server-alias",
		"haproxy-ingress.github.io/server-alias",
		"infoblox-dns-controller/aliases",
	}

	for _, key := range aliasAnnotationKeys {
		if val, ok := annotations[key]; ok {
			aliases := strings.Split(val, ",")
			for _, alias := range aliases {
				trimmedAlias := strings.TrimSpace(alias)
				if trimmedAlias != "" {
					serverAliases = append(serverAliases, trimmedAlias)
				}
			}
			if len(serverAliases) > 0 {
				break
			}
		}
	}
	return serverAliases
}

// hostsToRemove compares host lists between old and new Ingress objects.
func hostsToRemove(oldIng, newIng *netv1.Ingress) ([]string, error) {
	var removed []string
	oldHosts, err := getIngressHosts(oldIng)
	if err != nil {
		return nil, fmt.Errorf("error getting hosts from old ingress: %w", err)
	}
	newHosts, err := getIngressHosts(newIng)
	if err != nil {
		return nil, fmt.Errorf("error getting hosts from new ingress: %w", err)
	}

	newHostsMap := make(map[string]struct{}, len(newHosts))
	for _, h := range newHosts {
		newHostsMap[h] = struct{}{}
	}

	for _, oldHost := range oldHosts {
		if _, exists := newHostsMap[oldHost]; !exists {
			removed = append(removed, oldHost)
		}
	}
	return removed, nil
}

// alisesAreEqual compares two slices of aliases, ignoring order.
func alisesAreEqual(existingAliases, serverAliases []string) bool {
	if len(existingAliases) != len(serverAliases) {
		return false
	}
	if len(existingAliases) == 0 {
		return true
	}

	ea := append([]string(nil), existingAliases...)
	sa := append([]string(nil), serverAliases...)

	sort.Strings(ea)
	sort.Strings(sa)

	return slices.Equal(ea, sa)
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
				CreateFunc: func(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					ingressKey := client.ObjectKeyFromObject(e.Object).String()
					log.FromContext(ctx).V(1).Info("Create event received, queueing", "ingress", ingressKey)
					q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(e.Object)})
				},
				UpdateFunc: func(ctx context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					ingressKey := client.ObjectKeyFromObject(e.ObjectNew).String()
					l := log.FromContext(ctx) // Base logger

					oldIng, okOld := e.ObjectOld.(*netv1.Ingress)
					newIng, okNew := e.ObjectNew.(*netv1.Ingress)
					if !okOld || !okNew {
						l.Error(fmt.Errorf("update event received non-Ingress objects: Old=%T, New=%T", e.ObjectOld, e.ObjectNew), "Update handler received unexpected type", "ingress", ingressKey)
						return
					}

					shouldManageNew := isManagedByController(newIng)
					wasManagedOld := isManagedByController(oldIng)

					if !shouldManageNew {
						if wasManagedOld {
							l.Info("Ingress annotation removed or changed to false, queueing for potential cleanup", "ingress", ingressKey)
							q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(newIng)})
						} else {
							l.V(1).Info("Ingress update skipped, not managed by controller", "ingress", ingressKey)
						}
						return
					}

					l.V(1).Info("Processing update for managed ingress", "ingress", ingressKey)

					var ipForDeletionLogging string
					if len(oldIng.Status.LoadBalancer.Ingress) > 0 {
						ipForDeletionLogging = oldIng.Status.LoadBalancer.Ingress[0].IP
					}

					removed, err := hostsToRemove(oldIng, newIng)
					if err != nil {
						l.Error(err, "Could not determine hosts to remove during update", "ingress", ingressKey)
						q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(newIng)})
						return
					}

					if len(removed) > 0 {
						l.Info("Detected removed hosts, attempting deletion", "ingress", ingressKey, "removedHosts", removed)
						objMgr := ibclient.NewObjectManager(r.conn, "", "")

						for _, hostToRemove := range removed {
							rec, err := getHostRecord(objMgr, r.cfg, hostToRemove, oldIng.Name)
							if err != nil {
								var notfound *ibclient.NotFoundError
								if errors.As(err, &notfound) {
									l.Info("Host record not found for deletion during update, skipping", "ingress", ingressKey, "host", hostToRemove)
									continue
								}
								l.Error(err, "Could not get host record for deletion during update", "ingress", ingressKey, "host", hostToRemove)
								continue
							}

							l.Info("Attempting to delete host record for removed host", "ingress", ingressKey, "host", hostToRemove)
							err = deleteHostRecord(r.conn, rec, r.cfg, hostToRemove, ipForDeletionLogging, oldIng.Name)
							if err != nil {
								l.Error(err, "Could not delete host record for removed host", "ingress", ingressKey, "host", hostToRemove)
							} else {
								l.Info("Successfully deleted host record for removed host", "ingress", ingressKey, "host", hostToRemove)
							}
						}
					}
					l.V(1).Info("Update event processed, queueing for reconcile", "ingress", ingressKey)
					q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(newIng)})
				},
				DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					ingressKey := client.ObjectKeyFromObject(e.Object).String()
					l := log.FromContext(ctx)
					l.V(1).Info("Delete event received, queueing for finalizer check", "ingress", ingressKey)
					q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(e.Object)})
				},
				GenericFunc: func(ctx context.Context, e event.TypedGenericEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					ingressKey := client.ObjectKeyFromObject(e.Object).String()
					l := log.FromContext(ctx)
					l.V(1).Info("Generic event received, queueing", "ingress", ingressKey)
					q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(e.Object)})
				},
			},
		).
		Complete(r)
}
