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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CheckpointConfig defines checkpoint configuration
type CheckpointConfig struct {
	// CheckpointImageRepo is the container registry repository for checkpoint images
	// +required
	CheckpointImageRepo string `json:"checkpointImageRepo"`

	// CheckpointInterval is the interval between scheduled checkpoints
	// +required
	CheckpointInterval metav1.Duration `json:"checkpointInterval"`

	// CheckpointRepoCredentialRef is a reference to the secret containing credentials for the checkpoint image repo
	// +optional
	CheckpointRepoCredentialRef *corev1.SecretReference `json:"checkpointRepoCredentialRef,omitempty"`

	// CheckpointStorage defines the storage configuration for checkpoints
	// +optional
	CheckpointStorage *CheckpointStorageConfig `json:"checkpointStorage,omitempty"`
}

// CheckpointStorageConfig defines storage configuration for checkpoints
type CheckpointStorageConfig struct {
	// Type of storage (e.g., "gcs", "s3")
	// +optional
	Type string `json:"type,omitempty"`

	// Bucket name for checkpoint storage
	// +optional
	Bucket string `json:"bucket,omitempty"`

	// Path prefix for checkpoint storage
	// +optional
	PathPrefix string `json:"pathPrefix,omitempty"`
}

// SpotInstanceJobSpec defines the desired state of SpotInstanceJob
type SpotInstanceJobSpec struct {
	// Replicas is the number of job replicas to maintain (default: 2)
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// JobTemplate is the template for the Job to be created
	// +required
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	JobTemplate batchv1.JobTemplateSpec `json:"jobTemplate"`

	// CheckpointConfig defines checkpoint configuration
	// +required
	CheckpointConfig CheckpointConfig `json:"checkpointConfig"`

	// SpotInstanceVMRef is a reference to the SpotInstanceVM resource
	// +required
	SpotInstanceVMRef corev1.LocalObjectReference `json:"spotInstanceVMRef"`

	// GCPCredentialsSecretRef is a reference to the secret containing GCP credentials
	// +required
	GCPCredentialsSecretRef corev1.SecretReference `json:"gcpCredentialsSecretRef"`
}

// SpotInstanceJobStatus defines the observed state of SpotInstanceJob.
type SpotInstanceJobStatus struct {
	// Conditions represent the latest available observations of an object's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ActiveReplicas is the number of actively running job replicas
	// +optional
	ActiveReplicas int32 `json:"activeReplicas,omitempty"`

	// LastCheckpointTime is the last time a checkpoint was created
	// +optional
	LastCheckpointTime *metav1.Time `json:"lastCheckpointTime,omitempty"`

	// LastCheckpointImage is the image name of the last checkpoint
	// +optional
	LastCheckpointImage string `json:"lastCheckpointImage,omitempty"`

	// JobStatuses tracks the status of each job replica
	// +optional
	JobStatuses []JobReplicaStatus `json:"jobStatuses,omitempty"`
}

// JobReplicaStatus tracks the status of a single job replica
type JobReplicaStatus struct {
	// Name is the name of the job
	Name string `json:"name"`

	// NodeName is the node where the job is running
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// Phase is the current phase of the job replica
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastCheckpointImage is the checkpoint image used by this replica
	// +optional
	LastCheckpointImage string `json:"lastCheckpointImage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// SpotInstanceJob is the Schema for the spotinstancejobs API
type SpotInstanceJob struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of SpotInstanceJob
	// +required
	Spec SpotInstanceJobSpec `json:"spec"`

	// status defines the observed state of SpotInstanceJob
	// +optional
	Status SpotInstanceJobStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// SpotInstanceJobList contains a list of SpotInstanceJob
type SpotInstanceJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpotInstanceJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SpotInstanceJob{}, &SpotInstanceJobList{})
}
