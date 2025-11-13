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
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

func (r *SpotInstanceJobReconciler) hasPendingCheckpoints(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) (bool, error) {
	// List all CheckpointBackup CRs for this SpotInstanceJob
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
		return false, err
	}

	// Check if any checkpoint is in progress (not in terminal state)
	for _, cp := range checkpoints.Items {
		status, found, _ := unstructured.NestedMap(cp.Object, "status")
		if found {
			phase, found, _ := unstructured.NestedString(status, "phase")
			if found {
				// Terminal states: PhaseCompleted, PhaseCompletedPodDeleted, PhaseCompletedWithError, PhaseFailed
				// In-progress states: everything else
				if phase != "Completed" && phase != "CompletedPodDeleted" && phase != "CompletedWithError" && phase != "Failed" {
					return true, nil
				}
			} else {
				// Phase not set yet, assume it's pending/in-progress
				return true, nil
			}
		} else {
			// Status not set yet, assume it's pending/in-progress
			return true, nil
		}
	}

	return false, nil
}

// hasCompletedCheckpoint checks if there's at least one completed checkpoint for the SpotInstanceJob
// Returns: (hasCompleted, completedRecently, error)
// completedRecently is true if the checkpoint was completed within the last 5 minutes
func (r *SpotInstanceJobReconciler) hasCompletedCheckpoint(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) (bool, bool, error) {
	// List all CheckpointBackup CRs for this SpotInstanceJob
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
		return false, false, err
	}

	now := time.Now()
	recentThreshold := 5 * time.Minute
	hasCompleted := false
	completedRecently := false

	// Check if any checkpoint is completed (successful completion)
	for _, cp := range checkpoints.Items {
		status, found, _ := unstructured.NestedMap(cp.Object, "status")
		if found {
			phase, found, _ := unstructured.NestedString(status, "phase")
			if found {
				// Check for successful completion states
				if phase == "Completed" || phase == "CompletedPodDeleted" {
					hasCompleted = true

					// Check completion time to see if it was recent
					completionTimeStr, found, _ := unstructured.NestedString(status, "completionTime")
					if found {
						completionTime, err := time.Parse(time.RFC3339, completionTimeStr)
						if err == nil {
							if now.Sub(completionTime) < recentThreshold {
								completedRecently = true
							}
						}
					} else {
						// If no completionTime, check creation time
						creationTime := cp.GetCreationTimestamp()
						if now.Sub(creationTime.Time) < recentThreshold {
							completedRecently = true
						}
					}
				}
			}
		}
	}

	return hasCompleted, completedRecently, nil
}

// hasRestoredJobs checks if there are any jobs with checkpoint-updated annotation, indicating they were updated with checkpoint images
func (r *SpotInstanceJobReconciler) hasRestoredJobs(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) (bool, error) {
	// List all jobs belonging to this SpotInstanceJob
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
	}); err != nil {
		return false, err
	}

	// Check if any job has checkpoint-updated annotation
	for _, job := range jobList.Items {
		if job.Annotations != nil {
			if _, ok := job.Annotations["training.dcnlab.com/checkpoint-updated"]; ok {
				return true, nil
			}
		}
	}

	return false, nil
}

