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

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	addonsv1alpha1 "cluster-api-addon-provider-helm/api/v1alpha1"
	"cluster-api-addon-provider-helm/internal"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	// "sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster"
	// "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

// HelmChartProxyReconciler reconciles a HelmChartProxy object
type HelmChartProxyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// SetupWithManager sets up the controller with the Manager.
func (r *HelmChartProxyReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := ctrl.LoggerFrom(ctx)

	c, err := ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&addonsv1alpha1.HelmChartProxy{}).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(log, r.WatchFilterValue)).
		// WithEventFilter(predicates.ResourceIsNotExternallyManaged(log)).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	// Add a watch on clusterv1.Cluster object for changes.
	if err = c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(r.ClusterToHelmChartProxiesMapper),
		predicates.ResourceNotPausedAndHasFilterLabel(log, r.WatchFilterValue),
	); err != nil {
		return errors.Wrap(err, "failed adding a watch for Clusters")
	}

	// Add a watch on HelmReleaseProxy object for changes.
	if err = c.Watch(
		&source.Kind{Type: &addonsv1alpha1.HelmReleaseProxy{}},
		handler.EnqueueRequestsFromMapFunc(HelmReleaseProxyToHelmChartProxyMapper),
		predicates.ResourceNotPausedAndHasFilterLabel(log, r.WatchFilterValue),
	); err != nil {
		return errors.Wrap(err, "failed adding a watch for HelmReleaseProxies")
	}

	return nil
}

