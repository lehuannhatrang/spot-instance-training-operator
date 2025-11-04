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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	compute "google.golang.org/api/compute/v1"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
	"github.com/dcnlab/spot-instance-training-operator/internal/gcp"
)

const (
	provisionedInstanceFinalizer = "training.dcnlab.com/provisionedinstance-finalizer"
)

// ProvisionedInstanceReconciler reconciles a ProvisionedInstance object
type ProvisionedInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=provisionedinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=provisionedinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=provisionedinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=instancetemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ProvisionedInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling ProvisionedInstance", "name", req.Name, "namespace", req.Namespace)

	// Fetch the ProvisionedInstance
	provisionedInstance := &trainingv1alpha1.ProvisionedInstance{}
	if err := r.Get(ctx, req.NamespacedName, provisionedInstance); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !provisionedInstance.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, provisionedInstance)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(provisionedInstance, provisionedInstanceFinalizer) {
		controllerutil.AddFinalizer(provisionedInstance, provisionedInstanceFinalizer)
		if err := r.Update(ctx, provisionedInstance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize status if needed
	if provisionedInstance.Status.State == "" {
		provisionedInstance.Status.State = "PENDING"
		if err := r.Status().Update(ctx, provisionedInstance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Fetch InstanceTemplate
	instanceTemplate, err := r.fetchInstanceTemplate(ctx, provisionedInstance)
	if err != nil {
		logger.Error(err, "Failed to fetch InstanceTemplate")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Fetch GCP credentials
	gcpCredentials, err := r.fetchGCPCredentials(ctx, provisionedInstance.Spec.GCPCredentialsSecretRef)
	if err != nil {
		logger.Error(err, "Failed to fetch GCP credentials")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Create GCP provisioner
	provisioner, err := gcp.NewProvisioner(ctx, gcpCredentials, instanceTemplate.Spec.GCP.Project)
	if err != nil {
		logger.Error(err, "Failed to create GCP provisioner")
		return ctrl.Result{}, err
	}

	// If instance not created yet, provision it
	if provisionedInstance.Status.InstanceID == "" && provisionedInstance.Status.State == "PENDING" {
		return r.provisionInstance(ctx, provisionedInstance, instanceTemplate, provisioner)
	}

	// Check instance status and update
	return r.checkAndUpdateInstanceStatus(ctx, provisionedInstance, instanceTemplate, provisioner)
}

// fetchInstanceTemplate fetches the InstanceTemplate referenced by the ProvisionedInstance
func (r *ProvisionedInstanceReconciler) fetchInstanceTemplate(ctx context.Context, provisionedInstance *trainingv1alpha1.ProvisionedInstance) (*trainingv1alpha1.InstanceTemplate, error) {
	namespace := provisionedInstance.Spec.InstanceTemplateNamespace
	if namespace == "" {
		namespace = provisionedInstance.Namespace
	}

	instanceTemplate := &trainingv1alpha1.InstanceTemplate{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      provisionedInstance.Spec.InstanceTemplateName,
		Namespace: namespace,
	}, instanceTemplate); err != nil {
		return nil, err
	}

	return instanceTemplate, nil
}

// fetchGCPCredentials fetches GCP credentials from a secret
func (r *ProvisionedInstanceReconciler) fetchGCPCredentials(ctx context.Context, secretRef corev1.SecretReference) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      secretRef.Name,
		Namespace: secretRef.Namespace,
	}, secret); err != nil {
		return nil, err
	}

	credJSON, ok := secret.Data["credentials.json"]
	if !ok {
		return nil, fmt.Errorf("credentials.json not found in secret %s/%s", secretRef.Namespace, secretRef.Name)
	}

	return credJSON, nil
}

// provisionInstance provisions a new GCP instance
func (r *ProvisionedInstanceReconciler) provisionInstance(ctx context.Context, provisionedInstance *trainingv1alpha1.ProvisionedInstance, instanceTemplate *trainingv1alpha1.InstanceTemplate, provisioner *gcp.Provisioner) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if instance already exists in GCP
	existingInstances, err := provisioner.ListInstancesByPrefix(ctx, instanceTemplate.Spec.GCP.Zone, provisionedInstance.Name)
	if err != nil {
		logger.Error(err, "Failed to list existing instances")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	if len(existingInstances) > 0 {
		// Instance already exists, update status
		logger.Info("Instance already exists in GCP", "instance", provisionedInstance.Name)
		instance := existingInstances[0]
		return r.updateInstanceStatus(ctx, provisionedInstance, instance)
	}

	// Fetch kubeadm join token
	joinToken, caCertHash, controlPlaneEndpoint, err := r.fetchKubeadmJoinInfo(ctx, provisionedInstance.Namespace)
	if err != nil {
		logger.Error(err, "Failed to fetch kubeadm join info")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Create a copy of the template spec to modify the startup script
	templateSpec := instanceTemplate.Spec.DeepCopy()

	// Append kubeadm join command to startup script
	joinCommand := fmt.Sprintf("kubeadm join %s --token %s --discovery-token-ca-cert-hash %s", controlPlaneEndpoint, joinToken, caCertHash)
	if templateSpec.StartupScript != "" {
		templateSpec.StartupScript = templateSpec.StartupScript + "\n" + joinCommand
	} else {
		templateSpec.StartupScript = "#!/bin/bash\n" + joinCommand
	}

	// Determine if preemptible
	preemptible := true
	if templateSpec.Preemptible != nil {
		preemptible = *templateSpec.Preemptible
	}

	logger.Info("Provisioning new instance", "name", provisionedInstance.Name, "preemptible", preemptible)

	// Provision the instance
	instance, err := provisioner.ProvisionSpotInstance(ctx, templateSpec, provisionedInstance.Name, preemptible)
	if err != nil {
		logger.Error(err, "Failed to provision instance")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	logger.Info("Instance provisioned successfully", "instance", instance.Name, "id", fmt.Sprintf("%d", instance.Id))

	// Update status
	provisionedInstance.Status.InstanceID = fmt.Sprintf("%d", instance.Id)
	provisionedInstance.Status.InstanceName = instance.Name
	provisionedInstance.Status.State = "PROVISIONING"
	provisionedInstance.Status.CreationTime = &metav1.Time{Time: time.Now()}

	if err := r.Status().Update(ctx, provisionedInstance); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to check status
	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

// fetchKubeadmJoinInfo fetches kubeadm join information from secret
func (r *ProvisionedInstanceReconciler) fetchKubeadmJoinInfo(ctx context.Context, namespace string) (token, caCertHash, controlPlaneEndpoint string, err error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      "kubeadm-join-token",
		Namespace: namespace,
	}, secret); err != nil {
		return "", "", "", err
	}

	token = string(secret.Data["token"])
	caCertHash = string(secret.Data["ca-cert-hash"])
	controlPlaneEndpoint = string(secret.Data["control-plane-endpoint"])

	if token == "" || caCertHash == "" || controlPlaneEndpoint == "" {
		return "", "", "", fmt.Errorf("kubeadm join info incomplete in secret")
	}

	return token, caCertHash, controlPlaneEndpoint, nil
}

// checkAndUpdateInstanceStatus checks GCP instance status and updates CR
func (r *ProvisionedInstanceReconciler) checkAndUpdateInstanceStatus(ctx context.Context, provisionedInstance *trainingv1alpha1.ProvisionedInstance, instanceTemplate *trainingv1alpha1.InstanceTemplate, provisioner *gcp.Provisioner) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get instance from GCP
	zone := instanceTemplate.Spec.GCP.Zone

	instance, err := provisioner.GetInstance(ctx, zone, provisionedInstance.Status.InstanceName)
	if err != nil {
		// Check if it's a 404 (instance not found)
		if strings.Contains(err.Error(), "notFound") || strings.Contains(err.Error(), "404") {
			logger.Info("Instance not found in GCP, marking as TERMINATED")
			provisionedInstance.Status.State = "TERMINATED"
			provisionedInstance.Status.PreemptionNotice = true

			// Delete the node if it exists
			if provisionedInstance.Status.NodeName != "" {
				if err := r.deleteNode(ctx, provisionedInstance.Status.NodeName); err != nil {
					logger.Error(err, "Failed to delete node", "node", provisionedInstance.Status.NodeName)
					// Continue anyway
				}
			}

			if err := r.Status().Update(ctx, provisionedInstance); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get instance from GCP")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Instance not found in GCP
	if instance == nil {
		logger.Info("Instance not found in GCP, marking as TERMINATED")
		provisionedInstance.Status.State = "TERMINATED"
		provisionedInstance.Status.PreemptionNotice = true

		// Delete the node if it exists
		if provisionedInstance.Status.NodeName != "" {
			if err := r.deleteNode(ctx, provisionedInstance.Status.NodeName); err != nil {
				logger.Error(err, "Failed to delete node", "node", provisionedInstance.Status.NodeName)
				// Continue anyway
			}
		}

		if err := r.Status().Update(ctx, provisionedInstance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Update status from GCP
	return r.updateInstanceStatus(ctx, provisionedInstance, instance)
}

// updateInstanceStatus updates the ProvisionedInstance status from GCP instance
func (r *ProvisionedInstanceReconciler) updateInstanceStatus(ctx context.Context, provisionedInstance *trainingv1alpha1.ProvisionedInstance, instance *compute.Instance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Update basic info
	provisionedInstance.Status.InstanceID = fmt.Sprintf("%d", instance.Id)
	provisionedInstance.Status.InstanceName = instance.Name
	provisionedInstance.Status.State = instance.Status

	// Update IPs
	if len(instance.NetworkInterfaces) > 0 {
		provisionedInstance.Status.InternalIP = instance.NetworkInterfaces[0].NetworkIP
		if len(instance.NetworkInterfaces[0].AccessConfigs) > 0 {
			provisionedInstance.Status.ExternalIP = instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
		}
	}

	// Check if node joined cluster
	if instance.Status == "RUNNING" && !provisionedInstance.Status.JoinedCluster {
		node := &corev1.Node{}
		err := r.Get(ctx, types.NamespacedName{Name: instance.Name}, node)
		if err == nil {
			provisionedInstance.Status.JoinedCluster = true
			provisionedInstance.Status.NodeName = node.Name
			logger.Info("Node joined cluster", "node", node.Name)
		}
	}

	// Check for preemption notice
	if instance.Status == "RUNNING" {
		// Check instance metadata or scheduling for preemption
		if instance.Scheduling != nil && instance.Scheduling.Preemptible {
			// For spot instances, we can check status
			// STOPPING or TERMINATED means preemption happened
		}
	}

	// Detect preemption/termination
	if instance.Status == "STOPPING" || instance.Status == "TERMINATED" {
		provisionedInstance.Status.PreemptionNotice = true
		provisionedInstance.Status.State = instance.Status
		logger.Info("Instance is being preempted/terminated", "instance", instance.Name, "status", instance.Status)
	}

	if err := r.Status().Update(ctx, provisionedInstance); err != nil {
		return ctrl.Result{}, err
	}

	// If instance is terminated, delete the node and no need to requeue
	if instance.Status == "TERMINATED" {
		logger.Info("Instance terminated", "instance", instance.Name)
		// Delete the node if it exists
		if provisionedInstance.Status.NodeName != "" {
			if err := r.deleteNode(ctx, provisionedInstance.Status.NodeName); err != nil {
				logger.Error(err, "Failed to delete node", "node", provisionedInstance.Status.NodeName)
				// Continue anyway
			}
		}
		return ctrl.Result{}, nil
	}

	// Requeue to check status again
	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

// handleDeletion handles the deletion of a ProvisionedInstance
func (r *ProvisionedInstanceReconciler) handleDeletion(ctx context.Context, provisionedInstance *trainingv1alpha1.ProvisionedInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling ProvisionedInstance deletion", "name", provisionedInstance.Name)

	if controllerutil.ContainsFinalizer(provisionedInstance, provisionedInstanceFinalizer) {
		// Delete the VM instance from GCP
		if provisionedInstance.Status.InstanceName != "" {
			if err := r.deleteGCPInstance(ctx, provisionedInstance); err != nil {
				logger.Error(err, "Failed to delete GCP instance")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, err
			}
		}

		// Delete the Kubernetes node
		if provisionedInstance.Status.NodeName != "" {
			if err := r.deleteNode(ctx, provisionedInstance.Status.NodeName); err != nil {
				logger.Error(err, "Failed to delete node")
				// Continue anyway
			}
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(provisionedInstance, provisionedInstanceFinalizer)
		if err := r.Update(ctx, provisionedInstance); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// deleteGCPInstance deletes the GCP instance
func (r *ProvisionedInstanceReconciler) deleteGCPInstance(ctx context.Context, provisionedInstance *trainingv1alpha1.ProvisionedInstance) error {
	logger := log.FromContext(ctx)

	// Fetch InstanceTemplate to get project and zone
	instanceTemplate, err := r.fetchInstanceTemplate(ctx, provisionedInstance)
	if err != nil {
		return err
	}

	// Fetch GCP credentials
	gcpCredentials, err := r.fetchGCPCredentials(ctx, provisionedInstance.Spec.GCPCredentialsSecretRef)
	if err != nil {
		return err
	}

	// Create provisioner
	provisioner, err := gcp.NewProvisioner(ctx, gcpCredentials, instanceTemplate.Spec.GCP.Project)
	if err != nil {
		return err
	}

	zone := instanceTemplate.Spec.GCP.Zone
	logger.Info("Deleting GCP instance", "instance", provisionedInstance.Status.InstanceName, "zone", zone)

	if err := provisioner.DeleteInstance(ctx, instanceTemplate.Spec.GCP.Project, zone, provisionedInstance.Status.InstanceName); err != nil {
		return err
	}

	logger.Info("GCP instance deleted successfully", "instance", provisionedInstance.Status.InstanceName)
	return nil
}

// deleteNode deletes a Kubernetes node
func (r *ProvisionedInstanceReconciler) deleteNode(ctx context.Context, nodeName string) error {
	logger := log.FromContext(ctx)

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	logger.Info("Deleting node from cluster", "node", nodeName)
	if err := r.Delete(ctx, node); err != nil {
		return err
	}

	logger.Info("Node deleted successfully", "node", nodeName)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProvisionedInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&trainingv1alpha1.ProvisionedInstance{}).
		Named("provisionedinstance").
		Complete(r)
}
