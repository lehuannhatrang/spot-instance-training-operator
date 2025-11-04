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

// InstanceTemplateSpec defines the desired state of InstanceTemplate
type InstanceTemplateSpec struct {
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

	// Preemptible determines whether to create spot instances (true) or on-demand instances (false)
	// +kubebuilder:default=true
	// +optional
	Preemptible *bool `json:"preemptible,omitempty"`
}

// InstanceTemplateStatus defines the observed state of InstanceTemplate.
type InstanceTemplateStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// InstanceTemplate is the Schema for the instancetemplates API
type InstanceTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of InstanceTemplate
	// +required
	Spec InstanceTemplateSpec `json:"spec"`

	// status defines the observed state of InstanceTemplate
	// +optional
	Status InstanceTemplateStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// InstanceTemplateList contains a list of InstanceTemplate
type InstanceTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InstanceTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InstanceTemplate{}, &InstanceTemplateList{})
}
