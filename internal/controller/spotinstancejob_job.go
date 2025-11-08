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
		skipped := int32(0) // Track how many times we skipped without creating
		for created < needed && replicaIndex < desiredReplicas*2 {
			// Find next replica index without a job
			for replicaIndicesWithJobs[replicaIndex] {
				replicaIndex++
				if replicaIndex >= desiredReplicas*2 {
					logger.Info("Too many replica indices checked, stopping", "replicaIndex", replicaIndex)
					break
				}
			}

			// Safety check: if replicaIndex is too high, break out
			if replicaIndex >= desiredReplicas*2 {
				logger.Info("Reached maximum replica index limit, stopping job creation", "replicaIndex", replicaIndex, "limit", desiredReplicas*2)
				break
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

			// Check if ProvisionedInstance exists and is ready for this replica index
			provisionedInstanceList := &trainingv1alpha1.ProvisionedInstanceList{}
			if err := r.List(ctx, provisionedInstanceList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
				spotInstanceJobLabel: spotInstanceJob.Name,
				jobReplicaLabel:      fmt.Sprintf("%d", replicaIndex),
			}); err == nil && len(provisionedInstanceList.Items) > 0 {
				// Find an ACTIVE ProvisionedInstance (skip terminated ones)
				var activePI *trainingv1alpha1.ProvisionedInstance
				for i := range provisionedInstanceList.Items {
					pi := &provisionedInstanceList.Items[i]
					// Skip terminated/preempted instances
					if pi.Status.PreemptionNotice || pi.Status.State == "TERMINATED" || pi.Status.State == "STOPPING" {
						continue
					}
					activePI = pi
					break
				}

				if activePI != nil {
					if !activePI.Status.JoinedCluster || activePI.Status.NodeName == "" {
						// ProvisionedInstance exists but hasn't joined the cluster yet, wait
						logger.Info("Waiting for ProvisionedInstance to join cluster before creating job", "instance", activePI.Name, "replicaIndex", replicaIndex, "state", activePI.Status.State, "joinedCluster", activePI.Status.JoinedCluster)
						replicaIndex++
						skipped++
						// If we've skipped too many times, break to avoid infinite loop
						if skipped >= desiredReplicas*2 {
							logger.Info("Too many skips without creating jobs, will retry on next reconciliation", "skipped", skipped)
							break
						}
						continue
					}
					// Active ProvisionedInstance is ready, proceed to create job
				} else {
					// Only terminated instances found, treat as if no instance exists
					logger.Info("Only terminated ProvisionedInstances found for replica index, waiting for new one", "replicaIndex", replicaIndex)
					replicaIndex++
					skipped++
					if skipped >= desiredReplicas*2 {
						logger.Info("Too many skips without creating jobs, will retry on next reconciliation", "skipped", skipped)
						break
					}
					continue
				}
			} else if err == nil && len(provisionedInstanceList.Items) == 0 {
				// No ProvisionedInstance for this replica index yet, wait for it to be created
				logger.Info("No ProvisionedInstance found for replica index, waiting", "replicaIndex", replicaIndex)
				replicaIndex++
				skipped++
				// If we've skipped too many times, break to avoid infinite loop
				if skipped >= desiredReplicas*2 {
					logger.Info("Too many skips without creating jobs, will retry on next reconciliation", "skipped", skipped)
					break
				}
				continue
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
			skipped = 0 // Reset skipped counter after successful creation
		}
	} else if len(jobsBeingUpdated) > 0 {
		logger.Info("All replica indices have jobs, some are being updated", "activeReplicaCount", activeReplicaCount, "jobsBeingUpdated", len(jobsBeingUpdated))
	}

	return nil
}

// createJobReplica creates a new job replica

