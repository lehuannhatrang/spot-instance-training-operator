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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

func (r *SpotInstanceJobReconciler) handlePreemption(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) error {
	logger := log.FromContext(ctx)

	// List ProvisionedInstances
	provisionedInstanceList := &trainingv1alpha1.ProvisionedInstanceList{}
	if err := r.List(ctx, provisionedInstanceList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
	}); err != nil {
		return err
	}

	// Track preempted instances that need replacement
	preemptedInstances := []string{}
	// Track jobs that were on preempted nodes and need to be updated
	jobsToUpdate := make(map[string]bool)

	// Check for preempted instances
	for _, pi := range provisionedInstanceList.Items {
		if pi.Status.PreemptionNotice || pi.Status.State == "TERMINATED" || pi.Status.State == "STOPPING" {
			logger.Info("Detected preempted/terminated instance", "instance", pi.Name)
			preemptedInstances = append(preemptedInstances, pi.Name)

			// Get replica index from ProvisionedInstance labels - this is the authoritative source
			replicaIndex := int32(-1)
			if replicaIndexStr, ok := pi.Labels[jobReplicaLabel]; ok {
				if idx, err := strconv.ParseInt(replicaIndexStr, 10, 32); err == nil {
					replicaIndex = int32(idx)
					logger.Info("Preempted instance has replica index", "instance", pi.Name, "replicaIndex", replicaIndex)
				}
			}

			// Find the job with this replica index
			if replicaIndex >= 0 {
				// List all jobs for this SpotInstanceJob with this replica index
				jobList := &batchv1.JobList{}
				if err := r.List(ctx, jobList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
					spotInstanceJobLabel: spotInstanceJob.Name,
					jobReplicaLabel:      fmt.Sprintf("%d", replicaIndex),
				}); err == nil {
					foundJobForIndex := false
					for _, job := range jobList.Items {
						// Skip jobs that are being deleted or already updated
						if !job.DeletionTimestamp.IsZero() {
							logger.Info("Skipping job being deleted for preempted replica", "job", job.Name, "replicaIndex", replicaIndex)
							continue
						}
						if job.Annotations != nil {
							if _, ok := job.Annotations["training.dcnlab.com/checkpoint-updated"]; ok {
								logger.Info("Job already updated with checkpoint, skipping", "job", job.Name, "replicaIndex", replicaIndex)
								continue
							}
						}
						// This is the job to update
						jobsToUpdate[job.Name] = true
						foundJobForIndex = true
						logger.Info("✓ Job for preempted replica WILL BE UPDATED with checkpoint", "job", job.Name, "replicaIndex", replicaIndex, "instance", pi.Name, "nodeName", pi.Status.NodeName)
					}
					if !foundJobForIndex {
						logger.Info("No job found for preempted replica index (may have been deleted already)", "replicaIndex", replicaIndex, "instance", pi.Name)
					}
				} else {
					logger.Error(err, "Failed to list jobs for replica index", "replicaIndex", replicaIndex)
				}
			} else {
				// Fallback: If ProvisionedInstance doesn't have replica index label (old instances),
				// try to find jobs by node name
				logger.Info("ProvisionedInstance missing replica index label, falling back to node-based lookup", "instance", pi.Name)
				if pi.Status.NodeName != "" {
					jobsOnPreemptedNode, err := r.findJobsOnNode(ctx, spotInstanceJob, pi.Status.NodeName)
					if err != nil {
						logger.Error(err, "Failed to find jobs on preempted node", "node", pi.Status.NodeName)
					} else {
						for _, jobName := range jobsOnPreemptedNode {
							jobsToUpdate[jobName] = true
							logger.Info("Job found on preempted node (fallback)", "job", jobName, "node", pi.Status.NodeName)
						}
					}
				}
			}

			// Count other active replicas
			activeReplicas := 0
			for _, otherPi := range provisionedInstanceList.Items {
				if otherPi.Name != pi.Name && otherPi.Status.State == "RUNNING" && !otherPi.Status.PreemptionNotice {
					activeReplicas++
				}
			}

			if activeReplicas > 0 {
				logger.Info("Active replicas detected, will checkpoint alive replica and update preempted jobs", "activeReplicas", activeReplicas, "jobsToUpdate", len(jobsToUpdate))

				// Other replicas alive, first check if there are completed checkpoints that need to be processed
				// Process completed checkpoints synchronously before creating new ones
				// Only update jobs that were on preempted nodes, keep alive pods running
				if len(jobsToUpdate) > 0 {
					logger.Info("Processing completed checkpoints for jobs on preempted nodes", "jobsToUpdate", jobsToUpdate)
				}
				if err := r.processCompletedCheckpoints(ctx, spotInstanceJob, jobsToUpdate); err != nil {
					logger.Error(err, "Failed to process completed checkpoints")
				}

				// Check if we've already handled this preemption
				// Check if there's already a completed checkpoint for this SpotInstanceJob
				hasCompletedCheckpoint, completedRecently, err := r.hasCompletedCheckpoint(ctx, spotInstanceJob)
				if err != nil {
					logger.Error(err, "Failed to check for completed checkpoint")
				} else if hasCompletedCheckpoint {
					// Check if jobs were already recreated (have "restored-from" label)
					hasRestoredJobs, err := r.hasRestoredJobs(ctx, spotInstanceJob)
					if err != nil {
						logger.Error(err, "Failed to check for restored jobs")
					} else if hasRestoredJobs {
						// Checkpoint completed and jobs were recreated, skip creating new checkpoint
						logger.Info("Checkpoint already completed and jobs recreated for preempted instance, skipping", "instance", pi.Name)
						continue
					} else if completedRecently {
						// Checkpoint completed recently but jobs not recreated yet, wait a bit
						// This prevents creating duplicate checkpoints while job recreation is in progress
						logger.Info("Checkpoint completed recently, waiting for job recreation before creating new checkpoint", "instance", pi.Name)
						continue
					}
				}

				// Other replicas alive, create immediate checkpoint
				// Find a pod from this SpotInstanceJob that's running on an alive node and checkpoint that
				// (Don't checkpoint pods from jobs on the preempted node since those nodes are gone)
				logger.Info("Other replicas alive, creating immediate checkpoint for a pod on alive node")

				// Find all pods belonging to this SpotInstanceJob
				podList := &corev1.PodList{}
				if err := r.List(ctx, podList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
					spotInstanceJobLabel: spotInstanceJob.Name,
				}); err != nil {
					logger.Error(err, "Failed to list pods for SpotInstanceJob")
					continue
				}

				// Find first alive pod (on a node that exists and is Ready)
				// Skip pods on the preempted node
				var alivePod *corev1.Pod
				for i := range podList.Items {
					p := &podList.Items[i]
					if p.Spec.NodeName == "" {
						continue
					}

					// Skip pods on the preempted node
					if pi.Status.NodeName != "" && p.Spec.NodeName == pi.Status.NodeName {
						logger.Info("Skipping pod on preempted node", "pod", p.Name, "node", p.Spec.NodeName)
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
						alivePod = p
						logger.Info("Found alive pod for checkpoint", "pod", p.Name, "node", p.Spec.NodeName, "job", p.Labels["job-name"])
						break
					}
				}

				if alivePod != nil {
					// Get job name from pod labels
					jobName := ""
					if name, ok := alivePod.Labels["job-name"]; ok {
						jobName = name
					} else if name, ok := alivePod.Labels["batch.kubernetes.io/job-name"]; ok {
						jobName = name
					}

					if jobName != "" {
						if err := r.createCheckpointForJob(ctx, spotInstanceJob, jobName, true); err != nil {
							logger.Error(err, "Failed to create checkpoint", "job", jobName)
						}
					} else {
						logger.Info("Could not determine job name from pod labels", "pod", alivePod.Name)
					}
				} else {
					logger.Info("No alive pods found for SpotInstanceJob (excluding preempted node)", "spotInstanceJob", spotInstanceJob.Name, "preemptedNode", pi.Status.NodeName)
				}
			} else {
				// No other replicas alive, restore from latest checkpoint
				// Find jobs that were on the preempted node to know which jobs to recreate
				if pi.Status.NodeName != "" {
					jobs, err := r.findJobsOnNode(ctx, spotInstanceJob, pi.Status.NodeName)
					if err != nil {
						logger.Error(err, "Failed to find jobs on preempted node", "node", pi.Status.NodeName)
					} else if len(jobs) > 0 {
						logger.Info("No other replicas alive, restoring from latest checkpoint", "jobs", jobs)
						for _, jobName := range jobs {
							go r.recreateJobFromLatestCheckpoint(context.Background(), spotInstanceJob, jobName)
						}
					}
				}
			}
		}
	}

	// Recreate ProvisionedInstances for terminated instances
	if len(preemptedInstances) > 0 {
		logger.Info("Recreating ProvisionedInstances for preempted instances", "count", len(preemptedInstances))
		// The reconcileProvisionedInstances function will create new instances
		// to replace the terminated ones in the next reconciliation loop
	}

	return nil
}

// findJobsOnNode finds jobs running on a specific node
