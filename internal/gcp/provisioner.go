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

package gcp

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

// Provisioner handles GCP spot instance provisioning
type Provisioner struct {
	computeService *compute.Service
	project        string
}

// NewProvisioner creates a new GCP provisioner
func NewProvisioner(ctx context.Context, credentialsJSON []byte, project string) (*Provisioner, error) {
	creds, err := google.CredentialsFromJSON(ctx, credentialsJSON, compute.ComputeScope)
	if err != nil {
		return nil, fmt.Errorf("failed to create credentials: %w", err)
	}

	computeService, err := compute.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("failed to create compute service: %w", err)
	}

	return &Provisioner{
		computeService: computeService,
		project:        project,
	}, nil
}

// ProvisionSpotInstance creates a new GCP spot instance
func (p *Provisioner) ProvisionSpotInstance(ctx context.Context, vmSpec *trainingv1alpha1.SpotInstanceVMSpec, instanceName string) (*compute.Instance, error) {
	zone := vmSpec.GCP.Zone

	// Prepare the instance configuration
	preemptible := true
	automaticRestart := false
	instance := &compute.Instance{
		Name:        instanceName,
		MachineType: fmt.Sprintf("zones/%s/machineTypes/%s", zone, vmSpec.GCP.MachineType),
		Scheduling: &compute.Scheduling{
			Preemptible:       preemptible,
			AutomaticRestart:  &automaticRestart,
			OnHostMaintenance: "TERMINATE",
		},
		Disks: []*compute.AttachedDisk{
			{
				Boot:       true,
				AutoDelete: true,
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: vmSpec.VMImage,
					DiskSizeGb:  int64(vmSpec.GCP.DiskSizeGB),
					DiskType:    fmt.Sprintf("zones/%s/diskTypes/%s", zone, vmSpec.GCP.DiskType),
				},
			},
		},
		NetworkInterfaces: []*compute.NetworkInterface{
			{
				AccessConfigs: []*compute.AccessConfig{
					{
						Name: "External NAT",
						Type: "ONE_TO_ONE_NAT",
					},
				},
			},
		},
	}

	// Add GPU if specified
	if vmSpec.GPU != nil {
		instance.GuestAccelerators = []*compute.AcceleratorConfig{
			{
				AcceleratorCount: int64(vmSpec.GPU.Count),
				AcceleratorType:  fmt.Sprintf("zones/%s/acceleratorTypes/%s", zone, vmSpec.GPU.Type),
			},
		}
		// GPU instances need to allow GPU passthrough
		instance.Scheduling.OnHostMaintenance = "TERMINATE"
	}

	// Add startup script if provided
	startupScript := vmSpec.StartupScript

	// If kubeadm join config is provided, append join command to startup script
	if vmSpec.KubeadmJoinConfig != nil &&
		vmSpec.KubeadmJoinConfig.ControlPlaneEndpoint != "" &&
		vmSpec.KubeadmJoinConfig.CACertHash != "" {
		// Note: Token should be retrieved from secret, but for startup script we need it here
		// This is a simplified version - in production, you'd handle token more securely
		joinCmd := "\n\n# Join Kubernetes cluster\n"
		joinCmd += "while ! systemctl is-active --quiet kubelet; do\n"
		joinCmd += "  echo 'Waiting for kubelet...'\n"
		joinCmd += "  sleep 5\n"
		joinCmd += "done\n"
		joinCmd += "echo 'Joining cluster...'\n"
		// The actual join will be handled by SSH after provisioning
		startupScript += joinCmd
	}

	if startupScript != "" {
		instance.Metadata = &compute.Metadata{
			Items: []*compute.MetadataItems{
				{
					Key:   "startup-script",
					Value: &startupScript,
				},
			},
		}
	}

	// Add network tags
	if len(vmSpec.GCP.NetworkTags) > 0 {
		instance.Tags = &compute.Tags{
			Items: vmSpec.GCP.NetworkTags,
		}
	}

	// Add service account
	if vmSpec.GCP.ServiceAccount != "" {
		instance.ServiceAccounts = []*compute.ServiceAccount{
			{
				Email: vmSpec.GCP.ServiceAccount,
				Scopes: []string{
					compute.CloudPlatformScope,
				},
			},
		}
	}

	// Insert the instance
	op, err := p.computeService.Instances.Insert(p.project, zone, instance).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to insert instance: %w", err)
	}

	// Wait for the operation to complete
	if err := p.waitForOperation(ctx, op, zone); err != nil {
		return nil, fmt.Errorf("failed to wait for operation: %w", err)
	}

	// Get the instance details
	createdInstance, err := p.computeService.Instances.Get(p.project, zone, instanceName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}

	return createdInstance, nil
}

// DeleteSpotInstance deletes a GCP spot instance
func (p *Provisioner) DeleteSpotInstance(ctx context.Context, zone, instanceName string) error {
	op, err := p.computeService.Instances.Delete(p.project, zone, instanceName).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}

	return p.waitForOperation(ctx, op, zone)
}

// GetInstance retrieves instance information
func (p *Provisioner) GetInstance(ctx context.Context, zone, instanceName string) (*compute.Instance, error) {
	instance, err := p.computeService.Instances.Get(p.project, zone, instanceName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}
	return instance, nil
}

// waitForOperation waits for a GCP operation to complete
func (p *Provisioner) waitForOperation(ctx context.Context, op *compute.Operation, zone string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			result, err := p.computeService.ZoneOperations.Get(p.project, zone, op.Name).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("failed to get operation status: %w", err)
			}

			if result.Status == "DONE" {
				if result.Error != nil {
					return fmt.Errorf("operation failed: %v", result.Error)
				}
				return nil
			}
		}
	}
}

// CheckPreemption checks if an instance has received a preemption notice
func (p *Provisioner) CheckPreemption(ctx context.Context, zone, instanceName string) (bool, error) {
	instance, err := p.GetInstance(ctx, zone, instanceName)
	if err != nil {
		return false, err
	}

	// Check if the instance is being preempted
	// In GCP, preempted instances typically move to STOPPING or TERMINATED state
	if instance.Status == "STOPPING" || instance.Status == "TERMINATED" {
		// Check if it was preempted (not stopped by user)
		if instance.Scheduling != nil && instance.Scheduling.Preemptible {
			return true, nil
		}
	}

	return false, nil
}