// processCompletedCheckpoints processes completed checkpoints by updating only jobs that were on preempted nodes
// jobsToUpdate is a set of job names that should be updated (jobs on preempted nodes). If nil or empty, no jobs will be updated.
func (r *SpotInstanceJobReconciler) processCompletedCheckpoints(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, jobsToUpdate map[string]bool) error {
	logger := log.FromContext(ctx)

	// List all CheckpointBackup CRs for this SpotInstanceJob
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
		return err
	}

	// If no jobs to update specified, return early (no jobs should be updated)
	if len(jobsToUpdate) == 0 {
		logger.Info("No jobs to update specified, skipping checkpoint processing")
		return nil
	}

	// Track which jobs have been processed
	processedJobs := make(map[string]bool)

	// Process each completed checkpoint
	for _, cp := range checkpoints.Items {
		status, found, _ := unstructured.NestedMap(cp.Object, "status")
		if !found {
			continue
		}

		phase, found, _ := unstructured.NestedString(status, "phase")
		if !found || (phase != "Completed" && phase != "CompletedPodDeleted") {
			continue
		}

		// Extract containers from checkpoint status - we'll use this for ALL jobs that need updating
		// Try status.containers first, then fall back to status.builtImages
		var checkpointContainers []map[string]interface{}
		containers, found, _ := unstructured.NestedSlice(status, "containers")
		if found && len(containers) > 0 {
			checkpointContainers = make([]map[string]interface{}, len(containers))
			for i, c := range containers {
				if containerMap, ok := c.(map[string]interface{}); ok {
					checkpointContainers[i] = containerMap
				}
			}
		} else {
			// Fall back to builtImages
			builtImages, found, _ := unstructured.NestedSlice(status, "builtImages")
			if found && len(builtImages) > 0 {
				checkpointContainers = make([]map[string]interface{}, len(builtImages))
				for i, img := range builtImages {
					if imgMap, ok := img.(map[string]interface{}); ok {
						// Convert builtImages format to containers format
						containerName, _ := imgMap["containerName"].(string)
						imageName, _ := imgMap["imageName"].(string)
						checkpointContainers[i] = map[string]interface{}{
							"name":  containerName,
							"image": imageName,
						}
					}
				}
			}
		}

		if len(checkpointContainers) == 0 {
			logger.Info("No containers found in completed checkpoint, skipping", "checkpoint", cp.GetName())
			continue
		}

		// Get the job name that this checkpoint was created from (should NOT be updated)
		checkpointSourceJob := ""
		if jobLabel, found := cp.GetLabels()["training.dcnlab.com/job"]; found {
			checkpointSourceJob = jobLabel
			logger.Info("Checkpoint was created from job", "checkpoint", cp.GetName(), "sourceJob", checkpointSourceJob)
		}

		// Update ALL jobs that were on preempted nodes (not the job that was checkpointed)
		// The checkpoint was created from an alive replica, but we need to update the replicas on preempted nodes
		for jobNameToUpdate := range jobsToUpdate {
			// CRITICAL: Never update the job that the checkpoint was created from (it's alive and running)
			if checkpointSourceJob != "" && jobNameToUpdate == checkpointSourceJob {
				logger.Info("Skipping checkpoint source job (it's alive and should keep running)", "job", jobNameToUpdate, "checkpoint", cp.GetName())
				continue
			}

			// Skip if already processed
			if processedJobs[jobNameToUpdate] {
				continue
			}

			// Check if job still exists
			job := &batchv1.Job{}
			if err := r.Get(ctx, types.NamespacedName{Name: jobNameToUpdate, Namespace: spotInstanceJob.Namespace}, job); err != nil {
				if errors.IsNotFound(err) {
					// Job doesn't exist, might be being recreated - skip for now
					logger.Info("Job on preempted node doesn't exist, skipping", "job", jobNameToUpdate)
					continue
				}
				logger.Error(err, "Failed to get job on preempted node", "job", jobNameToUpdate)
				continue
			}

			// Check if job has already been updated with checkpoint
			if job.Annotations != nil {
				if _, ok := job.Annotations["training.dcnlab.com/checkpoint-updated"]; ok {
					// Job was already updated, skip
					processedJobs[jobNameToUpdate] = true
					logger.Info("Job on preempted node already updated with checkpoint", "job", jobNameToUpdate)
					continue
				}
			}

			// Check if job is being deleted
			if !job.DeletionTimestamp.IsZero() {
				// Job is being deleted, skip
				logger.Info("Job on preempted node is being deleted, skipping", "job", jobNameToUpdate)
				continue
			}

			logger.Info("Processing completed checkpoint, updating job on preempted node with checkpoint images", "checkpoint", cp.GetName(), "job", jobNameToUpdate, "checkpointedFrom", cp.GetLabels()["training.dcnlab.com/job"])

			// Update job with checkpoint images (updates container images instead of deleting/recreating)
			if err := r.updateJobWithCheckpoint(ctx, spotInstanceJob, jobNameToUpdate, checkpointContainers); err != nil {
				logger.Error(err, "Failed to update job with checkpoint", "job", jobNameToUpdate)
				continue
			}

			processedJobs[jobNameToUpdate] = true
			logger.Info("Successfully processed completed checkpoint for job on preempted node", "checkpoint", cp.GetName(), "job", jobNameToUpdate)
		}
	}

	return nil
}

