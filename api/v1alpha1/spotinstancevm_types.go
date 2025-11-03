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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// GCPConfig defines GCP-specific configuration
type GCPConfig struct {
	// Project is the GCP project ID
	// +required
	Project string `json:"project"`

	// Zone is the GCP zone for the instance
	// +required
	Zone string `json:"zone"`

	// Region is the GCP region (optional, derived from zone if not provided)
	// +optional
	Region string `json:"region,omitempty"`

	// MachineType is the GCP machine type (e.g., "n1-standard-4")
	// +required
	MachineType string `json:"machineType"`

	// DiskSizeGB is the boot disk size in GB
	// +kubebuilder:default=100
	// +optional
	DiskSizeGB int32 `json:"diskSizeGB,omitempty"`

	// DiskType is the disk type (e.g., "pd-standard", "pd-ssd")
	// +kubebuilder:default="pd-standard"
	// +optional
	DiskType string `json:"diskType,omitempty"`

	// NetworkTags are network tags to apply to the instance
	// +optional
	NetworkTags []string `json:"networkTags,omitempty"`

	// ServiceAccount is the service account email for the instance
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`
}

// GPUConfig defines GPU configuration for the VM
type GPUConfig struct {
	// Type is the GPU type (e.g., "nvidia-tesla-t4", "nvidia-tesla-v100")
	// +required
	Type string `json:"type"`

	// Count is the number of GPUs to attach
	// +kubebuilder:validation:Minimum=1
	// +required
	Count int32 `json:"count"`
}

// SpotInstanceVMSpec defines the desired state of SpotInstanceVM
type SpotInstanceVMSpec struct {
	// VMImage is the source image for the VM (e.g., "projects/debian-cloud/global/images/debian-11-bullseye-v20230629")
	// +required
	VMImage string `json:"vmImage"`

	// GPU defines GPU configuration for the VM
	// +optional
	GPU *GPUConfig `json:"gpu,omitempty"`

	// GCP defines GCP-specific configuration
	// +required
	GCP GCPConfig `json:"gcp"`

	// StartupScript is the startup script to run when the VM starts
	// +optional
	StartupScript string `json:"startupScript,omitempty"`

	// KubeadmJoinConfig defines configuration for joining the Kubernetes cluster
	// +optional
	KubeadmJoinConfig *KubeadmJoinConfig `json:"kubeadmJoinConfig,omitempty"`
}

// KubeadmJoinConfig defines configuration for joining the Kubernetes cluster
type KubeadmJoinConfig struct {
	// TokenSecretRef is a reference to the secret containing the kubeadm token
	// +optional
	TokenSecretRef string `json:"tokenSecretRef,omitempty"`

	// CACertHash is the CA certificate hash for kubeadm join
	// +optional
	CACertHash string `json:"caCertHash,omitempty"`

	// ControlPlaneEndpoint is the control plane endpoint
	// +optional
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
}

// SpotInstanceVMStatus defines the observed state of SpotInstanceVM.
type SpotInstanceVMStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ProvisionedInstances tracks the provisioned VM instances
	// +optional
	ProvisionedInstances []VMInstanceStatus `json:"provisionedInstances,omitempty"`
}

// VMInstanceStatus tracks the status of a provisioned VM instance
type VMInstanceStatus struct {
	// Name is the name of the VM instance
	Name string `json:"name"`

	// InstanceID is the GCP instance ID
	// +optional
	InstanceID string `json:"instanceID,omitempty"`

	// InternalIP is the internal IP address of the instance
	// +optional
	InternalIP string `json:"internalIP,omitempty"`

	// ExternalIP is the external IP address of the instance
	// +optional
	ExternalIP string `json:"externalIP,omitempty"`

	// State is the current state of the instance
	// +optional
	State string `json:"state,omitempty"`

	// JoinedCluster indicates whether the instance has joined the Kubernetes cluster
	// +optional
	JoinedCluster bool `json:"joinedCluster,omitempty"`

	// NodeName is the Kubernetes node name after joining the cluster
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// CreationTime is when the instance was created
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SpotInstanceVM is the Schema for the spotinstancevms API
type SpotInstanceVM struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of SpotInstanceVM
	// +required
	Spec SpotInstanceVMSpec `json:"spec"`

	// status defines the observed state of SpotInstanceVM
	// +optional
	Status SpotInstanceVMStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SpotInstanceVMList contains a list of SpotInstanceVM
type SpotInstanceVMList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpotInstanceVM `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SpotInstanceVM{}, &SpotInstanceVMList{})
}
