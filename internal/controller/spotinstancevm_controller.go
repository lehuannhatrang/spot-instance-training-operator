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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

const (
	spotInstanceVMFinalizer = "training.dcnlab.com/vm-finalizer"
)

// SpotInstanceVMReconciler reconciles a SpotInstanceVM object
type SpotInstanceVMReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancevms,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancevms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=training.training.dcnlab.com,resources=spotinstancevms/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SpotInstanceVMReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling SpotInstanceVM", "name", req.Name, "namespace", req.Namespace)

	// Fetch the SpotInstanceVM instance
	spotInstanceVM := &trainingv1alpha1.SpotInstanceVM{}
	if err := r.Get(ctx, req.NamespacedName, spotInstanceVM); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("SpotInstanceVM resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get SpotInstanceVM")
		return ctrl.Result{}, err
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(spotInstanceVM, spotInstanceVMFinalizer) {
		controllerutil.AddFinalizer(spotInstanceVM, spotInstanceVMFinalizer)
		if err := r.Update(ctx, spotInstanceVM); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle deletion
	if !spotInstanceVM.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, spotInstanceVM)
	}

	// Initialize status if needed
	if spotInstanceVM.Status.Conditions == nil {
		spotInstanceVM.Status.Conditions = []metav1.Condition{}
	}
	if spotInstanceVM.Status.ProvisionedInstances == nil {
		spotInstanceVM.Status.ProvisionedInstances = []trainingv1alpha1.VMInstanceStatus{}
	}

	// Update status
	if err := r.Status().Update(ctx, spotInstanceVM); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleDeletion handles the deletion of SpotInstanceVM
func (r *SpotInstanceVMReconciler) handleDeletion(ctx context.Context, spotInstanceVM *trainingv1alpha1.SpotInstanceVM) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling deletion of SpotInstanceVM")

	// Perform cleanup operations here
	// For example, delete all provisioned GCP instances

	// Remove finalizer
	controllerutil.RemoveFinalizer(spotInstanceVM, spotInstanceVMFinalizer)
	if err := r.Update(ctx, spotInstanceVM); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SpotInstanceVMReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&trainingv1alpha1.SpotInstanceVM{}).
		Named("spotinstancevm").
		Complete(r)
}
