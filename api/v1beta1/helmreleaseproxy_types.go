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

package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// HelmReleaseProxySpec defines the desired state of HelmReleaseProxy.
type HelmReleaseProxySpec struct {
	// Important: Run "make" to regenerate code after modifying this file

	// ClusterRef is a reference to the Cluster to install the Helm release on.
	ClusterRef *corev1.ObjectReference `json:"clusterRef,omitempty"`

	// ChartName is the name of the Helm chart in the repository.
	ChartName string `json:"chartName,omitempty"`

	// RepoURL is the URL of the Helm chart repository.
	RepoURL string `json:"repoURL,omitempty"`

	// ReleaseName is the release name of the installed Helm chart. If it is not specified, a name will be generated.
	// +optional
	ReleaseName string `json:"releaseName,omitempty"`

	// Version is the version of the Helm chart. To be replaced with a compatibility matrix.
	// +optional
	Version string `json:"version,omitempty"`

	// Namespace is the namespace the Helm release will be installed on the referenced Cluster. If it is not specified, the default namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Values is a map of key/value pairs specifying values to be passed to the Helm chart. The map key is the full path
	// to the field, and the map value is the value to set. The map value does not contain Go templates as it has already
	// been resolved to the value of the field in the referenced workload Cluster.
	// +optional
	Values map[string]string `json:"values,omitempty"`
}

// HelmReleaseProxyStatus defines the observed state of HelmReleaseProxy.
type HelmReleaseProxyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Ready is true when the Helm release on the referenced Cluster is up to date with the HelmReleaseProxySpec.
	// +optional
	Ready bool `json:"ready"`

	// FailureReason will be set in the event that there is a an error reconciling the HelmReleaseProxy.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// Status is the current status of the Helm release.
	// +optional
	Status string `json:"status,omitempty"`

	// Revision is the current revision of the Helm release.
	// +optional
	Revision int `json:"revision,omitempty"`

	// Namespace is the namespace of the Helm release on the workload cluster.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef.name",description="Cluster to which this HelmReleaseProxy belongs"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.status"
// +kubebuilder:printcolumn:name="Revision",type="string",JSONPath=".status.revision"
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".status.namespace"
// +kubebuilder:subresource:status

// HelmReleaseProxy is the Schema for the helmreleaseproxies API
type HelmReleaseProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HelmReleaseProxySpec   `json:"spec,omitempty"`
	Status HelmReleaseProxyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// HelmReleaseProxyList contains a list of HelmReleaseProxy
type HelmReleaseProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HelmReleaseProxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HelmReleaseProxy{}, &HelmReleaseProxyList{})
}
