// Copyright (c) 2019-2021 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1alpha1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"

	"github.com/vmware-tanzu/vm-operator/controllers/virtualmachine"
	vmopContext "github.com/vmware-tanzu/vm-operator/pkg/context"
	proberfake "github.com/vmware-tanzu/vm-operator/pkg/prober/fake"
	providerfake "github.com/vmware-tanzu/vm-operator/pkg/vmprovider/fake"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

func unitTests() {
	Describe("Invoking Reconcile", unitTestsReconcile)
}

const finalizer = "virtualmachine.vmoperator.vmware.com"

func unitTestsReconcile() {
	const (
		providerError = "provider error"
	)

	var (
		initObjects      []client.Object
		ctx              *builder.UnitTestContextForController
		reconciler       *virtualmachine.Reconciler
		fakeProbeManager *proberfake.ProberManager
		fakeVMProvider   *providerfake.VMProvider

		vm    *vmopv1alpha1.VirtualMachine
		vmCtx *vmopContext.VirtualMachineContext
	)

	BeforeEach(func() {
		vm = &vmopv1alpha1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "dummy-vm",
				Namespace:  "dummy-ns",
				Labels:     map[string]string{},
				Finalizers: []string{finalizer},
			},
			Spec: vmopv1alpha1.VirtualMachineSpec{
				ClassName: "dummy-class",
				ImageName: "dummy-image",
			},
		}
	})

	JustBeforeEach(func() {
		ctx = suite.NewUnitTestContextForController(initObjects...)
		fakeProbeManagerIf := proberfake.NewFakeProberManager()

		reconciler = virtualmachine.NewReconciler(
			ctx.Client,
			ctx.Logger,
			ctx.Recorder,
			ctx.VMProvider,
			fakeProbeManagerIf,
			16,
		)
		fakeVMProvider = ctx.VMProvider.(*providerfake.VMProvider)
		fakeProbeManager = fakeProbeManagerIf.(*proberfake.ProberManager)

		vmCtx = &vmopContext.VirtualMachineContext{
			Context: ctx,
			Logger:  ctx.Logger.WithName(vm.Name),
			VM:      vm,
		}
	})

	AfterEach(func() {
		ctx.AfterEach()
		ctx = nil
		initObjects = nil
		vmCtx = nil
		reconciler = nil
		fakeVMProvider = nil
	})

	Context("ReconcileNormal", func() {
		BeforeEach(func() {
			initObjects = append(initObjects, vm)
		})

		When("object does not have finalizer set", func() {
			BeforeEach(func() {
				vm.Finalizers = nil
			})

			It("will set finalizer", func() {
				err := reconciler.ReconcileNormal(vmCtx)
				Expect(err).NotTo(HaveOccurred())
				Expect(vmCtx.VM.GetFinalizers()).To(ContainElement(finalizer))
			})
		})

		It("will have finalizer set upon successful reconciliation", func() {
			err := reconciler.ReconcileNormal(vmCtx)
			Expect(err).NotTo(HaveOccurred())
			Expect(vmCtx.VM.GetFinalizers()).To(ContainElement(finalizer))
			Expect(vmCtx.VM.Status.Phase).To(Equal(vmopv1alpha1.Created))
		})

		It("will return error when provider fails to CreateOrUpdate VM", func() {
			fakeVMProvider.CreateOrUpdateVirtualMachineFn = func(ctx context.Context, vm *vmopv1alpha1.VirtualMachine) error {
				return errors.New(providerError)
			}

			err := reconciler.ReconcileNormal(vmCtx)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(providerError))
			expectEvent(ctx, "CreateOrUpdateFailure")
		})

		It("can be called multiple times", func() {
			err := reconciler.ReconcileNormal(vmCtx)
			Expect(err).ToNot(HaveOccurred())
			Expect(vmCtx.VM.GetFinalizers()).To(ContainElement(finalizer))

			err = reconciler.ReconcileNormal(vmCtx)
			Expect(err).ToNot(HaveOccurred())
			Expect(vmCtx.VM.GetFinalizers()).To(ContainElement(finalizer))
		})

		It("Should not call add to Prober Manager if CreateOrUpdate fails", func() {
			fakeVMProvider.CreateOrUpdateVirtualMachineFn = func(ctx context.Context, vm *vmopv1alpha1.VirtualMachine) error {
				return errors.New(providerError)
			}

			err := reconciler.ReconcileNormal(vmCtx)
			Expect(err).To(HaveOccurred())
			Expect(fakeProbeManager.IsAddToProberManagerCalled).Should(BeFalse())
		})

		It("Should call add to Prober Manager if ReconcileNormal succeeds", func() {
			fakeProbeManager.AddToProberManagerFn = func(vm *vmopv1alpha1.VirtualMachine) {
				fakeProbeManager.IsAddToProberManagerCalled = true
			}

			Expect(reconciler.ReconcileNormal(vmCtx)).Should(Succeed())
			Expect(fakeProbeManager.IsAddToProberManagerCalled).Should(BeTrue())
		})
	})

	Context("ReconcileDelete", func() {
		BeforeEach(func() {
			initObjects = append(initObjects, vm)
		})

		JustBeforeEach(func() {
			// Create the VM to be deleted
			err := reconciler.ReconcileNormal(vmCtx)
			Expect(err).NotTo(HaveOccurred())
		})

		It("will delete the created VM and emit corresponding event", func() {
			err := reconciler.ReconcileDelete(vmCtx)
			Expect(err).NotTo(HaveOccurred())

			expectEvent(ctx, "DeleteSuccess")
			Expect(vmCtx.VM.Status.Phase).To(Equal(vmopv1alpha1.Deleted))
		})

		It("will emit corresponding event during delete failure", func() {
			// Simulate delete failure
			fakeVMProvider.DeleteVirtualMachineFn = func(ctx context.Context, vm *vmopv1alpha1.VirtualMachine) error {
				return errors.New(providerError)
			}
			err := reconciler.ReconcileDelete(vmCtx)
			Expect(err).To(HaveOccurred())

			expectEvent(ctx, "DeleteFailure")
			Expect(vmCtx.VM.Status.Phase).To(Equal(vmopv1alpha1.Deleting))
		})

		It("Should not remove from Prober Manager if ReconcileDelete fails", func() {
			// Simulate delete failure
			fakeVMProvider.DeleteVirtualMachineFn = func(ctx context.Context, vm *vmopv1alpha1.VirtualMachine) error {
				return errors.New(providerError)
			}

			err := reconciler.ReconcileDelete(vmCtx)
			Expect(err).To(HaveOccurred())
			Expect(fakeProbeManager.IsRemoveFromProberManagerCalled).Should(BeFalse())
		})

		It("Should remove from Prober Manager if ReconcileDelete succeeds", func() {
			fakeProbeManager.RemoveFromProberManagerFn = func(vm *vmopv1alpha1.VirtualMachine) {
				fakeProbeManager.IsRemoveFromProberManagerCalled = true
			}

			Expect(reconciler.ReconcileDelete(vmCtx)).Should(Succeed())
			Expect(fakeProbeManager.IsRemoveFromProberManagerCalled).Should(BeTrue())
		})
	})
}

func expectEvent(ctx *builder.UnitTestContextForController, eventStr string) {
	var event string
	// This does not work if we have more than one events and the first one does not match.
	EventuallyWithOffset(1, ctx.Events).Should(Receive(&event))
	eventComponents := strings.Split(event, " ")
	ExpectWithOffset(1, eventComponents[1]).To(Equal(eventStr))
}
