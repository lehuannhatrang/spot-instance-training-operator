# Spot Instance Training Operator

A Kubernetes operator for managing training jobs on GCP spot instances with automatic checkpointing and recovery. This operator helps you run GPU/CPU training jobs on cost-effective preemptible instances while ensuring fault tolerance through automatic checkpointing.

## Features

- 🚀 Automatic provisioning of GCP spot instances with GPU support
- 💾 Scheduled checkpointing during training
- 🔄 Automatic recovery from preemption events
- 📊 Multi-replica job management for high availability
- 🔌 Integration with CheckpointBackup CRD for checkpoint management
- ⚙️ Flexible GPU and VM configuration

## Architecture

The operator manages two main custom resources:

1. **SpotInstanceVM**: Defines the VM specifications (GPU type, machine type, image, etc.)
2. **SpotInstanceJob**: Defines the training job with checkpoint configuration and job template

The controller automatically:
- Creates multiple job replicas (default: 2) for redundancy
- Provisions spot instances when needed
- Joins new nodes to the Kubernetes cluster
- Creates checkpoints at regular intervals
- Handles preemption events by creating immediate checkpoints and restarting jobs

## Prerequisites

- Kubernetes cluster (v1.24+)
- GCP service account with permissions to create/delete compute instances
- [CheckpointBackup CRD](https://raw.githubusercontent.com/lehuannhatrang/stateful-migration-operator/refs/heads/leehun/checkpointing-service/config/crd/bases/migration.dcnlab.com_checkpointbackups.yaml) installed
- Checkpoint agent running on nodes (from [stateful-migration-operator](https://github.com/lehuannhatrang/stateful-migration-operator))

## Installation

### Deploy the Operator

```bash
# Install CRDs
make install

# Deploy the operator
make deploy IMG=<your-registry>/spot-instance-training-operator:tag
```

### Create Required Secrets

1. **GCP Credentials Secret**:
```bash
kubectl create secret generic gcp-credentials \
  --from-file=credentials.json=/path/to/your/gcp-credentials.json \
  -n default
```

2. **Kubeadm Join Token** (if using dynamic node joining):
```bash
# Generate a token on the control plane
kubeadm token create --print-join-command

# Create secret with the token
kubectl create secret generic kubeadm-join-token \
  --from-literal=token=<token> \
  --from-literal=ca-cert-hash=<ca-cert-hash> \
  --from-literal=control-plane-endpoint=<endpoint> \
  -n default
```

## Usage

### 1. Define SpotInstanceVM

Create a `SpotInstanceVM` resource to define the VM specifications:

```yaml
apiVersion: training.training.dcnlab.com/v1alpha1
kind: SpotInstanceVM
metadata:
  name: my-training-vm
spec:
  vmImage: "projects/cos-cloud/global/images/cos-stable-109-17800-147-54"
  gpu:
    type: "nvidia-tesla-t4"
    count: 1
  gcp:
    project: "my-gcp-project"
    zone: "us-central1-a"
    machineType: "n1-standard-4"
    diskSizeGB: 100
```

### 2. Define SpotInstanceJob

Create a `SpotInstanceJob` resource to define your training job:

```yaml
apiVersion: training.training.dcnlab.com/v1alpha1
kind: SpotInstanceJob
metadata:
  name: my-training-job
spec:
  replicas: 2
  spotInstanceVMRef:
    name: my-training-vm
  gcpCredentialsSecretRef:
    name: gcp-credentials
    namespace: default
  checkpointConfig:
    checkpointImageRepo: "gcr.io/my-project/checkpoints"
    checkpointInterval: "30m"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
          - name: training
            image: "my-registry/training:latest"
            command: ["python", "train.py"]
            resources:
              requests:
                nvidia.com/gpu: 1
```

### 3. Apply Resources

```bash
kubectl apply -f spotinstancevm.yaml
kubectl apply -f spotinstancejob.yaml
```

## How It Works

### 1: Create a SpotInstanceJob (CR) , SpotInstanceVM (CR), GCP credentials (Secret) and apply to k8s

Example:

``` SpotInstanceJob
 ....
 // user need to define the checkpoint image repo here
 // user define checkpoint interval here
```
SpotInstanceJob should have job template.

```SpotInstanceVM
 ....
 // define the VM image
 // define the gpu model name
 // define number of gpus
 ....
```

User has to define the VM image in SpotInstanceVM 

```gcp-credential
 ....
```


### 2: (Initialize phase) SpotInstanceTraining Controller (Placed on Control Plane node) will watch SpotInstanceJob. This controller will create 2 replicas jobs for the same SpotInstanceJob. 
- It will check if nodes resources (GPU) are available to schedule jobs on.
- If there is enough resource, it will create jobs.
- If there is not enough resource, it will create a new Spot Instance VM ( from SpotInstanceVM spec ) , join that VM to cluster (it needs to get the kubeadm join command, then ssh to the new node and run kubeadm join command, it then need to wait for the pods ready on new spot instance and make sure gpu-operator works) , and then it schedule the jobs. [0]

### 3: Base on the checkpoint interval time of SpotInstanceJob, controller will create CheckpointBackup CR intervally to make CheckpointAgent create checkpoint of the pod. 

### 3: SpotInstanceTraining Controller also listen to the preemption notification from Google Compute Engine. If there is any preemption notification for a Spot Instance:
- Controller will detect which jobs is running on that node first, and found which SpotInstanceJob it belongs to.
- Controller then determine if the other replica of SpotInstanceJob is still alive. 
    - If it still alive: Controller create CheckpointBackup CR for the pods belongs to that job.
        The CheckpointBackup CR is available here: https://raw.githubusercontent.com/lehuannhatrang/stateful-migration-operator/refs/heads/leehun/checkpointing-service/config/crd/bases/migration.dcnlab.com_checkpointbackups.yaml
        - At the same time, controller will determine if there is enough resource require to backup a new job, it not it will create new spot instance [0].
        - Controller can know which is the checkpoint image name and when checkpoint process is done by CheckpointBackup CR. It will delete the preemtive job replica and create a new job replica with the checkpoint image name.
    - If it not alive: Controller will get checkpoint image repo from the SpotInstanceJob, check for the latest create tag and re-create job replicas from the checkpoint-image name.

## Development

### Building and Testing

```bash
# Build the operator
make build

# Run tests
make test

# Run the operator locally (for development)
make run
```

### Building Docker Image

```bash
# Build and push the Docker image
make docker-build docker-push IMG=<your-registry>/spot-instance-training-operator:tag
```

## Project Structure

```
spot-instance-training-operator/
├── api/v1alpha1/              # API definitions for CRDs
│   ├── spotinstancejob_types.go
│   └── spotinstancevm_types.go
├── internal/
│   ├── controller/            # Controller implementations
│   │   ├── spotinstancejob_controller.go
│   │   └── spotinstancevm_controller.go
│   ├── gcp/                   # GCP provisioning logic
│   │   ├── provisioner.go
│   │   └── node_joiner.go
│   └── checkpoint/            # Checkpoint management
│       └── manager.go
├── config/                    # Kubernetes manifests
│   ├── crd/                   # CRD definitions
│   ├── rbac/                  # RBAC configurations
│   ├── manager/               # Manager deployment
│   └── samples/               # Sample manifests
└── README.md
```

## API Reference

### SpotInstanceJob

| Field | Type | Description |
|-------|------|-------------|
| `replicas` | int32 | Number of job replicas (default: 2) |
| `jobTemplate` | JobTemplateSpec | Template for the Kubernetes Job |
| `checkpointConfig` | CheckpointConfig | Checkpoint configuration |
| `spotInstanceVMRef` | LocalObjectReference | Reference to SpotInstanceVM |
| `gcpCredentialsSecretRef` | SecretReference | Reference to GCP credentials secret |

### SpotInstanceVM

| Field | Type | Description |
|-------|------|-------------|
| `vmImage` | string | GCP VM image |
| `gpu` | GPUConfig | GPU configuration |
| `gcp` | GCPConfig | GCP-specific configuration |
| `startupScript` | string | Startup script for VM initialization |
| `kubeadmJoinConfig` | KubeadmJoinConfig | Configuration for joining Kubernetes cluster |

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

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

