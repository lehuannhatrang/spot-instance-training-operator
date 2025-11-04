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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	trainingv1alpha1 "github.com/dcnlab/spot-instance-training-operator/api/v1alpha1"
)

var _ = Describe("SpotInstanceJob Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		spotinstancejob := &trainingv1alpha1.SpotInstanceJob{}

		BeforeEach(func() {
			By("creating an InstanceTemplate")
			template := &trainingv1alpha1.InstanceTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: trainingv1alpha1.InstanceTemplateSpec{
					VMImage:     "test-image",
					Preemptible: ptr.To(true),
					GCP: trainingv1alpha1.GCPConfig{
						Project:     "test-project",
						Zone:        "test-zone",
						MachineType: "n1-standard-1",
						DiskSizeGB:  100,
					},
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-template", Namespace: "default"}, template)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, template)).To(Succeed())
			}

			By("creating the custom resource for the Kind SpotInstanceJob")
			err = k8sClient.Get(ctx, typeNamespacedName, spotinstancejob)
			if err != nil && errors.IsNotFound(err) {
				resource := &trainingv1alpha1.SpotInstanceJob{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: trainingv1alpha1.SpotInstanceJobSpec{
						Replicas: ptr.To(int32(1)),
						InstanceTemplateRef: corev1.LocalObjectReference{
							Name: "test-template",
						},
						GCPCredentialsSecretRef: corev1.SecretReference{
							Name:      "test-creds",
							Namespace: "default",
						},
						CheckpointConfig: trainingv1alpha1.CheckpointConfig{
							CheckpointImageRepo: "test-repo",
							CheckpointInterval:  metav1.Duration{Duration: 0},
						},
						JobTemplate: batchv1.JobTemplateSpec{
							Spec: batchv1.JobSpec{
								Template: corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "test",
												Image: "test",
											},
										},
										RestartPolicy: corev1.RestartPolicyNever,
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &trainingv1alpha1.SpotInstanceJob{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance SpotInstanceJob")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &SpotInstanceJobReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
