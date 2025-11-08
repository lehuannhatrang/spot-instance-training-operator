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
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

func (r *SpotInstanceJobReconciler) reconcileJobReplicas(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) error {
	logger := log.FromContext(ctx)

	desiredReplicas := int32(2)
	if spotInstanceJob.Spec.Replicas != nil {
		desiredReplicas = *spotInstanceJob.Spec.Replicas
	}

	// List existing jobs
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
	}); err != nil {
		return err
	}

	// Check if there are pending checkpoints - if so, don't create new jobs yet
	hasPendingCheckpoints, _ := r.hasPendingCheckpoints(ctx, spotInstanceJob)
	if hasPendingCheckpoints {
		logger.Info("Pending checkpoints detected, skipping job replica creation")
		return nil
	}

	// Track which replica indices already have jobs
	replicaIndicesWithJobs := make(map[int32]bool)
	jobsBeingUpdated := make(map[string]bool)

	for _, job := range jobList.Items {
		// Skip jobs that are being deleted (unless very recently deleted)
		if !job.DeletionTimestamp.IsZero() {
			deletionAge := time.Since(job.DeletionTimestamp.Time)
			// If job was deleted very recently (within 10 seconds), it might be being recreated
			if deletionAge < 10*time.Second {
				// Extract replica index from job labels
				if replicaIndexStr, ok := job.Labels[jobReplicaLabel]; ok {
					if idx, err := strconv.ParseInt(replicaIndexStr, 10, 32); err == nil {
						replicaIndicesWithJobs[int32(idx)] = true
						logger.Info("Job recently deleted, likely being updated", "job", job.Name, "replicaIndex", idx, "age", deletionAge)
					}
				}
				jobsBeingUpdated[job.Name] = true
			}
			continue
		}

		// Extract replica index from job labels
		replicaIndex := int32(-1)
		if replicaIndexStr, ok := job.Labels[jobReplicaLabel]; ok {
			if idx, err := strconv.ParseInt(replicaIndexStr, 10, 32); err == nil {
				replicaIndex = int32(idx)
			}
		}

		// If job is active, succeeded, or has checkpoint-updated annotation, mark its replica index as taken
		hasCheckpointUpdated := false
		if job.Annotations != nil {
			if _, ok := job.Annotations["training.dcnlab.com/checkpoint-updated"]; ok {
				hasCheckpointUpdated = true
				jobsBeingUpdated[job.Name] = true
			}
		}

		if job.Status.Active > 0 || job.Status.Succeeded > 0 || hasCheckpointUpdated {
			if replicaIndex >= 0 {
				replicaIndicesWithJobs[replicaIndex] = true
				if hasCheckpointUpdated && job.Status.Active == 0 {
					logger.Info("Job with checkpoint-updated annotation (pods starting)", "job", job.Name, "replicaIndex", replicaIndex)
				}
			}
		}
	}

	// Count how many replica indices have jobs
	activeReplicaCount := int32(len(replicaIndicesWithJobs))
	logger.Info("Job replica reconciliation", "desiredReplicas", desiredReplicas, "activeReplicaCount", activeReplicaCount, "replicaIndices", replicaIndicesWithJobs)

	// Create missing job replicas for replica indices that don't have jobs
	if activeReplicaCount < desiredReplicas {
		needed := desiredReplicas - activeReplicaCount

		replicaIndex := int32(0)
		created := int32(0)
		for created < needed {
			// Find next replica index without a job
			for replicaIndicesWithJobs[replicaIndex] {
				replicaIndex++
				if replicaIndex >= desiredReplicas*2 {
					logger.Info("Too many replica indices checked, stopping", "replicaIndex", replicaIndex)
					break
				}
			}

			// Double-check that no job with this replica index exists
			existingJobsForIndex := &batchv1.JobList{}
			if err := r.List(ctx, existingJobsForIndex, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
				spotInstanceJobLabel: spotInstanceJob.Name,
				jobReplicaLabel:      fmt.Sprintf("%d", replicaIndex),
			}); err == nil && len(existingJobsForIndex.Items) > 0 {
				// Check if any of these jobs are valid
				hasValidJob := false
				for _, existingJob := range existingJobsForIndex.Items {
					if !existingJob.DeletionTimestamp.IsZero() {
						deletionAge := time.Since(existingJob.DeletionTimestamp.Time)
						if deletionAge < 10*time.Second {
							hasValidJob = true
							logger.Info("Job for replica index recently deleted, skipping creation", "replicaIndex", replicaIndex, "job", existingJob.Name)
							break
						}
						continue
					}
					if existingJob.Status.Active > 0 || existingJob.Status.Succeeded > 0 {
						hasValidJob = true
						logger.Info("Job for replica index already exists", "replicaIndex", replicaIndex, "job", existingJob.Name)
						break
					}
					if existingJob.Annotations != nil {
						if _, ok := existingJob.Annotations["training.dcnlab.com/checkpoint-updated"]; ok {
							hasValidJob = true
							logger.Info("Job for replica index has checkpoint-updated", "replicaIndex", replicaIndex, "job", existingJob.Name)
							break
						}
					}
				}
				if hasValidJob {
					replicaIndicesWithJobs[replicaIndex] = true
					replicaIndex++
					continue
				}
			}

			jobName := fmt.Sprintf("%s-replica-%d-%d", spotInstanceJob.Name, replicaIndex, time.Now().Unix())
			if err := r.createJobReplica(ctx, spotInstanceJob, jobName, replicaIndex); err != nil {
				logger.Error(err, "Failed to create job replica", "jobName", jobName, "replicaIndex", replicaIndex)
				replicaIndex++
				continue
			}
			logger.Info("Created job replica", "jobName", jobName, "replicaIndex", replicaIndex)
			replicaIndicesWithJobs[replicaIndex] = true
			replicaIndex++
			created++
		}
	} else if len(jobsBeingUpdated) > 0 {
		logger.Info("All replica indices have jobs, some are being updated", "activeReplicaCount", activeReplicaCount, "jobsBeingUpdated", len(jobsBeingUpdated))
	}

	return nil
}

