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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	// Reconcile Job replicas
	if err := r.reconcileJobReplicas(ctx, spotInstanceJob); err != nil {
		logger.Error(err, "Failed to reconcile job replicas")
		return ctrl.Result{}, err
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
	hasPendingCheckpoints, err := r.hasPendingCheckpoints(ctx, spotInstanceJob)
	if err != nil {
		logger.Error(err, "Failed to check pending checkpoints")
		// Continue with reconciliation even if check failed
	} else if hasPendingCheckpoints {
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

	// Update status
	if err := r.updateStatus(ctx, spotInstanceJob); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	// Requeue more frequently if waiting for checkpoint completion
	if hasPendingCheckpoints {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

// fetchInstanceTemplate fetches the InstanceTemplate referenced by the SpotInstanceJob
func (r *SpotInstanceJobReconciler) fetchInstanceTemplate(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) (*trainingv1alpha1.InstanceTemplate, error) {
	instanceTemplate := &trainingv1alpha1.InstanceTemplate{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      spotInstanceJob.Spec.InstanceTemplateRef.Name,
		Namespace: spotInstanceJob.Namespace,
	}, instanceTemplate); err != nil {
		return nil, err
	}
	return instanceTemplate, nil
}

// reconcileJobReplicas ensures the desired number of job replicas are running
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

	activeJobs := 0
	for _, job := range jobList.Items {
		if job.Status.Active > 0 || job.Status.Succeeded > 0 {
			activeJobs++
		}
	}

	// Create missing job replicas
	if int32(activeJobs) < desiredReplicas {
		needed := desiredReplicas - int32(activeJobs)
		for i := int32(0); i < needed; i++ {
			jobName := fmt.Sprintf("%s-replica-%d-%d", spotInstanceJob.Name, i, time.Now().Unix())
			if err := r.createJobReplica(ctx, spotInstanceJob, jobName, i); err != nil {
				logger.Error(err, "Failed to create job replica", "jobName", jobName)
				continue
			}
			logger.Info("Created job replica", "jobName", jobName)
		}
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
func (r *SpotInstanceJobReconciler) reconcileProvisionedInstances(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, instanceTemplate *trainingv1alpha1.InstanceTemplate) error {
	logger := log.FromContext(ctx)

	desiredReplicas := int32(2)
	if spotInstanceJob.Spec.Replicas != nil {
		desiredReplicas = *spotInstanceJob.Spec.Replicas
	}

	// List existing ProvisionedInstances
	provisionedInstanceList := &trainingv1alpha1.ProvisionedInstanceList{}
	if err := r.List(ctx, provisionedInstanceList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
	}); err != nil {
		return err
	}

	// Count active instances (not terminated and not being deleted)
	activeInstances := 0
	hasTerminatedInstances := false
	for _, pi := range provisionedInstanceList.Items {
		// Count instances that are provisioning or running, not terminated or being deleted
		if (pi.Status.State == "" || pi.Status.State == "PENDING" || pi.Status.State == "PROVISIONING" || pi.Status.State == "RUNNING") && pi.DeletionTimestamp.IsZero() {
			activeInstances++
		}
		if pi.Status.State == "TERMINATED" && pi.Status.PreemptionNotice {
			hasTerminatedInstances = true
		}
	}

	// If there are terminated instances, check if checkpoint is in progress before creating new instances
	if hasTerminatedInstances && int32(activeInstances) < desiredReplicas {
		// Check if there are pending checkpoints (wait for checkpoint completion before provisioning)
		hasPendingCheckpoints, err := r.hasPendingCheckpoints(ctx, spotInstanceJob)
		if err != nil {
			logger.Error(err, "Failed to check pending checkpoints")
		} else if hasPendingCheckpoints {
			logger.Info("Pending checkpoints detected, will wait before creating new ProvisionedInstances")
			// Return nil - the main Reconcile will handle requeuing
			return nil
		}
	}

	// Create missing ProvisionedInstances
	if int32(activeInstances) < desiredReplicas {
		needed := desiredReplicas - int32(activeInstances)

		for i := int32(0); i < needed; i++ {
			instanceName := fmt.Sprintf("%s-%s-%d-%d",
				spotInstanceJob.Name,
				instanceTemplate.Name,
				i,
				time.Now().Unix())

			provisionedInstance := &trainingv1alpha1.ProvisionedInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: spotInstanceJob.Namespace,
					Labels: map[string]string{
						spotInstanceJobLabel: spotInstanceJob.Name,
					},
				},
				Spec: trainingv1alpha1.ProvisionedInstanceSpec{
					InstanceTemplateName:      instanceTemplate.Name,
					InstanceTemplateNamespace: instanceTemplate.Namespace,
					GCPCredentialsSecretRef:   spotInstanceJob.Spec.GCPCredentialsSecretRef,
				},
			}

			// Set owner reference
			if err := controllerutil.SetControllerReference(spotInstanceJob, provisionedInstance, r.Scheme); err != nil {
				return err
			}

			if err := r.Create(ctx, provisionedInstance); err != nil {
				logger.Error(err, "Failed to create ProvisionedInstance", "name", instanceName)
				continue
			}

			logger.Info("Created ProvisionedInstance", "name", instanceName)
		}
	}

	return nil
}

// hasPendingCheckpoints checks if there are any pending or in-progress checkpoints for the SpotInstanceJob
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