func (r *SpotInstanceJobReconciler) createJobReplica(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, jobName string, replicaIndex int32) error {
	logger := log.FromContext(ctx)

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

	// Find the ProvisionedInstance with the same replica index and assign pod to its node
	provisionedInstanceList := &trainingv1alpha1.ProvisionedInstanceList{}
	if err := r.List(ctx, provisionedInstanceList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
		jobReplicaLabel:      fmt.Sprintf("%d", replicaIndex),
	}); err != nil {
		logger.Error(err, "Failed to list ProvisionedInstances for replica index", "replicaIndex", replicaIndex)
	} else if len(provisionedInstanceList.Items) > 0 {
		// Find the ACTIVE (non-preempted, RUNNING) ProvisionedInstance
		var activePI *trainingv1alpha1.ProvisionedInstance
		for i := range provisionedInstanceList.Items {
			pi := &provisionedInstanceList.Items[i]
			// Skip preempted/terminated instances
			if pi.Status.PreemptionNotice || pi.Status.State == "TERMINATED" || pi.Status.State == "STOPPING" {
				logger.Info("Skipping preempted/terminated ProvisionedInstance", "instance", pi.Name, "state", pi.Status.State, "preemptionNotice", pi.Status.PreemptionNotice)
				continue
			}
			// Only use instances that are RUNNING and have joined the cluster
			if pi.Status.State == "RUNNING" && pi.Status.JoinedCluster && pi.Status.NodeName != "" {
				activePI = pi
				logger.Info("Found active ProvisionedInstance for job replica", "instance", pi.Name, "node", pi.Status.NodeName, "replicaIndex", replicaIndex)
				break
			}
		}

		if activePI != nil {
			// Assign pod to this node
			job.Spec.Template.Spec.NodeName = activePI.Status.NodeName
			logger.Info("Assigning job replica to node", "job", jobName, "replicaIndex", replicaIndex, "node", activePI.Status.NodeName, "instance", activePI.Name)
		} else {
			logger.Info("No active ProvisionedInstance ready yet, pod will be scheduled normally", "replicaIndex", replicaIndex)
		}
	} else {
		logger.Info("No ProvisionedInstance found for replica index, pod will be scheduled normally", "replicaIndex", replicaIndex)
	}

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

	// Clear the old nodeName - we'll assign to a new node
	updatedJobSpec.Template.Spec.NodeName = ""
	logger.Info("Cleared old nodeName from job spec", "job", jobName, "oldNodeName", job.Spec.Template.Spec.NodeName)

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
					Labels:      make(map[string]string),
					Annotations: make(map[string]string),
				},
				Spec: updatedJobSpec.Template.Spec,
			},
		},
	}

	// Preserve original pod template annotations from the job
	if job.Spec.Template.Annotations != nil {
		for k, v := range job.Spec.Template.Annotations {
			newJob.Spec.Template.Annotations[k] = v
		}
	}

	// Set only our custom labels (clear auto-generated labels)
	newJob.Spec.Template.Labels[spotInstanceJobLabel] = spotInstanceJob.Name
	newJob.Spec.Template.Labels[jobReplicaLabel] = fmt.Sprintf("%d", replicaIndex)

	// Find the ProvisionedInstance with the same replica index and assign pod to its node
	provisionedInstanceList := &trainingv1alpha1.ProvisionedInstanceList{}
	if err := r.List(ctx, provisionedInstanceList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
		jobReplicaLabel:      fmt.Sprintf("%d", replicaIndex),
	}); err != nil {
		logger.Error(err, "Failed to list ProvisionedInstances for replica index", "replicaIndex", replicaIndex)
	} else if len(provisionedInstanceList.Items) > 0 {
		// Find the ACTIVE (non-preempted, RUNNING) ProvisionedInstance
		var activePI *trainingv1alpha1.ProvisionedInstance
		for i := range provisionedInstanceList.Items {
			pi := &provisionedInstanceList.Items[i]
			// Skip preempted/terminated instances
			if pi.Status.PreemptionNotice || pi.Status.State == "TERMINATED" || pi.Status.State == "STOPPING" {
				logger.Info("Skipping preempted/terminated ProvisionedInstance", "instance", pi.Name, "state", pi.Status.State, "preemptionNotice", pi.Status.PreemptionNotice)
				continue
			}
			// Only use instances that are RUNNING and have joined the cluster
			if pi.Status.State == "RUNNING" && pi.Status.JoinedCluster && pi.Status.NodeName != "" {
				activePI = pi
				logger.Info("Found active ProvisionedInstance for updated job", "instance", pi.Name, "node", pi.Status.NodeName, "replicaIndex", replicaIndex)
				break
			}
		}

		if activePI != nil {
			// Assign pod to this node
			newJob.Spec.Template.Spec.NodeName = activePI.Status.NodeName
			logger.Info("Assigning updated job to node", "job", jobName, "replicaIndex", replicaIndex, "node", activePI.Status.NodeName, "instance", activePI.Name)
		} else {
			// No active ProvisionedInstance ready yet - wait for it to be provisioned
			logger.Info("No active ProvisionedInstance ready yet for updated job, will retry later", "replicaIndex", replicaIndex)
			return fmt.Errorf("waiting for ProvisionedInstance with replica index %d to be ready", replicaIndex)
		}
	} else {
		// No ProvisionedInstance found - wait for it to be created
		logger.Info("No ProvisionedInstance found for replica index for updated job, will retry later", "replicaIndex", replicaIndex)
		return fmt.Errorf("waiting for ProvisionedInstance with replica index %d to be created", replicaIndex)
	}

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
