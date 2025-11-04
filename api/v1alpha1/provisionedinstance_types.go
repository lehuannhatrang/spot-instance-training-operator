/*
Copyright 2025 DCN Lab.

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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ProvisionedInstanceSpec defines the desired state of ProvisionedInstance
type ProvisionedInstanceSpec struct {
	// InstanceTemplateName is the name of the InstanceTemplate to use for provisioning
	// +required
	InstanceTemplateName string `json:"instanceTemplateName"`

	// InstanceTemplateNamespace is the namespace of the InstanceTemplate
	// +optional
	InstanceTemplateNamespace string `json:"instanceTemplateNamespace,omitempty"`

	// SpotInstanceJobRef is a reference to the SpotInstanceJob that owns this instance
	// +optional
	SpotInstanceJobRef *metav1.OwnerReference `json:"spotInstanceJobRef,omitempty"`

	// GCPCredentialsSecretRef is a reference to the secret containing GCP credentials
	// +required
	GCPCredentialsSecretRef corev1.SecretReference `json:"gcpCredentialsSecretRef"`
}

// ProvisionedInstanceStatus defines the observed state of ProvisionedInstance.
type ProvisionedInstanceStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// InstanceID is the GCP instance ID
	// +optional
	InstanceID string `json:"instanceID,omitempty"`

	// InstanceName is the GCP instance name
	// +optional
	InstanceName string `json:"instanceName,omitempty"`

	// InternalIP is the internal IP address of the instance
	// +optional
	InternalIP string `json:"internalIP,omitempty"`

	// ExternalIP is the external IP address of the instance
	// +optional
	ExternalIP string `json:"externalIP,omitempty"`

	// State is the current state of the instance (PROVISIONING, RUNNING, STOPPING, TERMINATED)
	// +optional
	State string `json:"state,omitempty"`

	// NodeName is the Kubernetes node name after joining the cluster
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// JoinedCluster indicates whether the instance has joined the Kubernetes cluster
	// +optional
	JoinedCluster bool `json:"joinedCluster,omitempty"`

	// CreationTime is when the instance was created
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty"`

	// PreemptionNotice indicates if the instance has received a preemption notice
	// +optional
	PreemptionNotice bool `json:"preemptionNotice,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ProvisionedInstance is the Schema for the provisionedinstances API
type ProvisionedInstance struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of ProvisionedInstance
	// +required
	Spec ProvisionedInstanceSpec `json:"spec"`

	// status defines the observed state of ProvisionedInstance
	// +optional
	Status ProvisionedInstanceStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ProvisionedInstanceList contains a list of ProvisionedInstance
type ProvisionedInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisionedInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProvisionedInstance{}, &ProvisionedInstanceList{})
}
