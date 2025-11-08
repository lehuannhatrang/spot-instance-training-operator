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

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

func (r *SpotInstanceJobReconciler) updateStatus(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) error {
	// Count active jobs
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
	}); err != nil {
		return err
	}

	activeReplicas := int32(0)
	for _, job := range jobList.Items {
		if job.Status.Active > 0 {
			activeReplicas++
		}
	}

	spotInstanceJob.Status.ActiveReplicas = activeReplicas
	return r.Status().Update(ctx, spotInstanceJob)
}

// handleDeletion handles the deletion of a SpotInstanceJob
func (r *SpotInstanceJobReconciler) handleDeletion(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling SpotInstanceJob deletion", "name", spotInstanceJob.Name)

	if controllerutil.ContainsFinalizer(spotInstanceJob, spotInstanceJobFinalizer) {
		// Delete all CheckpointBackup CRs
		checkpoints := &unstructured.UnstructuredList{}
		checkpoints.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "migration.dcnlab.com",
			Version: "v1",
			Kind:    "CheckpointBackupList",
		})

		labelSelector := client.MatchingLabels{
			spotInstanceJobLabel: spotInstanceJob.Name,
		}
		if err := r.List(ctx, checkpoints, client.InNamespace(spotInstanceJob.Namespace), labelSelector); err != nil {
			logger.Error(err, "Failed to list CheckpointBackup CRs")
			// Continue with other cleanup even if listing checkpoints fails
		} else {
			for _, cp := range checkpoints.Items {
				if err := r.Delete(ctx, &cp); err != nil {
					if errors.IsNotFound(err) {
						logger.Info("CheckpointBackup already deleted", "name", cp.GetName())
					} else {
						logger.Error(err, "Failed to delete CheckpointBackup", "name", cp.GetName())
					}
				} else {
					logger.Info("Deleted CheckpointBackup", "name", cp.GetName())
				}
			}
		}

		// Delete all ProvisionedInstances
		provisionedInstanceList := &trainingv1alpha1.ProvisionedInstanceList{}
		if err := r.List(ctx, provisionedInstanceList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
			spotInstanceJobLabel: spotInstanceJob.Name,
		}); err != nil {
			return ctrl.Result{}, err
		}

		for _, pi := range provisionedInstanceList.Items {
			if err := r.Delete(ctx, &pi); err != nil {
				logger.Error(err, "Failed to delete ProvisionedInstance", "name", pi.Name)
			}
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(spotInstanceJob, spotInstanceJobFinalizer)
		if err := r.Update(ctx, spotInstanceJob); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
