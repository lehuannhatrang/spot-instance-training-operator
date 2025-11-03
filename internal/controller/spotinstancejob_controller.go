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

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

const (
	spotInstanceJobFinalizer = "training.dcnlab.com/finalizer"
	jobReplicaLabel          = "training.dcnlab.com/job-replica"
	spotInstanceJobLabel     = "training.dcnlab.com/spot-instance-job"
)

// SpotInstanceJobReconciler reconciles a SpotInstanceJob object
type SpotInstanceJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancejobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancejobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancejobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=migration.dcnlab.com,resources=checkpointbackups,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SpotInstanceJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling SpotInstanceJob", "name", req.Name, "namespace", req.Namespace)

	// Fetch the SpotInstanceJob instance
	spotInstanceJob := &trainingv1alpha1.SpotInstanceJob{}
	if err := r.Get(ctx, req.NamespacedName, spotInstanceJob); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("SpotInstanceJob resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get SpotInstanceJob")
		return ctrl.Result{}, err
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(spotInstanceJob, spotInstanceJobFinalizer) {
		controllerutil.AddFinalizer(spotInstanceJob, spotInstanceJobFinalizer)
		if err := r.Update(ctx, spotInstanceJob); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle deletion
	if !spotInstanceJob.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, spotInstanceJob)
	}

	// Get the SpotInstanceVM reference
	spotInstanceVM := &trainingv1alpha1.SpotInstanceVM{}
	vmRef := types.NamespacedName{
		Name:      spotInstanceJob.Spec.SpotInstanceVMRef.Name,
		Namespace: spotInstanceJob.Namespace,
	}
	if err := r.Get(ctx, vmRef, spotInstanceVM); err != nil {
		logger.Error(err, "Failed to get SpotInstanceVM", "vmRef", vmRef)
		return ctrl.Result{}, err
	}

	// Initialize status if needed
	if spotInstanceJob.Status.Conditions == nil {
		spotInstanceJob.Status.Conditions = []metav1.Condition{}
	}
	if spotInstanceJob.Status.JobStatuses == nil {
		spotInstanceJob.Status.JobStatuses = []trainingv1alpha1.JobReplicaStatus{}
	}

	// Determine the number of replicas
	replicas := int32(2) // Default
	if spotInstanceJob.Spec.Replicas != nil {
		replicas = *spotInstanceJob.Spec.Replicas
	}

	// List existing jobs
	jobList := &batchv1.JobList{}
	labelSelector := client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
	}
	if err := r.List(ctx, jobList, client.InNamespace(spotInstanceJob.Namespace), labelSelector); err != nil {
		logger.Error(err, "Failed to list jobs")
		return ctrl.Result{}, err
	}

	// Reconcile jobs to match desired replicas
	activeJobs := int32(0)
	for _, job := range jobList.Items {
		if job.Status.Active > 0 {
			activeJobs++
		}
	}

	// Create missing jobs
	if int32(len(jobList.Items)) < replicas {
		needed := replicas - int32(len(jobList.Items))
		for i := int32(0); i < needed; i++ {
			jobName := fmt.Sprintf("%s-replica-%d-%d", spotInstanceJob.Name, len(jobList.Items)+int(i), time.Now().Unix())
			if err := r.createJobReplica(ctx, spotInstanceJob, jobName, int32(len(jobList.Items)+int(i))); err != nil {
				logger.Error(err, "Failed to create job replica", "jobName", jobName)
				return ctrl.Result{}, err
			}
			logger.Info("Created job replica", "jobName", jobName)
		}
	}

	// Update status
	spotInstanceJob.Status.ActiveReplicas = activeJobs
	if err := r.Status().Update(ctx, spotInstanceJob); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	// Schedule checkpoint based on interval
	checkpointInterval := spotInstanceJob.Spec.CheckpointConfig.CheckpointInterval.Duration
	if checkpointInterval > 0 {
		return ctrl.Result{RequeueAfter: checkpointInterval}, nil
	}

	return ctrl.Result{}, nil
}

// createJobReplica creates a new Job replica from the template
func (r *SpotInstanceJobReconciler) createJobReplica(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, jobName string, replicaIndex int32) error {
	logger := log.FromContext(ctx)

	// Create a Job from the template
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: spotInstanceJob.Namespace,
			Labels: map[string]string{
				spotInstanceJobLabel: spotInstanceJob.Name,
				jobReplicaLabel:      fmt.Sprintf("%d", replicaIndex),
			},
		},
		Spec: spotInstanceJob.Spec.JobTemplate.Spec,
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(spotInstanceJob, job, r.Scheme); err != nil {
		return err
	}

	// Create the Job
	if err := r.Create(ctx, job); err != nil {
		logger.Error(err, "Failed to create Job", "jobName", jobName)
		return err
	}

	return nil
}

// handleDeletion handles the deletion of SpotInstanceJob
func (r *SpotInstanceJobReconciler) handleDeletion(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling deletion of SpotInstanceJob")

	// Perform cleanup operations here
	// For example, delete associated jobs, clean up GCP resources, etc.

	// Remove finalizer
	controllerutil.RemoveFinalizer(spotInstanceJob, spotInstanceJobFinalizer)
	if err := r.Update(ctx, spotInstanceJob); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// checkResourceAvailability checks if there are enough resources to schedule jobs
func (r *SpotInstanceJobReconciler) checkResourceAvailability(ctx context.Context, spotInstanceVM *trainingv1alpha1.SpotInstanceVM) (bool, error) {
	logger := log.FromContext(ctx)

	// List all nodes
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		logger.Error(err, "Failed to list nodes")
		return false, err
	}

	// Check for GPU availability
	if spotInstanceVM.Spec.GPU != nil {
		for _, node := range nodeList.Items {
			// Check if node has the required GPU
			if gpuCapacity, ok := node.Status.Capacity["nvidia.com/gpu"]; ok {
				if gpuCapacity.Value() >= int64(spotInstanceVM.Spec.GPU.Count) {
					// Check if GPU is available (not fully allocated)
					if gpuAllocatable, ok := node.Status.Allocatable["nvidia.com/gpu"]; ok {
						if gpuAllocatable.Value() >= int64(spotInstanceVM.Spec.GPU.Count) {
							return true, nil
						}
					}
				}
			}
		}
		return false, nil
	}

	return true, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SpotInstanceJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&trainingv1alpha1.SpotInstanceJob{}).
		Owns(&batchv1.Job{}).
		Named("spotinstancejob").
		Complete(r)
}