// handlePreemption checks for preempted instances and handles them appropriately
func (r *SpotInstanceJobReconciler) createCheckpointForJob(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, jobName string, immediate bool) error {
	logger := log.FromContext(ctx)

	// Check if a CheckpointBackup already exists for this job
	checkpoints := &unstructured.UnstructuredList{}
	checkpoints.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "migration.dcnlab.com",
		Version: "v1",
		Kind:    "CheckpointBackupList",
	})

	labelSelector := client.MatchingLabels{
		"training.dcnlab.com/job": jobName,
	}
	if err := r.List(ctx, checkpoints, client.InNamespace(spotInstanceJob.Namespace), labelSelector); err == nil {
		hasInProgressCheckpoint := false
		hasCompletedCheckpoint := false

		// Check all checkpoints for this job
		for _, cp := range checkpoints.Items {
			status, found, _ := unstructured.NestedMap(cp.Object, "status")
			if found {
				phase, found, _ := unstructured.NestedString(status, "phase")
				if found {
					// Check if checkpoint is in progress (not in terminal state)
					// Terminal states: PhaseCompleted, PhaseCompletedPodDeleted, PhaseCompletedWithError, PhaseFailed
					// In-progress states: PhaseCheckpointing, PhaseCheckpointed, PhaseImageBuilding, PhaseImageBuilt, PhaseImagePushing, PhaseImagePushed
					if phase != "Completed" && phase != "CompletedPodDeleted" && phase != "CompletedWithError" && phase != "Failed" {
						hasInProgressCheckpoint = true
						logger.Info("CheckpointBackup already exists for job and is in progress", "job", jobName, "checkpoint", cp.GetName(), "phase", phase)
					} else if phase == "Completed" || phase == "CompletedPodDeleted" {
						hasCompletedCheckpoint = true
					}
				} else {
					// Phase not set yet, assume it's pending/in-progress
					hasInProgressCheckpoint = true
					logger.Info("CheckpointBackup already exists for job (phase not set yet)", "job", jobName, "checkpoint", cp.GetName())
				}
			} else {
				// Status not set yet, assume it's pending/in-progress
				hasInProgressCheckpoint = true
				logger.Info("CheckpointBackup already exists for job (no status yet)", "job", jobName, "checkpoint", cp.GetName())
			}
		}

		// If there's an in-progress checkpoint, don't create a new one
		if hasInProgressCheckpoint {
			return nil
		}

		// If there's a completed checkpoint, check if the job is being recreated
		if hasCompletedCheckpoint {
			// Check if the job still exists (hasn't been deleted/recreated yet)
			job := &batchv1.Job{}
			if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: spotInstanceJob.Namespace}, job); err == nil {
				// Job still exists, check if it's being deleted or has been recreated
				if !job.DeletionTimestamp.IsZero() {
					// Job is being deleted, don't create a new checkpoint
					logger.Info("Job is being deleted, skipping checkpoint creation", "job", jobName)
					return nil
				}

				// Check if job has "restored-from" label (already recreated)
				if _, ok := job.Labels["restored-from"]; ok {
					// Job was already recreated, it's safe to create a new checkpoint if needed
					logger.Info("Job was already recreated, allowing new checkpoint", "job", jobName)
				} else {
					// Job exists and hasn't been recreated yet, wait for recreation to complete
					logger.Info("Completed checkpoint exists and job hasn't been recreated yet, skipping new checkpoint", "job", jobName)
					return nil
				}
			} else if errors.IsNotFound(err) {
				// Job doesn't exist, it might have been deleted and is being recreated
				// Check if there's a restored job
				restoredJobList := &batchv1.JobList{}
				if err := r.List(ctx, restoredJobList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
					"restored-from": jobName,
				}); err == nil && len(restoredJobList.Items) > 0 {
					// Job was recreated, it's safe to create a new checkpoint if needed
					logger.Info("Job was recreated, allowing new checkpoint", "job", jobName, "restoredJob", restoredJobList.Items[0].Name)
				} else {
					// Job doesn't exist and hasn't been recreated yet, wait
					logger.Info("Completed checkpoint exists but job doesn't exist yet (recreation in progress), skipping new checkpoint", "job", jobName)
					return nil
				}
			}
		}
	}

	// List pods for the job
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		"job-name": jobName,
	}); err != nil {
		// Try alternative label
		if err := r.List(ctx, podList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
			"batch.kubernetes.io/job-name": jobName,
		}); err != nil {
			return err
		}
	}

	if len(podList.Items) == 0 {
		logger.Info("No pods found for job, skipping checkpoint", "job", jobName)
		return nil
	}

	// Find first alive pod (pod with node that exists and is Ready)
	var pod *corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Spec.NodeName == "" {
			continue
		}

		// Check if node exists and is Ready
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: p.Spec.NodeName}, node); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("Pod node not found, skipping", "pod", p.Name, "node", p.Spec.NodeName)
				continue
			}
			logger.Error(err, "Failed to get node", "node", p.Spec.NodeName)
			continue
		}

		// Check if node is Ready
		nodeReady := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				nodeReady = true
				break
			}
		}

		if nodeReady {
			pod = p
			break
		}
	}

	if pod == nil {
		logger.Info("No alive pods found for job, skipping checkpoint", "job", jobName)
		return nil
	}

	timestamp := time.Now().Unix()
	checkpointName := fmt.Sprintf("%s-%s-%d", spotInstanceJob.Name, pod.Name, timestamp)

	// Parse image repo to extract registry URL and repository
	imageRepo := spotInstanceJob.Spec.CheckpointConfig.CheckpointImageRepo
	registryURL := "docker.io"
	repository := imageRepo

	// Parse registry URL if provided (e.g., "docker.io/lehuannhatrang/gpu-checkpoints" or "gcr.io/project/repo")
	if strings.Contains(imageRepo, "/") {
		parts := strings.SplitN(imageRepo, "/", 2)
		if len(parts) == 2 {
			// Check if first part is a registry (contains dot or common registry names)
			if strings.Contains(parts[0], ".") || parts[0] == "docker.io" || parts[0] == "gcr.io" || parts[0] == "ghcr.io" || parts[0] == "quay.io" {
				registryURL = parts[0]
				repository = parts[1]
			}
		}
	}

	spec := map[string]interface{}{
		"schedule": "immediately",
		"stopPod":  false,
		"podRef": map[string]interface{}{
			"name":      pod.Name,
			"namespace": pod.Namespace,
		},
		"resourceRef": map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"name":       jobName,
			"namespace":  spotInstanceJob.Namespace,
		},
		"registry": map[string]interface{}{
			"url":        registryURL,
			"repository": repository,
		},
	}

	// Add credential reference if provided
	if spotInstanceJob.Spec.CheckpointConfig.CheckpointRepoCredentialRef != nil {
		registry := spec["registry"].(map[string]interface{})
		registry["secretRef"] = map[string]interface{}{
			"name":      spotInstanceJob.Spec.CheckpointConfig.CheckpointRepoCredentialRef.Name,
			"namespace": spotInstanceJob.Spec.CheckpointConfig.CheckpointRepoCredentialRef.Namespace,
		}
	}

	// Add containers section - include all containers from the pod
	// Image format: {imageRepo}:{container_name}-{timestamp}
	containers := []map[string]interface{}{}
	for _, container := range pod.Spec.Containers {
		// Format: {imageRepo}:{container_name}-{timestamp}
		checkpointImage := fmt.Sprintf("%s:%s-%d", repository, container.Name, timestamp)
		containers = append(containers, map[string]interface{}{
			"name":  container.Name,
			"image": checkpointImage,
		})
	}
	spec["containers"] = containers

	checkpoint := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "migration.dcnlab.com/v1",
			"kind":       "CheckpointBackup",
			"metadata": map[string]interface{}{
				"name":      checkpointName,
				"namespace": spotInstanceJob.Namespace,
				"labels": map[string]interface{}{
					spotInstanceJobLabel:      spotInstanceJob.Name,
					"training.dcnlab.com/job": jobName,
				},
			},
			"spec": spec,
		},
	}

	if err := r.Create(ctx, checkpoint); err != nil {
		if errors.IsAlreadyExists(err) {
			logger.Info("CheckpointBackup already exists", "name", checkpointName)
			return nil
		}
		return err
	}

	logger.Info("Created CheckpointBackup CR",
		"name", checkpointName,
		"pod", pod.Name,
		"node", pod.Spec.NodeName,
		"job", jobName,
		"immediate", immediate)

	// NOTE: We no longer launch a goroutine here to update jobs.
	// The synchronous processCompletedCheckpoints() flow in handlePreemption() handles
	// updating jobs on preempted nodes correctly, ensuring we update the right replicas.
	// The goroutine approach was updating the checkpointed job (source) instead of
	// the preempted job (target), which caused wrong replicas to be updated.

	return nil
}

