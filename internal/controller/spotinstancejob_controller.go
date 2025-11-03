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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
	"github.com/dcnlab/spot-instance-training-operator/internal/gcp"
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
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancevms,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancevms/status,verbs=get;update;patch
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

	// Check if we need to provision more resources
	needed := replicas - int32(len(jobList.Items))
	if needed > 0 {
		// Check if there are enough GPU resources
		hasResources, err := r.checkResourceAvailability(ctx, spotInstanceVM, needed)
		if err != nil {
			logger.Error(err, "Failed to check resource availability")
			return ctrl.Result{}, err
		}

		// If not enough resources, provision new spot instances
		if !hasResources {
			logger.Info("Not enough resources, provisioning spot instances", "needed", needed)
			if err := r.provisionSpotInstances(ctx, spotInstanceJob, spotInstanceVM, needed); err != nil {
				logger.Error(err, "Failed to provision spot instances")
				// If instances are being provisioned, wait longer for them to join
				return ctrl.Result{RequeueAfter: 60 * time.Second}, err
			}
			// Requeue to wait for nodes to be ready (2 minutes should be enough for boot + join)
			logger.Info("Waiting for provisioned instances to join cluster")
			return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
		}

		// Create missing jobs
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
func (r *SpotInstanceJobReconciler) checkResourceAvailability(ctx context.Context, spotInstanceVM *trainingv1alpha1.SpotInstanceVM, needed int32) (bool, error) {
	logger := log.FromContext(ctx)

	// List all nodes
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		logger.Error(err, "Failed to list nodes")
		return false, err
	}

	// Check for GPU availability
	if spotInstanceVM.Spec.GPU != nil {
		availableNodes := int32(0)
		requiredGPUs := spotInstanceVM.Spec.GPU.Count

		for _, node := range nodeList.Items {
			// Skip nodes that are not ready
			if !isNodeReady(&node) {
				continue
			}

			// Check if node has the required GPU
			gpuAllocatable, ok := node.Status.Allocatable["nvidia.com/gpu"]
			if !ok {
				continue
			}

			if gpuAllocatable.Cmp(resource.MustParse(fmt.Sprintf("%d", requiredGPUs))) >= 0 {
				availableNodes++
				if availableNodes >= needed {
					return true, nil
				}
			}
		}
		logger.Info("Not enough GPU nodes available", "available", availableNodes, "needed", needed)
		return false, nil
	}

	return true, nil
}