// createJobReplica creates a new job replica

func (r *SpotInstanceJobReconciler) createJobReplica(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, jobName string, replicaIndex int32) error {
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

	// Ensure pod template has labels
	if job.Spec.Template.Labels == nil {
		job.Spec.Template.Labels = make(map[string]string)
	}
	job.Spec.Template.Labels[spotInstanceJobLabel] = spotInstanceJob.Name
	job.Spec.Template.Labels[jobReplicaLabel] = fmt.Sprintf("%d", replicaIndex)

	// Set owner reference
	if err := controllerutil.SetControllerReference(spotInstanceJob, job, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, job)
}

// reconcileProvisionedInstances ensures ProvisionedInstance CRs exist for each job replica

func (r *SpotInstanceJobReconciler) deleteJob(ctx context.Context, namespace, jobName string) error {
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, job); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	return r.Delete(ctx, job)
}

// updateJobWithCheckpoint updates an existing job's container images with checkpoint images
// This is done by deleting all pods, then deleting and immediately recreating the job with updated images

func (r *SpotInstanceJobReconciler) updateJobWithCheckpoint(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, jobName string, checkpointContainers []map[string]interface{}) error {
	logger := log.FromContext(ctx)

	// Get the existing job
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: spotInstanceJob.Namespace}, job); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Job not found, cannot update", "job", jobName)
			return nil
		}
		return err
	}

	// Check if job has already been updated with checkpoint (has annotation)
	if job.Annotations != nil {
		if _, ok := job.Annotations["training.dcnlab.com/checkpoint-updated"]; ok {
			logger.Info("Job already updated with checkpoint, skipping", "job", jobName)
			return nil
		}
	}

	// Create a map of container name -> checkpoint image
	checkpointImageMap := make(map[string]string)
	if len(checkpointContainers) > 0 {
		for _, c := range checkpointContainers {
			if name, ok := c["name"].(string); ok {
				if image, ok := c["image"].(string); ok {
					checkpointImageMap[name] = image
				}
			}
		}
	}

	// Save the job spec and update container images
	updatedJobSpec := job.Spec.DeepCopy()

	// Update container images
	if len(checkpointImageMap) > 0 {
		for i := range updatedJobSpec.Template.Spec.Containers {
			containerName := updatedJobSpec.Template.Spec.Containers[i].Name
			if checkpointImage, found := checkpointImageMap[containerName]; found {
				updatedJobSpec.Template.Spec.Containers[i].Image = checkpointImage
				logger.Info("Updating container with checkpoint image", "container", containerName, "image", checkpointImage)
			}
		}
	}

	// Extract replica index from job labels if available
	replicaIndex := int32(0)
	if replicaIndexStr, ok := job.Labels[jobReplicaLabel]; ok {
		if idx, err := strconv.ParseInt(replicaIndexStr, 10, 32); err == nil {
			replicaIndex = int32(idx)
		}
	}

	// Delete the job with background propagation to also delete pods
	// Use DeleteOptions to set propagation policy
	propagationPolicy := metav1.DeletePropagationBackground
	deleteOptions := client.DeleteOptions{
		PropagationPolicy: &propagationPolicy,
	}
	if err := r.Delete(ctx, job, &deleteOptions); err != nil {
		return fmt.Errorf("failed to delete job: %w", err)
	}

	// Wait for the deletion to be processed (Kubernetes needs time to delete the job and its pods)
	time.Sleep(2 * time.Second)

	// Create the job again with updated spec
	// Clear immutable fields: spec.selector and auto-generated labels
	newJobName := jobName // Try to use the same name
	newJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newJobName,
			Namespace: spotInstanceJob.Namespace,
			Labels: map[string]string{
				spotInstanceJobLabel: spotInstanceJob.Name,
				jobReplicaLabel:      fmt.Sprintf("%d", replicaIndex),
			},
			Annotations: map[string]string{
				"training.dcnlab.com/checkpoint-updated": "true",
			},
		},
		Spec: batchv1.JobSpec{
			// Copy all fields from updatedJobSpec except selector
			Parallelism:             updatedJobSpec.Parallelism,
			Completions:             updatedJobSpec.Completions,
			ActiveDeadlineSeconds:   updatedJobSpec.ActiveDeadlineSeconds,
			BackoffLimit:            updatedJobSpec.BackoffLimit,
			TTLSecondsAfterFinished: updatedJobSpec.TTLSecondsAfterFinished,
			CompletionMode:          updatedJobSpec.CompletionMode,
			Suspend:                 updatedJobSpec.Suspend,
			PodFailurePolicy:        updatedJobSpec.PodFailurePolicy,
			// Don't copy selector - Kubernetes will auto-generate it
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: make(map[string]string),
				},
				Spec: updatedJobSpec.Template.Spec,
			},
		},
	}

	// Set only our custom labels (clear auto-generated labels)
	newJob.Spec.Template.Labels[spotInstanceJobLabel] = spotInstanceJob.Name
	newJob.Spec.Template.Labels[jobReplicaLabel] = fmt.Sprintf("%d", replicaIndex)

	// Set owner reference
	if err := controllerutil.SetControllerReference(spotInstanceJob, newJob, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Try to create the job with the same name
	if err := r.Create(ctx, newJob); err != nil {
		if errors.IsAlreadyExists(err) {
			// Job with same name already exists (maybe recreated by SpotInstanceJob controller)
			// Check if it already has checkpoint images
			existingJob := &batchv1.Job{}
			if err := r.Get(ctx, types.NamespacedName{Name: newJobName, Namespace: spotInstanceJob.Namespace}, existingJob); err == nil {
				hasCheckpointImages := true
				for i := range existingJob.Spec.Template.Spec.Containers {
					containerName := existingJob.Spec.Template.Spec.Containers[i].Name
					if checkpointImage, found := checkpointImageMap[containerName]; found {
						if existingJob.Spec.Template.Spec.Containers[i].Image != checkpointImage {
							hasCheckpointImages = false
							break
						}
					}
				}
				if hasCheckpointImages {
					logger.Info("Job already has checkpoint images", "job", newJobName)
					return nil
				}
			}
			// Job exists but doesn't have checkpoint images - this shouldn't happen but handle it
			logger.Info("Job with same name exists but doesn't have checkpoint images, will be handled by next reconciliation", "job", newJobName)
			return nil
		}
		return fmt.Errorf("failed to create updated job: %w", err)
	}

	logger.Info("Successfully updated job with checkpoint images", "job", jobName, "containers", len(checkpointImageMap))
	return nil
}

