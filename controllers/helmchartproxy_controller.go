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

package controllers

import (
	"context"
	"fmt"

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlClient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"cluster-api-addon-helm/api/v1beta1"
	addonsv1beta1 "cluster-api-addon-helm/api/v1beta1"
	"cluster-api-addon-helm/internal"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	// "sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster"
	// "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

// HelmChartProxyReconciler reconciles a HelmChartProxy object
type HelmChartProxyReconciler struct {
	ctrlClient.Client
	Scheme *runtime.Scheme

	controller controller.Controller
	Tracker    *remote.ClusterCacheTracker
}

const finalizer = "addons.cluster.x-k8s.io"

//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HelmChartProxy object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *HelmChartProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the HelmChartProxy instance.
	helmChartProxy := &addonsv1beta1.HelmChartProxy{}
	if err := r.Client.Get(ctx, req.NamespacedName, helmChartProxy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// TODO: should patch helper return an error when the object has been deleted?
	patchHelper, err := patch.NewHelper(helmChartProxy, r.Client)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to init patch helper")
	}

	defer func() {
		if err := patchHelper.Patch(ctx, helmChartProxy); err != nil && reterr == nil {
			reterr = err
		}
	}()

	labelSelector := helmChartProxy.Spec.Selector
	log.V(2).Info("HelmChartProxy labels are", "labels", labelSelector)

	log.V(2).Info("Getting list of clusters with labels")
	clusterList, err := r.listClustersWithLabels(ctx, labelSelector)
	if err != nil {
		helmChartProxy.Status.FailureReason = to.StringPtr((errors.Wrapf(err, "failed to list clusters with label selector %+v", labelSelector.MatchLabels).Error()))
		helmChartProxy.Status.Ready = false

		return ctrl.Result{}, err
	}
	if clusterList == nil {
		log.V(2).Info("No clusters found")
	}
	for _, cluster := range clusterList.Items {
		log.V(2).Info("Found cluster", "name", cluster.Name)
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if helmChartProxy.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(helmChartProxy, finalizer) {
			controllerutil.AddFinalizer(helmChartProxy, finalizer)
			if err := r.Update(ctx, helmChartProxy); err != nil {
				helmChartProxy.Status.FailureReason = to.StringPtr(errors.Wrapf(err, "failed to add finalizer").Error())
				helmChartProxy.Status.Ready = false
				if err := r.Status().Update(ctx, helmChartProxy); err != nil {
					log.Error(err, "unable to update HelmChartProxy status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(helmChartProxy, finalizer) {
			// our finalizer is present, so lets handle any external dependency
			if err := r.reconcileDelete(ctx, helmChartProxy, clusterList.Items); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				helmChartProxy.Status.FailureReason = to.StringPtr(err.Error())
				helmChartProxy.Status.Ready = false
				if err := r.Status().Update(ctx, helmChartProxy); err != nil {
					log.Error(err, "unable to update HelmChartProxy status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}

			helmChartProxy.Status.Ready = true
			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(helmChartProxy, finalizer)
			if err := r.Update(ctx, helmChartProxy); err != nil {
				helmChartProxy.Status.FailureReason = to.StringPtr(errors.Wrapf(err, "failed to remove finalizer").Error())
				helmChartProxy.Status.Ready = false
				if err := r.Status().Update(ctx, helmChartProxy); err != nil {
					log.Error(err, "unable to update HelmChartProxy status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	log.V(2).Info("Reconciling HelmChartProxy", "randomName", helmChartProxy.Name)
	err = r.reconcileNormal(ctx, helmChartProxy, clusterList.Items)
	if err != nil {
		helmChartProxy.Status.Ready = false

		return ctrl.Result{}, err
	}

	helmChartProxy.Status.FailureReason = nil
	helmChartProxy.Status.Ready = true
	if err := r.Status().Update(ctx, helmChartProxy); err != nil {
		log.Error(err, "unable to update HelmChartProxy status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HelmChartProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controller, err := ctrl.NewControllerManagedBy(mgr).
		For(&addonsv1beta1.HelmChartProxy{}).
		Build(r)

	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	r.controller = controller

	log := ctrl.Log.WithName("remote").WithName("ClusterCacheTracker")
	tracker, err := remote.NewClusterCacheTracker(
		mgr,
		remote.ClusterCacheTrackerOptions{
			Log:     &log,
			Indexes: remote.DefaultIndexes,
		},
	)

	if err != nil {
		return errors.Wrap(err, "failed to create cluster cache tracker")
	}
	r.Tracker = tracker

	return nil
}

func (r *HelmChartProxyReconciler) watchClusterSecrets(ctx context.Context, cluster *clusterv1.Cluster) error {
	// If there is no tracker, don't watch remote nodes
	log := log.FromContext(context.TODO())
	log.Info("Watching cluster secrets on cluster", "cluster", cluster.Name)
	if r.Tracker == nil {
		log.Info("Tracker is nil, returning early")
		return nil
	}

	return r.Tracker.Watch(ctx, remote.WatchInput{
		Name:         "proxy-watchClusterSecrets",
		Cluster:      util.ObjectKey(cluster),
		Watcher:      r.controller,
		Kind:         &corev1.Secret{},
		EventHandler: handler.EnqueueRequestsFromMapFunc(r.secretToProxy),
	})
}

func (r *HelmChartProxyReconciler) secretToProxy(obj client.Object) []reconcile.Request {
	log := log.FromContext(context.TODO())

	// TODO: how can we figure out what cluster the secret came from? Should we make a Reconciler() for each cluster
	// that runs under the main Reconciler()?

	secret, ok := obj.(*corev1.Secret)
	if !ok {
		panic(fmt.Sprintf("Expected a Secret but got a %T", obj))
	}

	labels := secret.GetLabels()
	if owner, ok := labels["owner"]; !ok || owner != "helm" {
		// log.Info("Secret is not owned by helm, returning early")
		return nil
	}
	log.Info("Got secret with name", "secret", secret.Name)

	releaseName := labels["release"]

	helmChartProxyList := &v1beta1.HelmChartProxyList{}
	if err := r.Client.List(context.TODO(), helmChartProxyList); err != nil {
		log.Info("Failure to list HelmChartProxy", "error", err)
		return nil
	}

	var helmChartProxies []*v1beta1.HelmChartProxy
	for _, helmChartProxy := range helmChartProxyList.Items {
		log.Info("Secret resolved to HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
		if helmChartProxy.Spec.ReleaseName == releaseName {
			helmChartProxies = append(helmChartProxies, &helmChartProxy)
		}
	}
	// TODO: make sure the helmChartProxy label selector matches the cluster the secret is on

	if len(helmChartProxies) == 0 {
		log.Info("No HelmChartProxy found for secret", "secret", secret.Name)
		return nil
	} else if len(helmChartProxies) > 1 {
		log.Info("Multiple HelmChartProxies found for secret", "secret", secret.Name)
		return nil
	}
	// Should be deterministic, but just in case

	// TODO: how to figure out which secret has not been seen?

	// Idea: in the HelmChartProxy status, store a map of the installed cluster to its release revision/version.
	// That way we can do a reverse look up.

	return []reconcile.Request{
		{NamespacedName: util.ObjectKey(helmChartProxies[0])},
	}
	// // Match by clusterName when the node has the annotation.
	// if clusterName, ok := secret.GetAnnotations()[clusterv1.ClusterNameAnnotation]; ok {
	// 	filters = append(filters, client.MatchingLabels{
	// 		clusterv1.ClusterLabelName: clusterName,
	// 	})
	// }

	// // Match by namespace when the secret has the annotation.
	// if namespace, ok := secret.GetAnnotations()[clusterv1.ClusterNamespaceAnnotation]; ok {
	// 	filters = append(filters, client.InNamespace(namespace))
	// }

	// // Match by nodeName and status.nodeRef.name.
	// machineList := &clusterv1.MachineList{}
	// if err := r.Client.List(
	// 	context.TODO(),
	// 	machineList,
	// 	append(filters, client.MatchingFields{index.MachineNodeNameField: secret.Name})...); err != nil {
	// 	return nil
	// }

	// // There should be exactly 1 Machine for the secret.
	// if len(machineList.Items) == 1 {
	// 	return []reconcile.Request{{NamespacedName: util.ObjectKey(&machineList.Items[0])}}
	// }

	// // Otherwise let's match by providerID. This is useful when e.g the NodeRef has not been set yet.
	// // Match by providerID
	// nodeProviderID, err := noderefutil.NewProviderID(secret.Spec.ProviderID)
	// if err != nil {
	// 	return nil
	// }
	// machineList = &clusterv1.MachineList{}
	// if err := r.Client.List(
	// 	context.TODO(),
	// 	machineList,
	// 	append(filters, client.MatchingFields{index.MachineProviderIDField: nodeProviderID.IndexKey()})...); err != nil {
	// 	return nil
	// }

	// // There should be exactly 1 Machine for the node.
	// if len(machineList.Items) == 1 {
	// 	return []reconcile.Request{{NamespacedName: util.ObjectKey(&machineList.Items[0])}}
	// }

	return nil
}

// reconcileNormal...
func (r *HelmChartProxyReconciler) reconcileNormal(ctx context.Context, helmChartProxy *addonsv1beta1.HelmChartProxy, clusters []clusterv1.Cluster) error {
	log := ctrl.LoggerFrom(ctx)

	for _, cluster := range clusters {
		kubeconfigPath, err := internal.WriteClusterKubeconfigToFile(ctx, &cluster)
		if err != nil {
			log.Error(err, "failed to get kubeconfig for cluster", "cluster", cluster.Name)
			return err
		}

		err = r.reconcileCluster(ctx, helmChartProxy, &cluster, kubeconfigPath)
		if err != nil {
			log.Error(err, "failed to reconcile chart on cluster", "cluster", cluster.Name)
			return errors.Wrapf(err, "failed to reconcile HelmChartProxy %s on cluster %s", helmChartProxy.Name, cluster.Name)
		}
	}

	return nil
}

func (r *HelmChartProxyReconciler) reconcileCluster(ctx context.Context, helmChartProxy *addonsv1beta1.HelmChartProxy, cluster *clusterv1.Cluster, kubeconfigPath string) error {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Reconciling HelmChartProxy on cluster", "HelmChartProxy", helmChartProxy.Name, "cluster", cluster.Name)

	if err := r.watchClusterSecrets(ctx, cluster); err != nil {
		log.Error(err, "error watching secrets on target cluster", "cluster", cluster.Name)
		return err
	}

	releases, err := internal.ListHelmReleases(ctx, kubeconfigPath)
	if err != nil {
		log.Error(err, "failed to list releases")
	}
	log.V(2).Info("Querying existing releases:")
	for _, release := range releases {
		log.V(2).Info("Release found on cluster", "releaseName", release.Name, "cluster", cluster.Name, "revision", release.Version)
	}

	values, err := internal.ParseValues(ctx, r.Client, kubeconfigPath, helmChartProxy.Spec, cluster)
	if err != nil {
		log.Error(err, "failed to parse values on cluster", "cluster", cluster.Name)
	}

	existing, err := internal.GetHelmRelease(ctx, kubeconfigPath, helmChartProxy.Spec)
	if err != nil {
		log.V(2).Error(err, "error getting release from cluster", "cluster", cluster.Name)

		if err.Error() == "release: not found" {
			// Go ahead and create chart
			release, err := internal.InstallHelmRelease(ctx, kubeconfigPath, helmChartProxy.Spec, values)
			if err != nil {
				log.V(2).Error(err, "error installing chart with Helm on cluster", "cluster", cluster.Name)
				return errors.Wrapf(err, "failed to install chart on cluster %s", cluster.Name)
			}
			if release != nil {
				log.V(2).Info((fmt.Sprintf("Release '%s' successfully installed on cluster %s, revision = %d", release.Name, cluster.Name, release.Version)))
				// addClusterRefToStatusList(ctx, helmChartProxy, cluster)
			}

			return nil
		}

		return err
	}

	if existing != nil {
		// TODO: add logic for updating an existing release
		log.V(2).Info(fmt.Sprintf("Release '%s' already installed on cluster %s, running upgrade", existing.Name, cluster.Name))
		release, upgraded, err := internal.UpgradeHelmRelease(ctx, kubeconfigPath, helmChartProxy.Spec, values)
		if err != nil {
			log.V(2).Error(err, "error upgrading chart with Helm on cluster", "cluster", cluster.Name)
			return errors.Wrapf(err, "error upgrading chart with Helm on cluster %s", cluster.Name)
		}
		if release != nil && upgraded {
			log.V(2).Info((fmt.Sprintf("Release '%s' successfully upgraded on cluster %s, revision = %d", release.Name, cluster.Name, release.Version)))
			// addClusterRefToStatusList(ctx, helmChartProxy, cluster)
		}
	}

	return nil
}

// reconcileDelete...
func (r *HelmChartProxyReconciler) reconcileDelete(ctx context.Context, helmChartProxy *addonsv1beta1.HelmChartProxy, clusters []clusterv1.Cluster) error {
	log := ctrl.LoggerFrom(ctx)

	for _, cluster := range clusters {
		kubeconfigPath, err := internal.WriteClusterKubeconfigToFile(ctx, &cluster)
		if err != nil {
			log.Error(err, "failed to get kubeconfig for cluster", "cluster", cluster.Name)
			return errors.Wrapf(err, "failed to get kubeconfig for cluster %s", cluster.Name)
		}
		err = r.reconcileDeleteCluster(ctx, helmChartProxy, &cluster, kubeconfigPath)
		if err != nil {
			log.Error(err, "failed to delete chart on cluster", "cluster", cluster.Name)
			return errors.Wrapf(err, "failed to delete HelmChartProxy %s on cluster %s", helmChartProxy.Name, cluster.Name)
		}
	}

	return nil
}

func (r *HelmChartProxyReconciler) reconcileDeleteCluster(ctx context.Context, helmChartProxy *addonsv1beta1.HelmChartProxy, cluster *clusterv1.Cluster, kubeconfigPath string) error {
	log := ctrl.LoggerFrom(ctx)

	_, err := internal.GetHelmRelease(ctx, kubeconfigPath, helmChartProxy.Spec)
	if err != nil {
		log.V(2).Error(err, "error getting release from cluster", "cluster", cluster.Name)

		if err.Error() == "release: not found" {
			log.V(2).Info(fmt.Sprintf("Release '%s' not found on cluster %s, nothing to do for uninstall", helmChartProxy.Spec.ReleaseName, cluster.Name))
			return nil
		}

		return err
	}

	log.V(2).Info("Preparing to uninstall release on cluster", "releaseName", helmChartProxy.Spec.ReleaseName, "clusterName", cluster.Name)

	response, err := internal.UninstallHelmRelease(ctx, kubeconfigPath, helmChartProxy.Spec)
	if err != nil {
		log.V(2).Info("Error uninstalling chart with Helm:", err)
		return errors.Wrapf(err, "error uninstalling chart with Helm on cluster %s", cluster.Name)
	}

	log.V(2).Info((fmt.Sprintf("Chart '%s' successfully uninstalled on cluster %s", helmChartProxy.Spec.ChartName, cluster.Name)))

	if response != nil && response.Info != "" {
		log.V(2).Info(fmt.Sprintf("Response is %s", response.Info))
	}

	return nil
}

func (r *HelmChartProxyReconciler) listClustersWithLabels(ctx context.Context, labelSelector metav1.LabelSelector) (*clusterv1.ClusterList, error) {
	clusterList := &clusterv1.ClusterList{}
	labels := labelSelector.MatchLabels
	// Empty labels should match nothing, not everything
	if len(labels) == 0 {
		return nil, nil
	}

	// TODO: should we use ctrlClient.MatchingLabels or try to use the labelSelector itself?
	if err := r.Client.List(ctx, clusterList, ctrlClient.MatchingLabels(labels)); err != nil {
		return nil, err
	}

	return clusterList, nil
}

// func addClusterRefToStatusList(ctx context.Context, proxy *addonsv1beta1.HelmChartProxy, cluster *clusterv1.Cluster) {
// 	if proxy.Status.InstalledClusters == nil {
// 		proxy.Status.InstalledClusters = make([]corev1.ObjectReference, 1)
// 	}

// 	proxy.Status.InstalledClusters = append(proxy.Status.InstalledClusters, corev1.ObjectReference{
// 		Kind:       cluster.Kind,
// 		APIVersion: cluster.APIVersion,
// 		Name:       cluster.Name,
// 		Namespace:  cluster.Namespace,
// 	})
// }