// isNodeReady checks if a node is ready
func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// provisionSpotInstances provisions new GCP spot instances
func (r *SpotInstanceJobReconciler) provisionSpotInstances(ctx context.Context, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, spotInstanceVM *trainingv1alpha1.SpotInstanceVM, count int32) error {
	logger := log.FromContext(ctx)

	// Get GCP credentials from secret
	credentialsSecret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      spotInstanceJob.Spec.GCPCredentialsSecretRef.Name,
		Namespace: spotInstanceJob.Spec.GCPCredentialsSecretRef.Namespace,
	}
	if err := r.Get(ctx, secretKey, credentialsSecret); err != nil {
		return fmt.Errorf("failed to get GCP credentials secret: %w", err)
	}

	credentialsJSON, ok := credentialsSecret.Data["credentials.json"]
	if !ok {
		return fmt.Errorf("credentials.json not found in secret")
	}

	// Create GCP provisioner
	provisioner, err := gcp.NewProvisioner(ctx, credentialsJSON, spotInstanceVM.Spec.GCP.Project)
	if err != nil {
		return fmt.Errorf("failed to create GCP provisioner: %w", err)
	}

	// Check how many instances are already provisioned or being provisioned
	existingCount, err := r.countExistingInstances(ctx, provisioner, spotInstanceJob, spotInstanceVM)
	if err != nil {
		logger.Error(err, "Failed to count existing instances, will proceed with provisioning")
	} else if existingCount >= count {
		logger.Info("Sufficient instances already exist or are being provisioned",
			"existing", existingCount, "needed", count)
		// Requeue to check if nodes are ready
		return fmt.Errorf("instances being provisioned, waiting for nodes to join")
	} else if existingCount > 0 {
		// Reduce count by already existing instances
		count = count - existingCount
		logger.Info("Reducing provision count due to existing instances",
			"existing", existingCount, "willProvision", count)
	}

	// Get kubeadm token from secret
	// Try to get from kubeadmJoinConfig first, otherwise look for standard secret name
	secretName := "kubeadm-join-token"
	if spotInstanceVM.Spec.KubeadmJoinConfig != nil && spotInstanceVM.Spec.KubeadmJoinConfig.TokenSecretRef != "" {
		secretName = spotInstanceVM.Spec.KubeadmJoinConfig.TokenSecretRef
	}

	kubeadmToken := ""
	caCertHash := ""
	controlPlaneEndpoint := ""

	tokenSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: spotInstanceVM.Namespace,
	}, tokenSecret); err != nil {
		logger.Info("kubeadm join token not found, nodes will not auto-join cluster", "secret", secretName)
	} else {
		kubeadmToken = string(tokenSecret.Data["token"])
		caCertHash = string(tokenSecret.Data["ca-cert-hash"])
		controlPlaneEndpoint = string(tokenSecret.Data["control-plane-endpoint"])
		logger.Info("Found kubeadm join credentials", "secret", secretName)
	}

	// Provision instances
	for i := int32(0); i < count; i++ {
		instanceName := fmt.Sprintf("%s-%s-%d-%d", spotInstanceJob.Name, spotInstanceVM.Name, i, time.Now().Unix())
		logger.Info("Provisioning spot instance", "name", instanceName)

		// Clone the VM spec and inject kubeadm join command if we have credentials
		vmSpec := spotInstanceVM.Spec.DeepCopy()
		if kubeadmToken != "" && controlPlaneEndpoint != "" && caCertHash != "" {
			joinCmd := gcp.GenerateKubeadmJoinCommand(
				controlPlaneEndpoint,
				kubeadmToken,
				caCertHash,
			)
			// Append join command to startup script
			vmSpec.StartupScript += fmt.Sprintf("\n\n# Auto-join Kubernetes cluster\n%s\n", joinCmd)
			logger.Info("Added kubeadm join to startup script", "endpoint", controlPlaneEndpoint)
		}

		instance, err := provisioner.ProvisionSpotInstance(ctx, vmSpec, instanceName)
		if err != nil {
			logger.Error(err, "Failed to provision spot instance", "name", instanceName)
			return err
		}

		logger.Info("Spot instance provisioned", "name", instanceName, "instanceID", instance.Id)

		// Update SpotInstanceVM status
		if err := r.updateVMStatus(ctx, spotInstanceVM, instance); err != nil {
			logger.Error(err, "Failed to update SpotInstanceVM status")
		}

		// Node will auto-join via startup script
		if kubeadmToken != "" {
			logger.Info("Node will auto-join cluster via startup script", "instance", instanceName)
		}
	}

	return nil
}

// updateVMStatus updates the SpotInstanceVM status with provisioned instance info
func (r *SpotInstanceJobReconciler) updateVMStatus(ctx context.Context, spotInstanceVM *trainingv1alpha1.SpotInstanceVM, instance interface{}) error {
	// Get the latest version of SpotInstanceVM
	latest := &trainingv1alpha1.SpotInstanceVM{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      spotInstanceVM.Name,
		Namespace: spotInstanceVM.Namespace,
	}, latest); err != nil {
		return err
	}

	// Add instance to status (simplified - would need proper GCP instance type handling)
	now := metav1.Now()
	vmStatus := trainingv1alpha1.VMInstanceStatus{
		Name:         fmt.Sprintf("instance-%d", time.Now().Unix()),
		State:        "PROVISIONING",
		CreationTime: &now,
	}

	latest.Status.ProvisionedInstances = append(latest.Status.ProvisionedInstances, vmStatus)

	return r.Status().Update(ctx, latest)
}

// countExistingInstances counts instances already provisioned for this job
func (r *SpotInstanceJobReconciler) countExistingInstances(ctx context.Context, provisioner *gcp.Provisioner, spotInstanceJob *trainingv1alpha1.SpotInstanceJob, spotInstanceVM *trainingv1alpha1.SpotInstanceVM) (int32, error) {
	logger := log.FromContext(ctx)

	// List instances from GCP that match our naming pattern
	// Instances are named: {job-name}-{vm-name}-{index}-{timestamp}
	prefix := fmt.Sprintf("%s-%s-", spotInstanceJob.Name, spotInstanceVM.Name)

	instances, err := provisioner.ListInstancesByPrefix(ctx, spotInstanceVM.Spec.GCP.Zone, prefix)
	if err != nil {
		return 0, fmt.Errorf("failed to list GCP instances: %w", err)
	}

	// Count instances that are RUNNING or PROVISIONING
	count := int32(0)
	for _, instance := range instances {
		status := instance.Status
		// Count instances that are not TERMINATED or STOPPED
		if status == "PROVISIONING" || status == "STAGING" || status == "RUNNING" {
			count++
			logger.Info("Found existing instance", "name", instance.Name, "status", status)
		}
	}

	return count, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SpotInstanceJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&trainingv1alpha1.SpotInstanceJob{}).
		Owns(&batchv1.Job{}).
		Named("spotinstancejob").
		Complete(r)
}