// recreateJobWithCheckpoint recreates a job with optional checkpoint container images

func (r *SpotInstanceJobReconciler) recreateJobWithCheckpoint(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, oldJobName string, checkpointContainers []map[string]interface{}) error {
	logger := log.FromContext(ctx)

	// Generate new job name
	newJobName := fmt.Sprintf("%s-restored-%d", oldJobName, time.Now().Unix())

	// Create job from template
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newJobName,
			Namespace: spotInstanceJob.Namespace,
			Labels: map[string]string{
				spotInstanceJobLabel: spotInstanceJob.Name,
				"restored-from":      oldJobName,
			},
		},
		Spec: spotInstanceJob.Spec.JobTemplate.Spec,
	}

	// Ensure pod template has labels
	if job.Spec.Template.Labels == nil {
		job.Spec.Template.Labels = make(map[string]string)
	}
	job.Spec.Template.Labels[spotInstanceJobLabel] = spotInstanceJob.Name

	// If checkpoint containers provided, update container images
	if len(checkpointContainers) > 0 {
		// Create a map of container name -> checkpoint image
		checkpointImageMap := make(map[string]string)
		for _, c := range checkpointContainers {
			if name, ok := c["name"].(string); ok {
				if image, ok := c["image"].(string); ok {
					checkpointImageMap[name] = image
				}
			}
		}

		// Update container images in the job
		for i := range job.Spec.Template.Spec.Containers {
			containerName := job.Spec.Template.Spec.Containers[i].Name
			if checkpointImage, found := checkpointImageMap[containerName]; found {
				job.Spec.Template.Spec.Containers[i].Image = checkpointImage
				logger.Info("Updating container with checkpoint image", "container", containerName, "image", checkpointImage)
			}
		}
		logger.Info("Recreating job with checkpoint images", "job", newJobName, "containers", len(checkpointContainers))
	} else {
		logger.Info("Recreating job without checkpoint", "job", newJobName)
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(spotInstanceJob, job, r.Scheme); err != nil {
		return err
	}

	// Create job
	if err := r.Create(ctx, job); err != nil {
		return err
	}

	logger.Info("Successfully recreated job", "oldJob", oldJobName, "newJob", newJobName, "containers", len(checkpointContainers))
	return nil
}