//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies/finalizers,verbs=update
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmreleaseproxies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=list;watch
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kubeadmcontrolplanes,verbs=list;get;watch

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

	log.V(2).Info("Beginning reconcilation for HelmChartProxy", "requestNamespace", req.Namespace, "requestName", req.Name)

	// Fetch the HelmChartProxy instance.
	helmChartProxy := &addonsv1alpha1.HelmChartProxy{}
	if err := r.Client.Get(ctx, req.NamespacedName, helmChartProxy); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(2).Info("HelmChartProxy resource not found, skipping reconciliation", "helmChartProxy", req.NamespacedName)
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
		log.V(2).Info("Preparing to patch HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
		if err := patchHelmChartProxy(ctx, patchHelper, helmChartProxy); err != nil && reterr == nil {
			reterr = err
			log.Error(err, "failed to patch HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
			return
		}
		log.V(2).Info("Successfully patched HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
	}()

	label := helmChartProxy.Spec.ClusterSelector

	log.V(2).Info("Finding matching clusters for HelmChartProxy with label", "helmChartProxy", helmChartProxy.Name, "label", label)
	// TODO: When a Cluster is being deleted, it will show up in the list of clusters even though we can't Reconcile on it.
	// This is because of ownerRefs and how the Cluster gets deleted. It will be eventually consistent but it would be better
	// to not have errors. An idea would be to check the deletion timestamp.
	clusterList, err := r.listClustersWithLabel(ctx, label)
	if err != nil {
		helmChartProxy.SetError(errors.Wrapf(err, "failed to list clusters with label selector %+v", label))
		conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.ClusterSelectionFailedReason, clusterv1.ConditionSeverityError, err.Error())

		return ctrl.Result{}, err
	}
	// conditions.MarkTrue(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsReadyCondition)
	helmChartProxy.SetMatchingClusters(clusterList.Items)

	log.V(2).Info("Finding HelmRelease for HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
	labels := map[string]string{
		addonsv1alpha1.HelmChartProxyLabelName: helmChartProxy.Name,
	}
	releaseList, err := r.listInstalledReleases(ctx, labels)
	if err != nil {
		helmChartProxy.SetError(errors.Wrapf(err, "failed to list installed releases with labels %+v", labels))
		return ctrl.Result{}, err
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if helmChartProxy.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(helmChartProxy, addonsv1alpha1.HelmChartProxyFinalizer) {
			controllerutil.AddFinalizer(helmChartProxy, addonsv1alpha1.HelmChartProxyFinalizer)
			if err := patchHelmChartProxy(ctx, patchHelper, helmChartProxy); err != nil {
				// TODO: Should we try to set the error here? If we can't add the finalizer we likely can't update the status either.
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(helmChartProxy, addonsv1alpha1.HelmChartProxyFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			if err := r.reconcileDelete(ctx, helmChartProxy, releaseList.Items); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				helmChartProxy.SetError(err)
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(helmChartProxy, addonsv1alpha1.HelmChartProxyFinalizer)
			if err := patchHelmChartProxy(ctx, patchHelper, helmChartProxy); err != nil {
				// TODO: Should we try to set the error here? If we can't remove the finalizer we likely can't update the status either.
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		helmChartProxy.SetError(nil)
		return ctrl.Result{}, nil
	}

	log.V(2).Info("Reconciling HelmChartProxy", "randomName", helmChartProxy.Name)
	err = r.reconcileNormal(ctx, helmChartProxy, clusterList.Items, releaseList.Items)
	if err != nil {
		helmChartProxy.SetError(err)
		return ctrl.Result{}, err
	}
	conditions.MarkTrue(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition)

	err = r.aggregateHelmReleaseProxyReadyCondition(ctx, helmChartProxy)
	if err != nil {
		log.Error(err, "failed to aggregate HelmReleaseProxy ready condition", "helmChartProxy", helmChartProxy.Name)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileNormal...
func (r *HelmChartProxyReconciler) reconcileNormal(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, clusters []clusterv1.Cluster, helmReleaseProxies []addonsv1alpha1.HelmReleaseProxy) error {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Starting reconcileNormal for chart proxy", "name", helmChartProxy.Name)

	releasesToDelete := getOrphanedHelmReleaseProxies(ctx, clusters, helmReleaseProxies)
	log.V(2).Info("Deleting orphaned releases")
	for _, release := range releasesToDelete {
		log.V(2).Info("Deleting release", "release", release)
		if err := r.deleteHelmReleaseProxy(ctx, &release); err != nil {
			conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyDeletionFailedReason, clusterv1.ConditionSeverityError, err.Error())
			return err
		}
	}

	for _, cluster := range clusters {
		// Don't reconcile if the Cluster is being deleted
		if !cluster.ObjectMeta.DeletionTimestamp.IsZero() {
			continue
		}

		existingHelmReleaseProxy, err := r.getExistingHelmReleaseProxy(ctx, helmChartProxy, &cluster)
		if err != nil {
			// TODO: Should we set a condition here?
			return errors.Wrapf(err, "failed to get HelmReleaseProxy for cluster %s", cluster.Name)
		}
		// log.V(2).Info("Found existing HelmReleaseProxy", "cluster", cluster.Name, "release", existingHelmReleaseProxy.Name)

		if existingHelmReleaseProxy != nil && shouldReinstallHelmRelease(ctx, existingHelmReleaseProxy, helmChartProxy) {
			log.V(2).Info("Reinstalling Helm release by deleting and creating HelmReleaseProxy", "helmReleaseProxy", existingHelmReleaseProxy.Name)
			if err := r.deleteHelmReleaseProxy(ctx, existingHelmReleaseProxy); err != nil {
				conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyDeletionFailedReason, clusterv1.ConditionSeverityError, err.Error())

				return err
			}

			// TODO: Add a check on requeue to make sure that the HelmReleaseProxy isn't still deleting
			log.V(2).Info("Successfully deleted HelmReleaseProxy on cluster, returning to requeue for reconcile", "cluster", cluster.Name)
			conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyReinstallingReason, clusterv1.ConditionSeverityInfo, "HelmReleaseProxy on cluster '%s' successfully deleted, preparing to reinstall", cluster.Name)
			return nil // Try returning early so it will requeue
			// TODO: should we continue in the loop or just requeue?
		}

		values, err := internal.ParseValues(ctx, r.Client, helmChartProxy.Spec, &cluster)
		if err != nil {
			conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.ValueParsingFailedReason, clusterv1.ConditionSeverityError, err.Error())

			return errors.Wrapf(err, "failed to parse values on cluster %s", cluster.Name)
		}

		log.V(2).Info("Values for cluster", "cluster", cluster.Name, "values", values)
		if err := r.createOrUpdateHelmReleaseProxy(ctx, existingHelmReleaseProxy, helmChartProxy, &cluster, values); err != nil {
			conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxyCreationFailedReason, clusterv1.ConditionSeverityError, err.Error())

			return errors.Wrapf(err, "failed to create or update HelmReleaseProxy on cluster %s", cluster.Name)
		}
	}

	return nil
}

func getOrphanedHelmReleaseProxies(ctx context.Context, clusters []clusterv1.Cluster, helmReleaseProxies []addonsv1alpha1.HelmReleaseProxy) []addonsv1alpha1.HelmReleaseProxy {
	log := ctrl.LoggerFrom(ctx)
	log.V(2).Info("Getting HelmReleaseProxies to delete")

	selectedClusters := map[string]struct{}{}
	for _, cluster := range clusters {
		key := cluster.GetNamespace() + "/" + cluster.GetName()
		selectedClusters[key] = struct{}{}
	}
	log.V(2).Info("Selected clusters", "clusters", selectedClusters)

	releasesToDelete := []addonsv1alpha1.HelmReleaseProxy{}
	for _, helmReleaseProxy := range helmReleaseProxies {
		clusterRef := helmReleaseProxy.Spec.ClusterRef
		if clusterRef != nil {
			key := clusterRef.Namespace + "/" + clusterRef.Name
			if _, ok := selectedClusters[key]; !ok {
				releasesToDelete = append(releasesToDelete, helmReleaseProxy)
			}
		}
	}

	names := make([]string, len(releasesToDelete))
	for _, release := range releasesToDelete {
		names = append(names, release.Name)
	}
	log.V(2).Info("Releases to delete", "releases", names)

	return releasesToDelete
}

// reconcileDelete...
func (r *HelmChartProxyReconciler) reconcileDelete(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, releases []addonsv1alpha1.HelmReleaseProxy) error {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Deleting all HelmReleaseProxies as part of HelmChartProxy deletion", "helmChartProxy", helmChartProxy.Name)

	for _, release := range releases {
		log.V(2).Info("Deleting release", "releaseName", release.Name, "cluster", release.Spec.ClusterRef.Name)
		if err := r.deleteHelmReleaseProxy(ctx, &release); err != nil {
			// TODO: will this fail if clusterRef is nil
			return errors.Wrapf(err, "failed to delete release %s from cluster %s", release.Name, release.Spec.ClusterRef.Name)
		}
	}

	return nil
}

func (r *HelmChartProxyReconciler) listClustersWithLabel(ctx context.Context, label addonsv1alpha1.ClusterSelectorLabel) (*clusterv1.ClusterList, error) {
	clusterList := &clusterv1.ClusterList{}
	// TODO: validate empty key or empty value to make sure it doesn't match everything.
	labelMap := map[string]string{
		label.Key: label.Value,
	}

	if err := r.Client.List(ctx, clusterList, client.MatchingLabels(labelMap)); err != nil {
		return nil, err
	}

	return clusterList, nil
}

func (r *HelmChartProxyReconciler) listInstalledReleases(ctx context.Context, labels map[string]string) (*addonsv1alpha1.HelmReleaseProxyList, error) {
	releaseList := &addonsv1alpha1.HelmReleaseProxyList{}
	// Empty labels should match nothing, not everything
	if len(labels) == 0 {
		return nil, nil
	}

	// TODO: should we use client.MatchingLabels or try to use the labelSelector itself?
	if err := r.Client.List(ctx, releaseList, client.MatchingLabels(labels)); err != nil {
		return nil, err
	}

	return releaseList, nil
}

func (r *HelmChartProxyReconciler) aggregateHelmReleaseProxyReadyCondition(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy) error {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Aggregating HelmReleaseProxyReadyCondition")

	labels := map[string]string{
		addonsv1alpha1.HelmChartProxyLabelName: helmChartProxy.Name,
	}
	releaseList, err := r.listInstalledReleases(ctx, labels)
	if err != nil {
		// conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesReadyCondition, addonsv1alpha1.HelmReleaseProxyListFailedReason, clusterv1.ConditionSeverityError, err.Error())
		return err
	}

	getters := make([]conditions.Getter, 0, len(releaseList.Items))
	for _, r := range releaseList.Items {
		getters = append(getters, &r)
	}

	conditions.SetAggregate(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesReadyCondition, getters, conditions.AddSourceRef(), conditions.WithStepCounterIf(false))

	return nil
}

// getExistingHelmReleaseProxy...
func (r *HelmChartProxyReconciler) getExistingHelmReleaseProxy(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, cluster *clusterv1.Cluster) (*addonsv1alpha1.HelmReleaseProxy, error) {
	log := ctrl.LoggerFrom(ctx)

	helmReleaseProxyList := &addonsv1alpha1.HelmReleaseProxyList{}

	listOpts := []client.ListOption{
		client.MatchingLabels{
			clusterv1.ClusterLabelName:             cluster.Name,
			addonsv1alpha1.HelmChartProxyLabelName: helmChartProxy.Name,
		},
	}

	// TODO: Figure out if we want this search to be cross-namespaces.

	log.V(2).Info("Attempting to fetch existing HelmReleaseProxy with Cluster and HelmChartProxy labels", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
	if err := r.Client.List(context.TODO(), helmReleaseProxyList, listOpts...); err != nil {
		return nil, err
	}

	if helmReleaseProxyList.Items == nil || len(helmReleaseProxyList.Items) == 0 {
		log.V(2).Info("No HelmReleaseProxy found matching the cluster and HelmChartProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
		return nil, nil
	} else if len(helmReleaseProxyList.Items) > 1 {
		log.V(2).Info("Multiple HelmReleaseProxies found matching the cluster and HelmChartProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)
		return nil, errors.Errorf("multiple HelmReleaseProxies found matching the cluster and HelmChartProxy")
	}

	log.V(2).Info("Found existing matching HelmReleaseProxy", "cluster", cluster.Name, "helmChartProxy", helmChartProxy.Name)

	return &helmReleaseProxyList.Items[0], nil
}

// createOrUpdateHelmReleaseProxy...
func (r *HelmChartProxyReconciler) createOrUpdateHelmReleaseProxy(ctx context.Context, existing *addonsv1alpha1.HelmReleaseProxy, helmChartProxy *addonsv1alpha1.HelmChartProxy, cluster *clusterv1.Cluster, parsedValues string) error {
	log := ctrl.LoggerFrom(ctx)
	helmReleaseProxy := constructHelmReleaseProxy(existing, helmChartProxy, parsedValues, cluster)
	if helmReleaseProxy == nil {
		log.V(2).Info("HelmReleaseProxy is up to date, nothing to do", "helmReleaseProxy", existing.Name, "cluster", cluster.Name)
		return nil
	}
	if existing == nil {
		if err := r.Client.Create(ctx, helmReleaseProxy); err != nil {
			return errors.Wrapf(err, "failed to create HelmReleaseProxy '%s' for cluster: %s/%s", helmReleaseProxy.Name, cluster.Namespace, cluster.Name)
		}
	} else {
		// TODO: should this use patchHelmReleaseProxy() instead of Update() in case there's a race condition?
		if err := r.Client.Update(ctx, helmReleaseProxy); err != nil {
			return errors.Wrapf(err, "failed to update HelmReleaseProxy '%s' for cluster: %s/%s", helmReleaseProxy.Name, cluster.Namespace, cluster.Name)
		}
	}

	return nil
}

// deleteHelmReleaseProxy...
func (r *HelmChartProxyReconciler) deleteHelmReleaseProxy(ctx context.Context, helmReleaseProxy *addonsv1alpha1.HelmReleaseProxy) error {
	log := ctrl.LoggerFrom(ctx)

	if err := r.Client.Delete(ctx, helmReleaseProxy); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(2).Info("HelmReleaseProxy already deleted, nothing to do", "helmReleaseProxy", helmReleaseProxy.Name)
			return nil
		}
		return errors.Wrapf(err, "failed to delete helmReleaseProxy: %s", helmReleaseProxy.Name)
	}

	return nil
}

func constructHelmReleaseProxy(existing *addonsv1alpha1.HelmReleaseProxy, helmChartProxy *addonsv1alpha1.HelmChartProxy, parsedValues string, cluster *clusterv1.Cluster) *addonsv1alpha1.HelmReleaseProxy {
	helmReleaseProxy := &addonsv1alpha1.HelmReleaseProxy{}
	if existing == nil {
		helmReleaseProxy.GenerateName = fmt.Sprintf("%s-%s-", helmChartProxy.Spec.ChartName, cluster.Name)
		helmReleaseProxy.Namespace = helmChartProxy.Namespace
		helmReleaseProxy.OwnerReferences = util.EnsureOwnerRef(helmReleaseProxy.OwnerReferences, *metav1.NewControllerRef(helmChartProxy, helmChartProxy.GroupVersionKind()))
		// helmReleaseProxy.OwnerReferences = util.EnsureOwnerRef(helmReleaseProxy.OwnerReferences,
		// 	metav1.OwnerReference{
		// 		Kind:       cluster.Kind,
		// 		APIVersion: cluster.APIVersion,
		// 		Name:       cluster.Name,
		// 		UID:        cluster.UID,
		// 	})

		newLabels := map[string]string{}
		newLabels[clusterv1.ClusterLabelName] = cluster.Name
		newLabels[addonsv1alpha1.HelmChartProxyLabelName] = helmChartProxy.Name
		helmReleaseProxy.Labels = newLabels

		helmReleaseProxy.Spec.ClusterRef = &corev1.ObjectReference{
			Kind:       cluster.Kind,
			APIVersion: cluster.APIVersion,
			Name:       cluster.Name,
			Namespace:  cluster.Namespace,
		}

		helmReleaseProxy.Spec.ReleaseName = helmChartProxy.Spec.ReleaseName
		helmReleaseProxy.Spec.ChartName = helmChartProxy.Spec.ChartName
		helmReleaseProxy.Spec.RepoURL = helmChartProxy.Spec.RepoURL
		helmReleaseProxy.Spec.Namespace = helmChartProxy.Spec.Namespace

		// helmChartProxy.ObjectMeta.SetAnnotations(helmReleaseProxy.Annotations)
	} else {
		helmReleaseProxy = existing
		changed := false
		if existing.Spec.Version != helmChartProxy.Spec.Version {
			changed = true
		}
		if !cmp.Equal(existing.Spec.Values, parsedValues) {
			changed = true
		}

		if !changed {
			return nil
		}
	}

	helmReleaseProxy.Spec.Version = helmChartProxy.Spec.Version
	helmReleaseProxy.Spec.Values = parsedValues

	return helmReleaseProxy
}

func shouldReinstallHelmRelease(ctx context.Context, existing *addonsv1alpha1.HelmReleaseProxy, helmChartProxy *addonsv1alpha1.HelmChartProxy) bool {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Checking if HelmReleaseProxy needs to be reinstalled by by checking if immutable fields changed", "helmReleaseProxy", existing.Name)

	annotations := existing.GetAnnotations()
	result, ok := annotations[addonsv1alpha1.IsReleaseNameGeneratedAnnotation]

	// log.V(2).Info("IsReleaseNameGeneratedAnnotation", "result", result, "ok", ok)

	isReleaseNameGenerated := ok && result == "true"
	switch {
	case existing.Spec.ChartName != helmChartProxy.Spec.ChartName:
		log.V(2).Info("ChartName changed", "existing", existing.Spec.ChartName, "helmChartProxy", helmChartProxy.Spec.ChartName)
	case existing.Spec.RepoURL != helmChartProxy.Spec.RepoURL:
		log.V(2).Info("RepoURL changed", "existing", existing.Spec.RepoURL, "helmChartProxy", helmChartProxy.Spec.RepoURL)
	case isReleaseNameGenerated && helmChartProxy.Spec.ReleaseName != "":
		log.V(2).Info("Generated ReleaseName changed", "existing", existing.Spec.ReleaseName, "helmChartProxy", helmChartProxy.Spec.ReleaseName)
	case !isReleaseNameGenerated && existing.Spec.ReleaseName != helmChartProxy.Spec.ReleaseName:
		log.V(2).Info("Non-generated ReleaseName changed", "existing", existing.Spec.ReleaseName, "helmChartProxy", helmChartProxy.Spec.ReleaseName)
	case existing.Spec.Namespace != helmChartProxy.Spec.Namespace:
		log.V(2).Info("Namespace changed", "existing", existing.Spec.Namespace, "helmChartProxy", helmChartProxy.Spec.Namespace)
		return true
	}

	return false
}

func patchHelmChartProxy(ctx context.Context, patchHelper *patch.Helper, helmChartProxy *addonsv1alpha1.HelmChartProxy) error {
	// TODO: Update the readyCondition by summarizing the state of other conditions when they are implemented.
	conditions.SetSummary(helmChartProxy,
		conditions.WithConditions(
			addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition,
			addonsv1alpha1.HelmReleaseProxiesReadyCondition,
		),
	)

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return patchHelper.Patch(
		ctx,
		helmChartProxy,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
			addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition,
			addonsv1alpha1.HelmReleaseProxiesReadyCondition,
		}},
		patch.WithStatusObservedGeneration{},
	)
}

// Note: this finds every HelmReleaseProxy associated with a Cluster and returns a Request for its parent HelmChartProxy.
// This will not trigger an update if the HelmChartProxy selected a Cluster but ran into an error before creating the HelmReleaseProxy.
// Though in that case the HelmChartProxy will requeue soon anyway so it's most likely not an issue.
func (r *HelmChartProxyReconciler) ClusterToHelmChartProxiesMapper(o client.Object) []ctrl.Request {
	cluster, ok := o.(*clusterv1.Cluster)
	if !ok {
		fmt.Errorf("Expected a Cluster but got %T", o)
		return nil
	}

	helmReleaseProxies := &addonsv1alpha1.HelmReleaseProxyList{}

	listOpts := []client.ListOption{
		client.MatchingLabels{
			clusterv1.ClusterLabelName: cluster.Name,
		},
	}

	// TODO: Figure out if we want this search to be cross-namespaces.

	if err := r.Client.List(context.TODO(), helmReleaseProxies, listOpts...); err != nil {
		return nil
	}

	results := []ctrl.Request{}
	for _, helmReleaseProxy := range helmReleaseProxies.Items {
		results = append(results, ctrl.Request{
			// The HelmReleaseProxy is always in the same namespace as the HelmChartProxy.
			NamespacedName: client.ObjectKey{Namespace: helmReleaseProxy.GetNamespace(), Name: helmReleaseProxy.Labels[addonsv1alpha1.HelmChartProxyLabelName]},
		})
	}

	return results
}

func HelmReleaseProxyToHelmChartProxyMapper(o client.Object) []ctrl.Request {
	helmReleaseProxy, ok := o.(*addonsv1alpha1.HelmReleaseProxy)
	if !ok {
		fmt.Errorf("Expected a HelmReleaseProxy but got %T", o)
		return nil
	}

	// Check if the controller reference is already set and
	// return an empty result when one is found.
	for _, ref := range helmReleaseProxy.ObjectMeta.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			name := client.ObjectKey{
				Namespace: helmReleaseProxy.GetNamespace(),
				Name:      ref.Name,
			}
			return []ctrl.Request{
				{
					NamespacedName: name,
				},
			}
		}
	}

	return nil
}
