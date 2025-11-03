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
	"bytes"
	"context"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
	compute "google.golang.org/api/compute/v1"
)

// NodeJoiner handles joining GCP instances to the Kubernetes cluster
type NodeJoiner struct {
	provisioner *Provisioner
	sshConfig   *ssh.ClientConfig
}

// NewNodeJoiner creates a new NodeJoiner
func NewNodeJoiner(provisioner *Provisioner, sshConfig *ssh.ClientConfig) *NodeJoiner {
	return &NodeJoiner{
		provisioner: provisioner,
		sshConfig:   sshConfig,
	}
}

// JoinNodeToCluster joins a node to the Kubernetes cluster
func (nj *NodeJoiner) JoinNodeToCluster(ctx context.Context, instance *compute.Instance, kubeadmJoinCommand string) error {
	// Get the external IP of the instance
	externalIP := ""
	if len(instance.NetworkInterfaces) > 0 && len(instance.NetworkInterfaces[0].AccessConfigs) > 0 {
		externalIP = instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
	}

	if externalIP == "" {
		return fmt.Errorf("instance has no external IP")
	}

	// Wait for SSH to be available
	if err := nj.waitForSSH(ctx, externalIP); err != nil {
		return fmt.Errorf("failed to wait for SSH: %w", err)
	}

	// Execute kubeadm join command
	if err := nj.executeSSHCommand(externalIP, kubeadmJoinCommand); err != nil {
		return fmt.Errorf("failed to execute kubeadm join: %w", err)
	}

	return nil
}

// waitForSSH waits for SSH to become available on the instance
func (nj *NodeJoiner) waitForSSH(ctx context.Context, host string) error {
	maxRetries := 30
	retryDelay := 10 * time.Second

	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", host), nj.sshConfig)
		if err == nil {
			client.Close()
			return nil
		}

		time.Sleep(retryDelay)
	}

	return fmt.Errorf("SSH connection timeout")
}

// executeSSHCommand executes a command via SSH
func (nj *NodeJoiner) executeSSHCommand(host, command string) error {
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:22", host), nj.sshConfig)
	if err != nil {
		return fmt.Errorf("failed to dial SSH: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Run(command); err != nil {
		return fmt.Errorf("failed to run command: %w, stderr: %s", err, stderr.String())
	}

	return nil
}

// WaitForNodeReady waits for a node to become ready in the cluster
func (nj *NodeJoiner) WaitForNodeReady(ctx context.Context, nodeName string, timeout time.Duration) error {
	// This would typically use the Kubernetes client to check node status
	// For now, we'll implement a placeholder
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for node to become ready")
		case <-ticker.C:
			// TODO: Implement actual node readiness check using Kubernetes client
			// For now, just wait for a fixed duration
			return nil
		}
	}
}

// GenerateKubeadmJoinCommand generates a kubeadm join command
func GenerateKubeadmJoinCommand(endpoint, token, caCertHash string) string {
	return fmt.Sprintf("sudo kubeadm join %s --token %s --discovery-token-ca-cert-hash %s",
		endpoint, token, caCertHash)
}
