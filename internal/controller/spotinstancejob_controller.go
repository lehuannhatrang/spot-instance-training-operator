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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
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
	provisionedInstanceLabel = "training.dcnlab.com/provisioned-instance"
)

// SpotInstanceJobReconciler reconciles a SpotInstanceJob object
type SpotInstanceJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancejobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancejobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancejobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=instancetemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=provisionedinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=provisionedinstances/status,verbs=get
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=migration.dcnlab.com,resources=checkpointbackups,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop
func (r *SpotInstanceJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the SpotInstanceJob
	spotInstanceJob := &trainingv1alpha1.SpotInstanceJob{}
	if err := r.Get(ctx, req.NamespacedName, spotInstanceJob); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !spotInstanceJob.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, spotInstanceJob)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(spotInstanceJob, spotInstanceJobFinalizer) {
		controllerutil.AddFinalizer(spotInstanceJob, spotInstanceJobFinalizer)
		if err := r.Update(ctx, spotInstanceJob); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize status
	if spotInstanceJob.Status.ActiveReplicas == 0 && len(spotInstanceJob.Status.JobStatuses) == 0 {
		spotInstanceJob.Status.ActiveReplicas = 0
		if err := r.Status().Update(ctx, spotInstanceJob); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Fetch InstanceTemplate
	instanceTemplate, err := r.fetchInstanceTemplate(ctx, spotInstanceJob)
	if err != nil {
		logger.Error(err, "Failed to fetch InstanceTemplate")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Handle preemption FIRST (this will create checkpoints if preemption is detected)
	pendingCheckpoints, err := r.hasPendingCheckpoints(ctx, spotInstanceJob)
	if err != nil {
		logger.Error(err, "Failed to check pending checkpoints")
		// Continue with reconciliation even if check failed
	} else if !pendingCheckpoints {
		// No pending checkpoints, safe to handle preemption
		if err := r.handlePreemption(ctx, spotInstanceJob); err != nil {
			logger.Error(err, "Failed to handle preemption")
			return ctrl.Result{}, err
		}
	}

	// Check if we're waiting for checkpoint completion AFTER handling preemption
	// (preemption handling may have just created a checkpoint)
	pendingCheckpoints, err = r.hasPendingCheckpoints(ctx, spotInstanceJob)
	if err != nil {
		logger.Error(err, "Failed to check pending checkpoints")
		// Continue with reconciliation even if check failed
	} else if pendingCheckpoints {
		logger.Info("Waiting for checkpoint completion before creating new ProvisionedInstances (checkpoint typically takes 3-5 minutes)")
		// Skip ProvisionedInstance creation while checkpoint is in progress
	} else {
		// No pending checkpoints, safe to reconcile ProvisionedInstances
		reconcileErr := r.reconcileProvisionedInstances(ctx, spotInstanceJob, instanceTemplate)
		if reconcileErr != nil {
			logger.Error(reconcileErr, "Failed to reconcile provisioned instances")
			return ctrl.Result{}, reconcileErr
		}
	}

	// Reconcile Job replicas AFTER ProvisionedInstances are created
	// This allows jobs to be assigned to specific nodes
	if err := r.reconcileJobReplicas(ctx, spotInstanceJob); err != nil {
		logger.Error(err, "Failed to reconcile job replicas")
		return ctrl.Result{}, err
	}

	// Update status
	if err := r.updateStatus(ctx, spotInstanceJob); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	// Requeue more frequently if waiting for checkpoint completion
	if pendingCheckpoints {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SpotInstanceJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&trainingv1alpha1.SpotInstanceJob{}).
		Owns(&batchv1.Job{}).
		Owns(&trainingv1alpha1.ProvisionedInstance{}).
		Named("spotinstancejob").
		Complete(r)
}
