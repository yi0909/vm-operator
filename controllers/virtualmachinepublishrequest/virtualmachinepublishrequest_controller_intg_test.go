// Copyright (c) 2022-2023 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package virtualmachinepublishrequest_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/uuid"
	"github.com/vmware/govmomi/vim25/types"

	vmopv1alpha1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"
	imgregv1a1 "github.com/vmware-tanzu/vm-operator/external/image-registry/api/v1alpha1"

	"github.com/vmware-tanzu/vm-operator/controllers/contentlibrary/utils"
	"github.com/vmware-tanzu/vm-operator/controllers/virtualmachinepublishrequest"
	"github.com/vmware-tanzu/vm-operator/pkg/conditions"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

func intgTests() {
	Describe("Invoking VirtualMachinePublishRequest controller tests", virtualMachinePublishRequestReconcile)
}

func virtualMachinePublishRequestReconcile() {
	var (
		ctx   *builder.IntegrationTestContext
		vmpub *vmopv1alpha1.VirtualMachinePublishRequest
		vm    *vmopv1alpha1.VirtualMachine
		cl    *imgregv1a1.ContentLibrary
	)

	getVirtualMachinePublishRequest := func(ctx *builder.IntegrationTestContext, objKey client.ObjectKey) *vmopv1alpha1.VirtualMachinePublishRequest {
		vmpubObj := &vmopv1alpha1.VirtualMachinePublishRequest{}
		if err := ctx.Client.Get(ctx, objKey, vmpubObj); err != nil {
			return nil
		}
		return vmpubObj
	}

	BeforeEach(func() {
		ctx = suite.NewIntegrationTestContext()

		vm = &vmopv1alpha1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dummy-vm",
				Namespace: ctx.Namespace,
			},
			Spec: vmopv1alpha1.VirtualMachineSpec{
				ImageName:  "dummy-image",
				ClassName:  "dummy-class",
				PowerState: vmopv1alpha1.VirtualMachinePoweredOn,
			},
		}

		vmpub = builder.DummyVirtualMachinePublishRequest("dummy-vmpub", ctx.Namespace, vm.Name,
			"dummy-item", "dummy-cl")
		cl = builder.DummyContentLibrary("dummy-cl", ctx.Namespace, "dummy-cl")
	})

	AfterEach(func() {
		ctx.AfterEach()
		ctx = nil
	})

	Context("Reconcile", func() {
		var (
			itemID string
		)

		BeforeEach(func() {
			Expect(ctx.Client.Create(ctx, vm)).To(Succeed())
			vmObj := &vmopv1alpha1.VirtualMachine{}
			Expect(ctx.Client.Get(ctx, client.ObjectKeyFromObject(vm), vmObj)).To(Succeed())
			vmObj.Status = vmopv1alpha1.VirtualMachineStatus{
				Phase:    vmopv1alpha1.Created,
				UniqueID: "dummy-unique-id",
			}
			Expect(ctx.Client.Status().Update(ctx, vmObj)).To(Succeed())
			Expect(ctx.Client.Create(ctx, cl)).To(Succeed())

			cl.Status.Conditions = []imgregv1a1.Condition{
				{
					Type:   imgregv1a1.ReadyCondition,
					Status: corev1.ConditionTrue,
				},
			}
			Expect(ctx.Client.Status().Update(ctx, cl)).To(Succeed())

			Expect(ctx.Client.Create(ctx, vmpub)).To(Succeed())

			itemID = uuid.New().String()
			go func() {
				// create ContentLibraryItem and VirtualMachineImage once the task succeeds.
				for {
					obj := getVirtualMachinePublishRequest(ctx, client.ObjectKeyFromObject(vmpub))
					if state := intgFakeVMProvider.GetVMPublishRequestResult(obj); state == types.TaskInfoStateSuccess {
						clitem := &imgregv1a1.ContentLibraryItem{}
						err := ctx.Client.Get(ctx, client.ObjectKey{Name: "dummy-clitem", Namespace: ctx.Namespace}, clitem)
						if k8serrors.IsNotFound(err) {
							clitem = utils.DummyContentLibraryItem("dummy-clitem", ctx.Namespace)
							Expect(ctx.Client.Create(ctx, clitem)).To(Succeed())

							vmi := builder.DummyVirtualMachineImage("dummy-image")
							vmi.Namespace = ctx.Namespace
							vmi.Spec.ImageID = itemID
							Expect(ctx.Client.Create(ctx, vmi)).To(Succeed())
							vmi.Status.ImageName = vmpub.Spec.Target.Item.Name
							Expect(ctx.Client.Status().Update(ctx, vmi)).To(Succeed())

							return
						}
					}
					time.Sleep(time.Second)
				}
			}()
		})

		AfterEach(func() {
			err := ctx.Client.Delete(ctx, vmpub)
			Expect(err == nil || k8serrors.IsNotFound(err)).To(BeTrue())
			err = ctx.Client.Delete(ctx, vm)
			Expect(err == nil || k8serrors.IsNotFound(err)).To(BeTrue())
			err = ctx.Client.Delete(ctx, cl)
			Expect(err == nil || k8serrors.IsNotFound(err)).To(BeTrue())

			intgFakeVMProvider.Reset()
		})

		It("VirtualMachinePublishRequest completed", func() {
			By("VM publish task is queued", func() {
				intgFakeVMProvider.Lock()
				intgFakeVMProvider.GetTasksByActIDFn = func(_ context.Context, actID string) (tasksInfo []types.TaskInfo, retErr error) {
					task := types.TaskInfo{
						DescriptionId: virtualmachinepublishrequest.TaskDescriptionID,
						State:         types.TaskInfoStateQueued,
					}
					return []types.TaskInfo{task}, nil
				}
				intgFakeVMProvider.Unlock()

				// Force trigger a reconcile.
				vmPubObj := getVirtualMachinePublishRequest(ctx, client.ObjectKeyFromObject(vmpub))
				vmPubObj.Annotations = map[string]string{"dummy": "dummy-1"}
				Expect(ctx.Client.Update(ctx, vmPubObj)).To(Succeed())

				Eventually(func() bool {
					vmPubObj = getVirtualMachinePublishRequest(ctx, client.ObjectKeyFromObject(vmpub))
					for _, condition := range vmPubObj.Status.Conditions {
						if condition.Type == vmopv1alpha1.VirtualMachinePublishRequestConditionUploaded {
							return condition.Status == corev1.ConditionFalse &&
								condition.Reason == vmopv1alpha1.UploadTaskQueuedReason &&
								vmPubObj.Status.Attempts == 1
						}
					}
					return false
				}).Should(BeTrue())
			})

			By("VM publish task is running", func() {
				intgFakeVMProvider.Lock()
				intgFakeVMProvider.GetTasksByActIDFn = func(_ context.Context, actID string) (tasksInfo []types.TaskInfo, retErr error) {
					task := types.TaskInfo{
						DescriptionId: virtualmachinepublishrequest.TaskDescriptionID,
						State:         types.TaskInfoStateRunning,
					}
					return []types.TaskInfo{task}, nil
				}
				intgFakeVMProvider.Unlock()

				// Force trigger a reconcile.
				vmPubObj := getVirtualMachinePublishRequest(ctx, client.ObjectKeyFromObject(vmpub))
				vmPubObj.Annotations = map[string]string{"dummy": "dummy-2"}
				Expect(ctx.Client.Update(ctx, vmPubObj)).To(Succeed())

				Eventually(func() bool {
					vmPubObj = getVirtualMachinePublishRequest(ctx, client.ObjectKeyFromObject(vmpub))
					for _, condition := range vmPubObj.Status.Conditions {
						if condition.Type == vmopv1alpha1.VirtualMachinePublishRequestConditionUploaded {
							return condition.Status == corev1.ConditionFalse &&
								condition.Reason == vmopv1alpha1.UploadingReason &&
								vmPubObj.Status.Attempts == 1
						}
					}
					return false
				}).Should(BeTrue())
			})

			By("VM publish task succeeded", func() {
				intgFakeVMProvider.Lock()
				intgFakeVMProvider.GetTasksByActIDFn = func(_ context.Context, actID string) (tasksInfo []types.TaskInfo, retErr error) {
					task := types.TaskInfo{
						DescriptionId: virtualmachinepublishrequest.TaskDescriptionID,
						State:         types.TaskInfoStateSuccess,
						Result:        types.ManagedObjectReference{Type: "ContentLibraryItem", Value: fmt.Sprintf("clibitem-%s", itemID)},
					}
					return []types.TaskInfo{task}, nil
				}
				intgFakeVMProvider.Unlock()

				// Force trigger a reconcile.
				vmPubObj := getVirtualMachinePublishRequest(ctx, client.ObjectKeyFromObject(vmpub))
				vmPubObj.Annotations = map[string]string{"dummy": "dummy-3"}
				Expect(ctx.Client.Update(ctx, vmPubObj)).To(Succeed())

				obj := &vmopv1alpha1.VirtualMachinePublishRequest{}
				Eventually(func() bool {
					obj = getVirtualMachinePublishRequest(ctx, client.ObjectKeyFromObject(vmpub))
					return obj != nil && conditions.IsTrue(obj,
						vmopv1alpha1.VirtualMachinePublishRequestConditionComplete)
				}).Should(BeTrue())

				Expect(obj.Status.Ready).To(BeTrue())
				Expect(obj.Status.ImageName).To(Equal("dummy-image"))
				Expect(obj.Status.CompletionTime).NotTo(BeZero())
			})
		})
	})
}