// waitForCheckpointAndRecreateJob waits for checkpoint completion and recreates the job
func (r *SpotInstanceJobReconciler) waitForCheckpointAndRecreateJob(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, oldJobName string) {
	logger := log.FromContext(ctx)
	logger.Info("Waiting for checkpoint completion", "job", oldJobName)

	// Wait up to 10 minutes for checkpoint (typically takes 3-5 minutes)
	timeout := time.After(20 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var checkpointContainers []map[string]interface{}
	for {
		select {
		case <-timeout:
			logger.Error(nil, "Timeout waiting for checkpoint", "job", oldJobName)
			return
		case <-ticker.C:
			// List CheckpointBackup CRs for this job
			checkpoints := &unstructured.UnstructuredList{}
			checkpoints.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "migration.dcnlab.com",
				Version: "v1",
				Kind:    "CheckpointBackupList",
			})

			labelSelector := client.MatchingLabels{
				"training.dcnlab.com/job": oldJobName,
			}
			if err := r.List(ctx, checkpoints, client.InNamespace(spotInstanceJob.Namespace), labelSelector); err != nil {
				logger.Error(err, "Failed to list checkpoints")
				continue
			}

			// Find completed checkpoint (PhaseCompleted or PhaseCompletedPodDeleted)
			var foundContainers []map[string]interface{}
			for _, cp := range checkpoints.Items {
				status, found, _ := unstructured.NestedMap(cp.Object, "status")
				if !found {
					continue
				}

				phase, found, _ := unstructured.NestedString(status, "phase")
				logger.Info("Checking checkpoint", "name", cp.GetName(), "status", status, "phase", phase)
				if found && (phase == "Completed" || phase == "CompletedPodDeleted") {
					// Extract containers array from status
					// Try status.containers first, then fall back to status.builtImages
					containers, found, _ := unstructured.NestedSlice(status, "containers")
					if found && len(containers) > 0 {
						foundContainers = make([]map[string]interface{}, len(containers))
						for i, c := range containers {
							if containerMap, ok := c.(map[string]interface{}); ok {
								foundContainers[i] = containerMap
							}
						}
						break
					} else {
						// Fall back to builtImages
						builtImages, found, _ := unstructured.NestedSlice(status, "builtImages")
						if found && len(builtImages) > 0 {
							foundContainers = make([]map[string]interface{}, len(builtImages))
							for i, img := range builtImages {
								if imgMap, ok := img.(map[string]interface{}); ok {
									// Convert builtImages format to containers format
									containerName, _ := imgMap["containerName"].(string)
									imageName, _ := imgMap["imageName"].(string)
									foundContainers[i] = map[string]interface{}{
										"name":  containerName,
										"image": imageName,
									}
								}
							}
							break
						}
					}
				}
			}

			if len(foundContainers) > 0 {
				checkpointContainers = foundContainers
				logger.Info("Checkpoint completed", "job", oldJobName, "containers", len(checkpointContainers))

				// Check if job still exists
				job := &batchv1.Job{}
				if err := r.Get(ctx, types.NamespacedName{Name: oldJobName, Namespace: spotInstanceJob.Namespace}, job); err != nil {
					if errors.IsNotFound(err) {
						logger.Info("Job doesn't exist, may have been deleted or updated", "job", oldJobName)
						return
					}
					logger.Error(err, "Failed to get job", "job", oldJobName)
					continue
				}

				// Check if job has already been updated with checkpoint
				if job.Annotations != nil {
					if _, ok := job.Annotations["training.dcnlab.com/checkpoint-updated"]; ok {
						logger.Info("Job was already updated with checkpoint", "job", oldJobName)
						return
					}
				}

				// Check if job is being deleted
				if !job.DeletionTimestamp.IsZero() {
					logger.Info("Job is being deleted, waiting for deletion to complete", "job", oldJobName)
					continue
				}

				// Update job with checkpoint images (updates container images instead of deleting/recreating)
				if err := r.updateJobWithCheckpoint(ctx, spotInstanceJob, oldJobName, checkpointContainers); err != nil {
					logger.Error(err, "Failed to update job with checkpoint", "job", oldJobName)
					continue
				}

				logger.Info("Successfully updated job with checkpoint images", "job", oldJobName)
				return
			}
		}
	}
}

