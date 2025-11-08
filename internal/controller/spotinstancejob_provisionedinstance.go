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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

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

	// Track which replica indices already have ACTIVE ProvisionedInstances
	// Don't count terminated/preempted instances so their replica index can be reused
	replicaIndicesWithInstances := make(map[int32]bool)
	for _, pi := range provisionedInstanceList.Items {
		// Skip terminated/preempted instances - their replica index should be reused
		if pi.Status.PreemptionNotice || pi.Status.State == "TERMINATED" || pi.Status.State == "STOPPING" {
			continue
		}
		// Extract replica index from labels if available
		if replicaIndexStr, ok := pi.Labels[jobReplicaLabel]; ok {
			if idx, err := strconv.ParseInt(replicaIndexStr, 10, 32); err == nil {
				replicaIndicesWithInstances[int32(idx)] = true
			}
		}
	}

	// Create missing ProvisionedInstances
	if int32(activeInstances) < desiredReplicas {
		needed := desiredReplicas - int32(activeInstances)

		// Find the first replica index that doesn't have an instance
		replicaIndex := int32(0)
		created := int32(0)
		for created < needed {
			// Find next replica index without an instance
			for replicaIndicesWithInstances[replicaIndex] {
				replicaIndex++
				if replicaIndex >= desiredReplicas*2 {
					break
				}
			}

			instanceName := fmt.Sprintf("%s-%s-%d-%d",
				spotInstanceJob.Name,
				instanceTemplate.Name,
				replicaIndex,
				time.Now().Unix())

			provisionedInstance := &trainingv1alpha1.ProvisionedInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      instanceName,
					Namespace: spotInstanceJob.Namespace,
					Labels: map[string]string{
						spotInstanceJobLabel: spotInstanceJob.Name,
						jobReplicaLabel:      fmt.Sprintf("%d", replicaIndex),
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
				logger.Error(err, "Failed to create ProvisionedInstance", "name", instanceName, "replicaIndex", replicaIndex)
				replicaIndex++
				continue
			}

			logger.Info("Created ProvisionedInstance", "name", instanceName, "replicaIndex", replicaIndex)
			replicaIndicesWithInstances[replicaIndex] = true
			replicaIndex++
			created++
		}
	}

	return nil
}
