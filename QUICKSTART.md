# Quick Start Guide

This guide will walk you through deploying the Spot Instance Training Operator and running your first training job on GCP spot instances.

## Prerequisites

Before you begin, ensure you have:

- A Kubernetes cluster (v1.24+)
- `kubectl` configured to access your cluster
- A GCP project with the Compute Engine API enabled
- A GCP service account with permissions to create/delete instances
- The CheckpointBackup CRD installed ([installation link](https://raw.githubusercontent.com/lehuannhatrang/stateful-migration-operator/refs/heads/leehun/checkpointing-service/config/crd/bases/migration.dcnlab.com_checkpointbackups.yaml))

## Step 1: Install the Operator

### Install CRDs

```bash
cd spot-instance-training-operator
make install
```

### Deploy the Operator

```bash
# Build and push the image (replace with your registry)
make docker-build docker-push IMG=gcr.io/your-project/spot-instance-training-operator:v1.0.0

# Deploy to cluster
make deploy IMG=gcr.io/your-project/spot-instance-training-operator:v1.0.0
```

Verify the operator is running:

```bash
kubectl get pods -n spot-instance-training-operator-system
```

## Step 2: Create GCP Credentials Secret

Create a secret containing your GCP service account credentials:

```bash
kubectl create secret generic gcp-credentials \
  --from-file=credentials.json=/path/to/your/gcp-service-account-key.json \
  -n default
```

## Step 3: Create a SpotInstanceVM Resource

Create a file `my-training-vm.yaml`:

```yaml
apiVersion: training.training.dcnlab.com/v1alpha1
kind: SpotInstanceVM
metadata:
  name: my-training-vm
  namespace: default
spec:
  vmImage: "projects/cos-cloud/global/images/cos-stable-109-17800-147-54"
  
  gpu:
    type: "nvidia-tesla-t4"
    count: 1
  
  gcp:
    project: "your-gcp-project-id"
    zone: "us-central1-a"
    machineType: "n1-standard-4"
    diskSizeGB: 100
    diskType: "pd-standard"
```

Apply the resource:

```bash
kubectl apply -f my-training-vm.yaml
```

## Step 4: Create a SpotInstanceJob Resource

Create a file `my-training-job.yaml`:

```yaml
apiVersion: training.training.dcnlab.com/v1alpha1
kind: SpotInstanceJob
metadata:
  name: my-training-job
  namespace: default
spec:
  replicas: 2  # Number of redundant replicas
  
  spotInstanceVMRef:
    name: my-training-vm
  
  gcpCredentialsSecretRef:
    name: gcp-credentials
    namespace: default
  
  checkpointConfig:
    checkpointImageRepo: "gcr.io/your-project/training-checkpoints"
    checkpointInterval: "30m"  # Checkpoint every 30 minutes
    checkpointStorage:
      type: "gcs"
      bucket: "your-checkpoint-bucket"
      pathPrefix: "my-training-job"
  
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
          - name: training
            image: "tensorflow/tensorflow:latest-gpu"
            command:
            - "python"
            - "-c"
            - |
              import tensorflow as tf
              print("TensorFlow version:", tf.__version__)
              print("GPUs available:", tf.config.list_physical_devices('GPU'))
              # Your training code here
            resources:
              requests:
                nvidia.com/gpu: 1
                memory: "8Gi"
                cpu: "4"
              limits:
                nvidia.com/gpu: 1
                memory: "8Gi"
                cpu: "4"
```

Apply the resource:

```bash
kubectl apply -f my-training-job.yaml
```

## Step 5: Monitor Your Training Job

Check the status of your SpotInstanceJob:

```bash
kubectl get spotinstancejob my-training-job -o yaml
```

View the created jobs:

```bash
kubectl get jobs -l training.dcnlab.com/spot-instance-job=my-training-job
```

View the pods:

```bash
kubectl get pods -l training.dcnlab.com/spot-instance-job=my-training-job
```

Check the logs:

```bash
kubectl logs -l training.dcnlab.com/spot-instance-job=my-training-job -f
```

## Step 6: Check Checkpoint Status

List checkpoints created for your job:

```bash
kubectl get checkpointbackups -l training.dcnlab.com/spot-instance-job=my-training-job
```

View checkpoint details:

```bash
kubectl get checkpointbackup <checkpoint-name> -o yaml
```

## Handling Preemptions

The operator automatically handles preemption events:

1. When a spot instance receives a preemption notice, the operator detects it
2. If other replicas are still running, it creates an immediate checkpoint
3. It provisions a new spot instance if needed
4. The job is restarted on the new instance using the latest checkpoint

You can simulate a preemption by deleting a node:

```bash
# In GCP Console, delete a spot instance running your job
# The operator will automatically recover
```

## Cleanup

To delete your training job and resources:

```bash
kubectl delete spotinstancejob my-training-job
kubectl delete spotinstancevm my-training-vm
```

To uninstall the operator:

```bash
make undeploy
```

## Troubleshooting

### Operator not starting

Check the operator logs:

```bash
kubectl logs -n spot-instance-training-operator-system deployment/spot-instance-training-operator-controller-manager
```

### Jobs not being created

1. Check the SpotInstanceJob status:
   ```bash
   kubectl describe spotinstancejob my-training-job
   ```

2. Check if the SpotInstanceVM exists:
   ```bash
   kubectl get spotinstancevm
   ```

3. Verify GCP credentials are correct:
   ```bash
   kubectl get secret gcp-credentials -o yaml
   ```

### Spot instances not being provisioned

1. Check GCP service account permissions
2. Verify GCP project ID and zone are correct
3. Check quota limits in GCP Console
4. Review operator logs for errors

## Next Steps

- Customize your training container image
- Implement checkpoint/restore logic in your training code
- Configure checkpoint intervals based on your needs
- Set up monitoring and alerting for your training jobs
- Explore advanced configurations in the API reference

## Support

For issues and questions:
- Check the [main README](README.md) for detailed documentation
- Review the [sequence diagram](sequence-diagram.mmd) for workflow details
- Open an issue on the GitHub repository