// recreateJobFromLatestCheckpoint finds latest checkpoint and recreates job
func (r *SpotInstanceJobReconciler) recreateJobFromLatestCheckpoint(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, oldJobName string) {
	logger := log.FromContext(ctx)
	logger.Info("Recreating job from latest checkpoint", "job", oldJobName)

	// List all CheckpointBackup CRs for this SpotInstanceJob
	checkpoints := &unstructured.UnstructuredList{}
	checkpoints.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "migration.dcnlab.com",
		Version: "v1",
		Kind:    "CheckpointBackupList",
	})

	labelSelector := client.MatchingLabels{
		"training.dcnlab.com/spot-instance-job": spotInstanceJob.Name,
	}
	if err := r.List(ctx, checkpoints, client.InNamespace(spotInstanceJob.Namespace), labelSelector); err != nil {
		logger.Error(err, "Failed to list checkpoints")
		return
	}

	// Find latest completed checkpoint (PhaseCompleted or PhaseCompletedPodDeleted)
	var latestContainers []map[string]interface{}
	var latestTime time.Time

	for _, cp := range checkpoints.Items {
		status, found, _ := unstructured.NestedMap(cp.Object, "status")
		if !found {
			continue
		}

		phase, found, _ := unstructured.NestedString(status, "phase")
		if !found || (phase != "Completed" && phase != "CompletedPodDeleted") {
			continue
		}

		completionTimeStr, found, _ := unstructured.NestedString(status, "completionTime")
		if !found {
			continue
		}

		completionTime, err := time.Parse(time.RFC3339, completionTimeStr)
		if err != nil {
			continue
		}

		// Extract containers array from status
		// Try status.containers first, then fall back to status.builtImages
		var containers []map[string]interface{}
		containersSlice, found, _ := unstructured.NestedSlice(status, "containers")
		if found && len(containersSlice) > 0 {
			containers = make([]map[string]interface{}, len(containersSlice))
			for i, c := range containersSlice {
				if containerMap, ok := c.(map[string]interface{}); ok {
					containers[i] = containerMap
				}
			}
		} else {
			// Fall back to builtImages
			builtImages, found, _ := unstructured.NestedSlice(status, "builtImages")
			if found && len(builtImages) > 0 {
				containers = make([]map[string]interface{}, len(builtImages))
				for i, img := range builtImages {
					if imgMap, ok := img.(map[string]interface{}); ok {
						// Convert builtImages format to containers format
						containerName, _ := imgMap["containerName"].(string)
						imageName, _ := imgMap["imageName"].(string)
						containers[i] = map[string]interface{}{
							"name":  containerName,
							"image": imageName,
						}
					}
				}
			}
		}

		if len(containers) > 0 {
			if latestContainers == nil || completionTime.After(latestTime) {
				latestContainers = containers
				latestTime = completionTime
			}
		}
	}

	if len(latestContainers) == 0 {
		logger.Info("No completed checkpoint found, cannot update job", "job", oldJobName)
		return
	}

	logger.Info("Found latest checkpoint", "job", oldJobName, "containers", len(latestContainers))

	// Check if job exists
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: oldJobName, Namespace: spotInstanceJob.Namespace}, job); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Job doesn't exist, cannot update", "job", oldJobName)
			return
		}
		logger.Error(err, "Failed to get job", "job", oldJobName)
		return
	}

	// Update job with checkpoint images (updates container images instead of deleting/recreating)
	if err := r.updateJobWithCheckpoint(ctx, spotInstanceJob, oldJobName, latestContainers); err != nil {
		logger.Error(err, "Failed to update job with checkpoint", "job", oldJobName)
	}
}

// deleteJob deletes a job
func (r *SpotInstanceJobReconciler) findJobsOnNode(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, nodeName string) ([]string, error) {
	logger := log.FromContext(ctx)

	// List all pods in the namespace
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(spotInstanceJob.Namespace)); err != nil {
		return nil, err
	}

	jobs := make(map[string]bool)
	for _, pod := range podList.Items {
		// Check if pod is on the target node
		if pod.Spec.NodeName != nodeName {
			continue
		}

		// Check if pod belongs to this SpotInstanceJob
		if pod.Labels[spotInstanceJobLabel] != spotInstanceJob.Name {
			continue
		}

		// Get job name from labels
		jobName := ""
		if name, ok := pod.Labels["job-name"]; ok {
			jobName = name
		} else if name, ok := pod.Labels["batch.kubernetes.io/job-name"]; ok {
			jobName = name
		}

		if jobName != "" {
			jobs[jobName] = true
		}
	}

	jobList := make([]string, 0, len(jobs))
	for job := range jobs {
		jobList = append(jobList, job)
	}

	logger.Info("Jobs found on node", "node", nodeName, "count", len(jobList), "jobs", jobList)
	return jobList, nil
}

// createCheckpointForJob creates a CheckpointBackup CR for a job