// hasRestoredJobs checks if there are any jobs with "restored-from" label, indicating they were recreated from a checkpoint
func (r *SpotInstanceJobReconciler) hasRestoredJobs(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob) (bool, error) {
	// List all jobs belonging to this SpotInstanceJob
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(spotInstanceJob.Namespace), client.MatchingLabels{
		spotInstanceJobLabel: spotInstanceJob.Name,
	}); err != nil {
		return false, err
	}

	// Check if any job has "restored-from" label
	for _, job := range jobList.Items {
		if _, ok := job.Labels["restored-from"]; ok {
			return true, nil
		}
	}

	return false, nil
}

// handlePreemption checks for preempted instances and handles them appropriately
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

	// Check for preempted instances
	for _, pi := range provisionedInstanceList.Items {
		if pi.Status.PreemptionNotice || pi.Status.State == "TERMINATED" || pi.Status.State == "STOPPING" {
			logger.Info("Detected preempted/terminated instance", "instance", pi.Name)
			preemptedInstances = append(preemptedInstances, pi.Name)

			// Count other active replicas
			activeReplicas := 0
			for _, otherPi := range provisionedInstanceList.Items {
				if otherPi.Name != pi.Name && otherPi.Status.State == "RUNNING" && !otherPi.Status.PreemptionNotice {
					activeReplicas++
				}
			}

			if activeReplicas > 0 {
				// Other replicas alive, check if we've already handled this preemption
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
		// Check if there's already an in-progress checkpoint
		for _, cp := range checkpoints.Items {
			status, found, _ := unstructured.NestedMap(cp.Object, "status")
			if found {
				phase, found, _ := unstructured.NestedString(status, "phase")
				if found {
					// Check if checkpoint is in progress (not in terminal state)
					// Terminal states: PhaseCompleted, PhaseCompletedPodDeleted, PhaseCompletedWithError, PhaseFailed
					// In-progress states: PhaseCheckpointing, PhaseCheckpointed, PhaseImageBuilding, PhaseImageBuilt, PhaseImagePushing, PhaseImagePushed
					if phase != "Completed" && phase != "CompletedPodDeleted" && phase != "CompletedWithError" && phase != "Failed" {
						logger.Info("CheckpointBackup already exists for job and is in progress", "job", jobName, "checkpoint", cp.GetName(), "phase", phase)
						return nil
					}
				} else {
					// Phase not set yet, assume it's pending/in-progress
					logger.Info("CheckpointBackup already exists for job (phase not set yet)", "job", jobName, "checkpoint", cp.GetName())
					return nil
				}
			} else {
				// Status not set yet, assume it's pending/in-progress
				logger.Info("CheckpointBackup already exists for job (no status yet)", "job", jobName, "checkpoint", cp.GetName())
				return nil
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
		checkpointImage := fmt.Sprintf("%s:%s-%d", imageRepo, container.Name, timestamp)
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

	// If immediate checkpoint, wait for completion and recreate job
	if immediate {
		go r.waitForCheckpointAndRecreateJob(context.Background(), spotInstanceJob, jobName)
	}

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
					containers, found, _ := unstructured.NestedSlice(status, "containers")
					if found && len(containers) > 0 {
						foundContainers = make([]map[string]interface{}, len(containers))
						for i, c := range containers {
							if containerMap, ok := c.(map[string]interface{}); ok {
								foundContainers[i] = containerMap
							}
						}
						break
					}
				}
			}

			if len(foundContainers) > 0 {
				checkpointContainers = foundContainers
				logger.Info("Checkpoint completed", "job", oldJobName, "containers", len(checkpointContainers))

				// Delete old job
				if err := r.deleteJob(ctx, spotInstanceJob.Namespace, oldJobName); err != nil {
					logger.Error(err, "Failed to delete old job", "job", oldJobName)
				}

				// Recreate job with checkpoint images
				if err := r.recreateJobWithCheckpoint(ctx, spotInstanceJob, oldJobName, checkpointContainers); err != nil {
					logger.Error(err, "Failed to recreate job", "job", oldJobName)
				}
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
		containers, found, _ := unstructured.NestedSlice(status, "containers")
		if found && len(containers) > 0 {
			if latestContainers == nil || completionTime.After(latestTime) {
				latestContainers = make([]map[string]interface{}, len(containers))
				for i, c := range containers {
					if containerMap, ok := c.(map[string]interface{}); ok {
						latestContainers[i] = containerMap
					}
				}
				latestTime = completionTime
			}
		}
	}

	if len(latestContainers) == 0 {
		logger.Info("No completed checkpoint found, recreating without checkpoint", "job", oldJobName)
		// Recreate without checkpoint
		if err := r.recreateJobWithCheckpoint(ctx, spotInstanceJob, oldJobName, nil); err != nil {
			logger.Error(err, "Failed to recreate job", "job", oldJobName)
		}
		return
	}

	logger.Info("Found latest checkpoint", "job", oldJobName, "containers", len(latestContainers))

	// Delete old job
	if err := r.deleteJob(ctx, spotInstanceJob.Namespace, oldJobName); err != nil {
		logger.Error(err, "Failed to delete old job", "job", oldJobName)
	}

	// Recreate job with checkpoint
	if err := r.recreateJobWithCheckpoint(ctx, spotInstanceJob, oldJobName, latestContainers); err != nil {
		logger.Error(err, "Failed to recreate job", "job", oldJobName)
	}
}

// deleteJob deletes a job
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

// updateStatus updates the SpotInstanceJob status
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
func (r *SpotInstanceJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&trainingv1alpha1.SpotInstanceJob{}).
		Owns(&batchv1.Job{}).
		Owns(&trainingv1alpha1.ProvisionedInstance{}).
		Named("spotinstancejob").
		Complete(r)
}
