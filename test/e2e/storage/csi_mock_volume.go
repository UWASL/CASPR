/*
Copyright 2019 The Kubernetes Authors.

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

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	storagev1alpha1 "k8s.io/api/storage/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	cachetools "k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/kubernetes/pkg/controller/volume/scheduling"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eevents "k8s.io/kubernetes/test/e2e/framework/events"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	"k8s.io/kubernetes/test/e2e/storage/drivers"
	"k8s.io/kubernetes/test/e2e/storage/testpatterns"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
	utilptr "k8s.io/utils/pointer"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

const (
	csiNodeLimitUpdateTimeout  = 5 * time.Minute
	csiPodUnschedulableTimeout = 5 * time.Minute
	csiResizeWaitPeriod        = 5 * time.Minute
	csiVolumeAttachmentTimeout = 7 * time.Minute
	// how long to wait for Resizing Condition on PVC to appear
	csiResizingConditionWait = 2 * time.Minute

	// Time for starting a pod with a volume.
	csiPodRunningTimeout = 5 * time.Minute

	// How log to wait for kubelet to unstage a volume after a pod is deleted
	csiUnstageWaitTimeout = 1 * time.Minute

	// Name of CSI driver pod name (it's in a StatefulSet with a stable name)
	driverPodName = "csi-mockplugin-0"
	// Name of CSI driver container name
	driverContainerName = "mock"
	// Prefix of the mock driver grpc log
	grpcCallPrefix = "gRPCCall:"
)

// csiCall represents an expected call from Kubernetes to CSI mock driver and
// expected return value.
// When matching expected csiCall with a real CSI mock driver output, one csiCall
// matches *one or more* calls with the same method and error code.
// This is due to exponential backoff in Kubernetes, where the test cannot expect
// exact number of call repetitions.
type csiCall struct {
	expectedMethod string
	expectedError  codes.Code
	// This is a mark for the test itself to delete the tested pod *after*
	// this csiCall is received.
	deletePod bool
}

var _ = utils.SIGDescribe("CSI mock volume", func() {
	type testParameters struct {
		disableAttach       bool
		attachLimit         int
		registerDriver      bool
		lateBinding         bool
		enableTopology      bool
		podInfo             *bool
		storageCapacity     *bool
		scName              string // pre-selected storage class name; must be unique in the cluster
		enableResizing      bool   // enable resizing for both CSI mock driver and storageClass.
		enableNodeExpansion bool   // enable node expansion for CSI mock driver
		// just disable resizing on driver it overrides enableResizing flag for CSI mock driver
		disableResizingOnDriver bool
		enableSnapshot          bool
		javascriptHooks         map[string]string
		tokenRequests           []storagev1.TokenRequest
		requiresRepublish       *bool
		fsGroupPolicy           *storagev1.FSGroupPolicy
	}

	type mockDriverSetup struct {
		cs           clientset.Interface
		config       *testsuites.PerTestConfig
		testCleanups []func()
		pods         []*v1.Pod
		pvcs         []*v1.PersistentVolumeClaim
		sc           map[string]*storagev1.StorageClass
		vsc          map[string]*unstructured.Unstructured
		driver       testsuites.TestDriver
		provisioner  string
		tp           testParameters
	}

	var m mockDriverSetup

	f := framework.NewDefaultFramework("csi-mock-volumes")

	init := func(tp testParameters) {
		m = mockDriverSetup{
			cs:  f.ClientSet,
			sc:  make(map[string]*storagev1.StorageClass),
			vsc: make(map[string]*unstructured.Unstructured),
			tp:  tp,
		}
		cs := f.ClientSet
		var err error
		driverOpts := drivers.CSIMockDriverOpts{
			RegisterDriver:      tp.registerDriver,
			PodInfo:             tp.podInfo,
			StorageCapacity:     tp.storageCapacity,
			EnableTopology:      tp.enableTopology,
			AttachLimit:         tp.attachLimit,
			DisableAttach:       tp.disableAttach,
			EnableResizing:      tp.enableResizing,
			EnableNodeExpansion: tp.enableNodeExpansion,
			EnableSnapshot:      tp.enableSnapshot,
			JavascriptHooks:     tp.javascriptHooks,
			TokenRequests:       tp.tokenRequests,
			RequiresRepublish:   tp.requiresRepublish,
			FSGroupPolicy:       tp.fsGroupPolicy,
		}

		// this just disable resizing on driver, keeping resizing on SC enabled.
		if tp.disableResizingOnDriver {
			driverOpts.EnableResizing = false
		}

		m.driver = drivers.InitMockCSIDriver(driverOpts)
		config, testCleanup := m.driver.PrepareTest(f)
		m.testCleanups = append(m.testCleanups, testCleanup)
		m.config = config
		m.provisioner = config.GetUniqueDriverName()

		if tp.registerDriver {
			err = waitForCSIDriver(cs, m.config.GetUniqueDriverName())
			framework.ExpectNoError(err, "Failed to get CSIDriver %v", m.config.GetUniqueDriverName())
			m.testCleanups = append(m.testCleanups, func() {
				destroyCSIDriver(cs, m.config.GetUniqueDriverName())
			})
		}

		// Wait for the CSIDriver actually get deployed and CSINode object to be generated.
		// This indicates the mock CSI driver pod is up and running healthy.
		err = drivers.WaitForCSIDriverRegistrationOnNode(m.config.ClientNodeSelection.Name, m.config.GetUniqueDriverName(), cs)
		framework.ExpectNoError(err, "Failed to register CSIDriver %v", m.config.GetUniqueDriverName())
	}

	createPod := func(ephemeral bool) (class *storagev1.StorageClass, claim *v1.PersistentVolumeClaim, pod *v1.Pod) {
		ginkgo.By("Creating pod")
		var sc *storagev1.StorageClass
		if dDriver, ok := m.driver.(testsuites.DynamicPVTestDriver); ok {
			sc = dDriver.GetDynamicProvisionStorageClass(m.config, "")
		}
		scTest := testsuites.StorageClassTest{
			Name:                 m.driver.GetDriverInfo().Name,
			Provisioner:          sc.Provisioner,
			Parameters:           sc.Parameters,
			ClaimSize:            "1Gi",
			ExpectedSize:         "1Gi",
			DelayBinding:         m.tp.lateBinding,
			AllowVolumeExpansion: m.tp.enableResizing,
		}

		// The mock driver only works when everything runs on a single node.
		nodeSelection := m.config.ClientNodeSelection
		if ephemeral {
			pod = startPausePodInline(f.ClientSet, scTest, nodeSelection, f.Namespace.Name)
			if pod != nil {
				m.pods = append(m.pods, pod)
			}
		} else {
			class, claim, pod = startPausePod(f.ClientSet, scTest, nodeSelection, m.tp.scName, f.Namespace.Name)
			if class != nil {
				m.sc[class.Name] = class
			}
			if claim != nil {
				m.pvcs = append(m.pvcs, claim)
			}
			if pod != nil {
				m.pods = append(m.pods, pod)
			}
		}
		return // result variables set above
	}

	createPodWithPVC := func(pvc *v1.PersistentVolumeClaim) (*v1.Pod, error) {
		nodeSelection := m.config.ClientNodeSelection
		pod, err := startPausePodWithClaim(m.cs, pvc, nodeSelection, f.Namespace.Name)
		if pod != nil {
			m.pods = append(m.pods, pod)
		}
		return pod, err
	}

	createPodWithFSGroup := func(fsGroup *int64) (*storagev1.StorageClass, *v1.PersistentVolumeClaim, *v1.Pod) {
		ginkgo.By("Creating pod with fsGroup")
		nodeSelection := m.config.ClientNodeSelection
		var sc *storagev1.StorageClass
		if dDriver, ok := m.driver.(testsuites.DynamicPVTestDriver); ok {
			sc = dDriver.GetDynamicProvisionStorageClass(m.config, "")
		}
		scTest := testsuites.StorageClassTest{
			Name:                 m.driver.GetDriverInfo().Name,
			Provisioner:          sc.Provisioner,
			Parameters:           sc.Parameters,
			ClaimSize:            "1Gi",
			ExpectedSize:         "1Gi",
			DelayBinding:         m.tp.lateBinding,
			AllowVolumeExpansion: m.tp.enableResizing,
		}

		class, claim, pod := startBusyBoxPod(f.ClientSet, scTest, nodeSelection, m.tp.scName, f.Namespace.Name, fsGroup)

		if class != nil {
			m.sc[class.Name] = class
		}
		if claim != nil {
			m.pvcs = append(m.pvcs, claim)
		}

		if pod != nil {
			m.pods = append(m.pods, pod)
		}

		return class, claim, pod
	}

	cleanup := func() {
		cs := f.ClientSet
		var errs []error

		for _, pod := range m.pods {
			ginkgo.By(fmt.Sprintf("Deleting pod %s", pod.Name))
			errs = append(errs, e2epod.DeletePodWithWait(cs, pod))
		}

		for _, claim := range m.pvcs {
			ginkgo.By(fmt.Sprintf("Deleting claim %s", claim.Name))
			claim, err := cs.CoreV1().PersistentVolumeClaims(claim.Namespace).Get(context.TODO(), claim.Name, metav1.GetOptions{})
			if err == nil {
				if err := cs.CoreV1().PersistentVolumeClaims(claim.Namespace).Delete(context.TODO(), claim.Name, metav1.DeleteOptions{}); err != nil {
					errs = append(errs, err)
				}
				if claim.Spec.VolumeName != "" {
					errs = append(errs, e2epv.WaitForPersistentVolumeDeleted(cs, claim.Spec.VolumeName, framework.Poll, 2*time.Minute))
				}
			}
		}

		for _, sc := range m.sc {
			ginkgo.By(fmt.Sprintf("Deleting storageclass %s", sc.Name))
			cs.StorageV1().StorageClasses().Delete(context.TODO(), sc.Name, metav1.DeleteOptions{})
		}

		for _, vsc := range m.vsc {
			ginkgo.By(fmt.Sprintf("Deleting volumesnapshotclass %s", vsc.GetName()))
			m.config.Framework.DynamicClient.Resource(testsuites.SnapshotClassGVR).Delete(context.TODO(), vsc.GetName(), metav1.DeleteOptions{})
		}
		ginkgo.By("Cleaning up resources")
		for _, cleanupFunc := range m.testCleanups {
			cleanupFunc()
		}

		err := utilerrors.NewAggregate(errs)
		framework.ExpectNoError(err, "while cleaning up after test")
	}

	// The CSIDriverRegistry feature gate is needed for this test in Kubernetes 1.12.
	ginkgo.Context("CSI attach test using mock driver", func() {
		tests := []struct {
			name                   string
			disableAttach          bool
			deployClusterRegistrar bool
		}{
			{
				name:                   "should not require VolumeAttach for drivers without attachment",
				disableAttach:          true,
				deployClusterRegistrar: true,
			},
			{
				name:                   "should require VolumeAttach for drivers with attachment",
				deployClusterRegistrar: true,
			},
			{
				name:                   "should preserve attachment policy when no CSIDriver present",
				deployClusterRegistrar: false,
			},
		}
		for _, t := range tests {
			test := t
			ginkgo.It(t.name, func() {
				var err error
				init(testParameters{registerDriver: test.deployClusterRegistrar, disableAttach: test.disableAttach})
				defer cleanup()

				_, claim, pod := createPod(false)
				if pod == nil {
					return
				}
				err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
				framework.ExpectNoError(err, "Failed to start pod: %v", err)

				ginkgo.By("Checking if VolumeAttachment was created for the pod")
				handle := getVolumeHandle(m.cs, claim)
				attachmentHash := sha256.Sum256([]byte(fmt.Sprintf("%s%s%s", handle, m.provisioner, m.config.ClientNodeSelection.Name)))
				attachmentName := fmt.Sprintf("csi-%x", attachmentHash)
				_, err = m.cs.StorageV1().VolumeAttachments().Get(context.TODO(), attachmentName, metav1.GetOptions{})
				if err != nil {
					if apierrors.IsNotFound(err) {
						if !test.disableAttach {
							framework.ExpectNoError(err, "Expected VolumeAttachment but none was found")
						}
					} else {
						framework.ExpectNoError(err, "Failed to find VolumeAttachment")
					}
				}
				if test.disableAttach {
					framework.ExpectError(err, "Unexpected VolumeAttachment found")
				}
			})

		}
	})

	ginkgo.Context("CSI CSIDriver deployment after pod creation using non-attachable mock driver", func() {
		ginkgo.It("should bringup pod after deploying CSIDriver attach=false [Slow]", func() {
			var err error
			init(testParameters{registerDriver: false, disableAttach: true})
			defer cleanup()

			_, claim, pod := createPod(false /* persistent volume, late binding as specified above */)
			if pod == nil {
				return
			}

			ginkgo.By("Checking if attaching failed and pod cannot start")
			eventSelector := fields.Set{
				"involvedObject.kind":      "Pod",
				"involvedObject.name":      pod.Name,
				"involvedObject.namespace": pod.Namespace,
				"reason":                   events.FailedAttachVolume,
			}.AsSelector().String()
			msg := "AttachVolume.Attach failed for volume"

			err = e2eevents.WaitTimeoutForEvent(m.cs, pod.Namespace, eventSelector, msg, framework.PodStartTimeout)
			if err != nil {
				podErr := e2epod.WaitTimeoutForPodRunningInNamespace(m.cs, pod.Name, pod.Namespace, 10*time.Second)
				framework.ExpectError(podErr, "Pod should not be in running status because attaching should failed")
				// Events are unreliable, don't depend on the event. It's used only to speed up the test.
				framework.Logf("Attach should fail and the corresponding event should show up, error: %v", err)
			}

			// VolumeAttachment should be created because the default value for CSI attachable is true
			ginkgo.By("Checking if VolumeAttachment was created for the pod")
			handle := getVolumeHandle(m.cs, claim)
			attachmentHash := sha256.Sum256([]byte(fmt.Sprintf("%s%s%s", handle, m.provisioner, m.config.ClientNodeSelection.Name)))
			attachmentName := fmt.Sprintf("csi-%x", attachmentHash)
			_, err = m.cs.StorageV1().VolumeAttachments().Get(context.TODO(), attachmentName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					framework.ExpectNoError(err, "Expected VolumeAttachment but none was found")
				} else {
					framework.ExpectNoError(err, "Failed to find VolumeAttachment")
				}
			}

			ginkgo.By("Deploy CSIDriver object with attachRequired=false")
			driverNamespace := m.config.DriverNamespace

			canAttach := false
			o := utils.PatchCSIOptions{
				OldDriverName: "csi-mock",
				NewDriverName: "csi-mock-" + f.UniqueName,
				CanAttach:     &canAttach,
			}
			cleanupCSIDriver, err := utils.CreateFromManifests(f, driverNamespace, func(item interface{}) error {
				return utils.PatchCSIDeployment(f, o, item)
			}, "test/e2e/testing-manifests/storage-csi/mock/csi-mock-driverinfo.yaml")
			if err != nil {
				framework.Failf("fail to deploy CSIDriver object: %v", err)
			}
			m.testCleanups = append(m.testCleanups, cleanupCSIDriver)

			ginkgo.By("Wait for the pod in running status")
			err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
			framework.ExpectNoError(err, "Failed to start pod: %v", err)

			ginkgo.By(fmt.Sprintf("Wait for the volumeattachment to be deleted up to %v", csiVolumeAttachmentTimeout))
			// This step can be slow because we have to wait either a NodeUpdate event happens or
			// the detachment for this volume timeout so that we can do a force detach.
			err = waitForVolumeAttachmentTerminated(attachmentName, m.cs)
			framework.ExpectNoError(err, "Failed to delete VolumeAttachment: %v", err)
		})
	})

	ginkgo.Context("CSI workload information using mock driver", func() {
		var (
			err          error
			podInfoTrue  = true
			podInfoFalse = false
		)
		tests := []struct {
			name                   string
			podInfoOnMount         *bool
			deployClusterRegistrar bool
			expectPodInfo          bool
			expectEphemeral        bool
		}{
			{
				name:                   "should not be passed when podInfoOnMount=nil",
				podInfoOnMount:         nil,
				deployClusterRegistrar: true,
				expectPodInfo:          false,
				expectEphemeral:        false,
			},
			{
				name:                   "should be passed when podInfoOnMount=true",
				podInfoOnMount:         &podInfoTrue,
				deployClusterRegistrar: true,
				expectPodInfo:          true,
				expectEphemeral:        false,
			},
			{
				name:                   "contain ephemeral=true when using inline volume",
				podInfoOnMount:         &podInfoTrue,
				deployClusterRegistrar: true,
				expectPodInfo:          true,
				expectEphemeral:        true,
			},
			{
				name:                   "should not be passed when podInfoOnMount=false",
				podInfoOnMount:         &podInfoFalse,
				deployClusterRegistrar: true,
				expectPodInfo:          false,
				expectEphemeral:        false,
			},
			{
				name:                   "should not be passed when CSIDriver does not exist",
				deployClusterRegistrar: false,
				expectPodInfo:          false,
				expectEphemeral:        false,
			},
		}
		for _, t := range tests {
			test := t
			ginkgo.It(t.name, func() {
				init(testParameters{
					registerDriver: test.deployClusterRegistrar,
					podInfo:        test.podInfoOnMount})

				defer cleanup()

				_, _, pod := createPod(test.expectEphemeral)
				if pod == nil {
					return
				}
				err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
				framework.ExpectNoError(err, "Failed to start pod: %v", err)

				// If we expect an ephemeral volume, the feature has to be enabled.
				// Otherwise need to check if we expect pod info, because the content
				// of that depends on whether the feature is enabled or not.
				csiInlineVolumesEnabled := test.expectEphemeral
				if test.expectPodInfo {
					ginkgo.By("checking for CSIInlineVolumes feature")
					csiInlineVolumesEnabled, err = testsuites.CSIInlineVolumesEnabled(m.cs, f.Namespace.Name)
					framework.ExpectNoError(err, "failed to test for CSIInlineVolumes")
				}

				ginkgo.By("Deleting the previously created pod")
				err = e2epod.DeletePodWithWait(m.cs, pod)
				framework.ExpectNoError(err, "while deleting")

				ginkgo.By("Checking CSI driver logs")
				err = checkPodLogs(m.cs, m.config.DriverNamespace.Name, driverPodName, driverContainerName, pod, test.expectPodInfo, test.expectEphemeral, csiInlineVolumesEnabled, false, 1)
				framework.ExpectNoError(err)
			})
		}
	})

	ginkgo.Context("CSI volume limit information using mock driver", func() {
		ginkgo.It("should report attach limit when limit is bigger than 0 [Slow]", func() {
			// define volume limit to be 2 for this test
			var err error
			init(testParameters{attachLimit: 2})
			defer cleanup()
			nodeName := m.config.ClientNodeSelection.Name
			driverName := m.config.GetUniqueDriverName()

			csiNodeAttachLimit, err := checkCSINodeForLimits(nodeName, driverName, m.cs)
			framework.ExpectNoError(err, "while checking limits in CSINode: %v", err)

			gomega.Expect(csiNodeAttachLimit).To(gomega.BeNumerically("==", 2))

			_, _, pod1 := createPod(false)
			gomega.Expect(pod1).NotTo(gomega.BeNil(), "while creating first pod")

			err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod1.Name, pod1.Namespace)
			framework.ExpectNoError(err, "Failed to start pod1: %v", err)

			_, _, pod2 := createPod(false)
			gomega.Expect(pod2).NotTo(gomega.BeNil(), "while creating second pod")

			err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod2.Name, pod2.Namespace)
			framework.ExpectNoError(err, "Failed to start pod2: %v", err)

			_, _, pod3 := createPod(false)
			gomega.Expect(pod3).NotTo(gomega.BeNil(), "while creating third pod")
			err = waitForMaxVolumeCondition(pod3, m.cs)
			framework.ExpectNoError(err, "while waiting for max volume condition on pod : %+v", pod3)
		})
	})

	ginkgo.Context("CSI Volume expansion", func() {
		tests := []struct {
			name                    string
			nodeExpansionRequired   bool
			disableAttach           bool
			disableResizingOnDriver bool
			expectFailure           bool
		}{
			{
				name:                  "should expand volume without restarting pod if nodeExpansion=off",
				nodeExpansionRequired: false,
			},
			{
				name:                  "should expand volume by restarting pod if attach=on, nodeExpansion=on",
				nodeExpansionRequired: true,
			},
			{
				name:                  "should expand volume by restarting pod if attach=off, nodeExpansion=on",
				disableAttach:         true,
				nodeExpansionRequired: true,
			},
			{
				name:                    "should not expand volume if resizingOnDriver=off, resizingOnSC=on",
				disableResizingOnDriver: true,
				expectFailure:           true,
			},
		}
		for _, t := range tests {
			test := t
			ginkgo.It(t.name, func() {
				var err error
				tp := testParameters{
					enableResizing:          true,
					enableNodeExpansion:     test.nodeExpansionRequired,
					disableResizingOnDriver: test.disableResizingOnDriver,
				}
				// disabling attach requires drive registration feature
				if test.disableAttach {
					tp.disableAttach = true
					tp.registerDriver = true
				}

				init(tp)
				defer cleanup()

				sc, pvc, pod := createPod(false)
				gomega.Expect(pod).NotTo(gomega.BeNil(), "while creating pod for resizing")

				framework.ExpectEqual(*sc.AllowVolumeExpansion, true, "failed creating sc with allowed expansion")

				err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
				framework.ExpectNoError(err, "Failed to start pod1: %v", err)

				ginkgo.By("Expanding current pvc")
				newSize := resource.MustParse("6Gi")
				newPVC, err := testsuites.ExpandPVCSize(pvc, newSize, m.cs)
				framework.ExpectNoError(err, "While updating pvc for more size")
				pvc = newPVC
				gomega.Expect(pvc).NotTo(gomega.BeNil())

				pvcSize := pvc.Spec.Resources.Requests[v1.ResourceStorage]
				if pvcSize.Cmp(newSize) != 0 {
					framework.Failf("error updating pvc size %q", pvc.Name)
				}
				if test.expectFailure {
					err = testsuites.WaitForResizingCondition(pvc, m.cs, csiResizingConditionWait)
					framework.ExpectError(err, "unexpected resizing condition on PVC")
					return
				}

				ginkgo.By("Waiting for persistent volume resize to finish")
				err = testsuites.WaitForControllerVolumeResize(pvc, m.cs, csiResizeWaitPeriod)
				framework.ExpectNoError(err, "While waiting for CSI PV resize to finish")

				checkPVCSize := func() {
					ginkgo.By("Waiting for PVC resize to finish")
					pvc, err = testsuites.WaitForFSResize(pvc, m.cs)
					framework.ExpectNoError(err, "while waiting for PVC resize to finish")

					pvcConditions := pvc.Status.Conditions
					framework.ExpectEqual(len(pvcConditions), 0, "pvc should not have conditions")
				}

				// if node expansion is not required PVC should be resized as well
				if !test.nodeExpansionRequired {
					checkPVCSize()
				} else {
					ginkgo.By("Checking for conditions on pvc")
					npvc, err := testsuites.WaitForPendingFSResizeCondition(pvc, m.cs)
					framework.ExpectNoError(err, "While waiting for pvc to have fs resizing condition")
					pvc = npvc

					inProgressConditions := pvc.Status.Conditions
					if len(inProgressConditions) > 0 {
						framework.ExpectEqual(inProgressConditions[0].Type, v1.PersistentVolumeClaimFileSystemResizePending, "pvc must have fs resizing condition")
					}

					ginkgo.By("Deleting the previously created pod")
					err = e2epod.DeletePodWithWait(m.cs, pod)
					framework.ExpectNoError(err, "while deleting pod for resizing")

					ginkgo.By("Creating a new pod with same volume")
					pod2, err := createPodWithPVC(pvc)
					gomega.Expect(pod2).NotTo(gomega.BeNil(), "while creating pod for csi resizing")
					framework.ExpectNoError(err, "while recreating pod for resizing")

					checkPVCSize()
				}
			})
		}
	})
	ginkgo.Context("CSI online volume expansion", func() {
		tests := []struct {
			name          string
			disableAttach bool
		}{
			{
				name: "should expand volume without restarting pod if attach=on, nodeExpansion=on",
			},
			{
				name:          "should expand volume without restarting pod if attach=off, nodeExpansion=on",
				disableAttach: true,
			},
		}
		for _, t := range tests {
			test := t
			ginkgo.It(test.name, func() {
				var err error
				params := testParameters{enableResizing: true, enableNodeExpansion: true}
				if test.disableAttach {
					params.disableAttach = true
					params.registerDriver = true
				}

				init(params)

				defer cleanup()

				sc, pvc, pod := createPod(false)
				gomega.Expect(pod).NotTo(gomega.BeNil(), "while creating pod for resizing")

				framework.ExpectEqual(*sc.AllowVolumeExpansion, true, "failed creating sc with allowed expansion")

				err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
				framework.ExpectNoError(err, "Failed to start pod1: %v", err)

				ginkgo.By("Expanding current pvc")
				newSize := resource.MustParse("6Gi")
				newPVC, err := testsuites.ExpandPVCSize(pvc, newSize, m.cs)
				framework.ExpectNoError(err, "While updating pvc for more size")
				pvc = newPVC
				gomega.Expect(pvc).NotTo(gomega.BeNil())

				pvcSize := pvc.Spec.Resources.Requests[v1.ResourceStorage]
				if pvcSize.Cmp(newSize) != 0 {
					framework.Failf("error updating pvc size %q", pvc.Name)
				}

				ginkgo.By("Waiting for persistent volume resize to finish")
				err = testsuites.WaitForControllerVolumeResize(pvc, m.cs, csiResizeWaitPeriod)
				framework.ExpectNoError(err, "While waiting for PV resize to finish")

				ginkgo.By("Waiting for PVC resize to finish")
				pvc, err = testsuites.WaitForFSResize(pvc, m.cs)
				framework.ExpectNoError(err, "while waiting for PVC to finish")

				pvcConditions := pvc.Status.Conditions
				framework.ExpectEqual(len(pvcConditions), 0, "pvc should not have conditions")

			})
		}
	})

	ginkgo.Context("CSI NodeStage error cases [Slow]", func() {
		// Global variable in all scripts (called before each test)
		globalScript := `counter=0; console.log("globals loaded", OK, INVALIDARGUMENT)`
		trackedCalls := []string{
			"NodeStageVolume",
			"NodeUnstageVolume",
		}

		tests := []struct {
			name              string
			expectPodRunning  bool
			expectedCalls     []csiCall
			nodeStageScript   string
			nodeUnstageScript string
		}{
			{
				// This is already tested elsewhere, adding simple good case here to test the test framework.
				name:             "should call NodeUnstage after NodeStage success",
				expectPodRunning: true,
				expectedCalls: []csiCall{
					{expectedMethod: "NodeStageVolume", expectedError: codes.OK, deletePod: true},
					{expectedMethod: "NodeUnstageVolume", expectedError: codes.OK},
				},
				nodeStageScript: `OK;`,
			},
			{
				// Kubelet should repeat NodeStage as long as the pod exists
				name:             "should retry NodeStage after NodeStage final error",
				expectPodRunning: true,
				expectedCalls: []csiCall{
					// This matches all 3 NodeStage calls with InvalidArgument error
					{expectedMethod: "NodeStageVolume", expectedError: codes.InvalidArgument},
					{expectedMethod: "NodeStageVolume", expectedError: codes.OK, deletePod: true},
					{expectedMethod: "NodeUnstageVolume", expectedError: codes.OK},
				},
				// Fail first 3 NodeStage requests, 4th succeeds
				nodeStageScript: `console.log("Counter:", ++counter); if (counter < 4) { INVALIDARGUMENT; } else { OK; }`,
			},
			{
				// Kubelet should repeat NodeStage as long as the pod exists
				name:             "should retry NodeStage after NodeStage ephemeral error",
				expectPodRunning: true,
				expectedCalls: []csiCall{
					// This matches all 3 NodeStage calls with DeadlineExceeded error
					{expectedMethod: "NodeStageVolume", expectedError: codes.DeadlineExceeded},
					{expectedMethod: "NodeStageVolume", expectedError: codes.OK, deletePod: true},
					{expectedMethod: "NodeUnstageVolume", expectedError: codes.OK},
				},
				// Fail first 3 NodeStage requests, 4th succeeds
				nodeStageScript: `console.log("Counter:", ++counter); if (counter < 4) { DEADLINEEXCEEDED; } else { OK; }`,
			},
			{
				// After NodeUnstage with ephemeral error, the driver may continue staging the volume.
				// Kubelet should call NodeUnstage to make sure the volume is really unstaged after
				// the pod is deleted.
				name:             "should call NodeUnstage after NodeStage ephemeral error",
				expectPodRunning: false,
				expectedCalls: []csiCall{
					// Delete the pod before NodeStage succeeds - it should get "uncertain" because of ephemeral error
					// This matches all repeated NodeStage calls with DeadlineExceeded error (due to exp. backoff).
					{expectedMethod: "NodeStageVolume", expectedError: codes.DeadlineExceeded, deletePod: true},
					{expectedMethod: "NodeUnstageVolume", expectedError: codes.OK},
				},
				nodeStageScript: `DEADLINEEXCEEDED;`,
			},
			{
				// After NodeUnstage with final error, kubelet can be sure the volume is not staged.
				// The test checks that NodeUnstage is *not* called.
				name:             "should not call NodeUnstage after NodeStage final error",
				expectPodRunning: false,
				expectedCalls: []csiCall{
					// Delete the pod before NodeStage succeeds - it should get "globally unmounted" because of final error.
					// This matches all repeated NodeStage calls with InvalidArgument error (due to exp. backoff).
					{expectedMethod: "NodeStageVolume", expectedError: codes.InvalidArgument, deletePod: true},
				},
				nodeStageScript: `INVALIDARGUMENT;`,
			},
		}
		for _, t := range tests {
			test := t
			ginkgo.It(test.name, func() {
				scripts := map[string]string{
					"globals":                globalScript,
					"nodeStageVolumeStart":   test.nodeStageScript,
					"nodeUnstageVolumeStart": test.nodeUnstageScript,
				}
				init(testParameters{
					disableAttach:   true,
					registerDriver:  true,
					javascriptHooks: scripts,
				})
				defer cleanup()

				_, claim, pod := createPod(false)
				if pod == nil {
					return
				}
				// Wait for PVC to get bound to make sure the CSI driver is fully started.
				err := e2epv.WaitForPersistentVolumeClaimPhase(v1.ClaimBound, f.ClientSet, f.Namespace.Name, claim.Name, time.Second, framework.ClaimProvisionTimeout)
				framework.ExpectNoError(err, "while waiting for PVC to get provisioned")

				ginkgo.By("Waiting for expected CSI calls")
				// Watch for all calls up to deletePod = true
				ctx, cancel := context.WithTimeout(context.Background(), csiPodRunningTimeout)
				defer cancel()
				for {
					if ctx.Err() != nil {
						framework.Failf("timed out waiting for the CSI call that indicates that the pod can be deleted: %v", test.expectedCalls)
					}
					time.Sleep(1 * time.Second)
					_, index, err := compareCSICalls(trackedCalls, test.expectedCalls, m.cs, m.config.DriverNamespace.Name, driverPodName, driverContainerName)
					framework.ExpectNoError(err, "while waiting for initial CSI calls")
					if index == 0 {
						// No CSI call received yet
						continue
					}
					// Check the last *received* call wanted the pod to be deleted
					if test.expectedCalls[index-1].deletePod {
						break
					}
				}

				if test.expectPodRunning {
					ginkgo.By("Waiting for pod to be running")
					err := e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
					framework.ExpectNoError(err, "Failed to start pod: %v", err)
				}

				ginkgo.By("Deleting the previously created pod")
				err = e2epod.DeletePodWithWait(m.cs, pod)
				framework.ExpectNoError(err, "while deleting")

				ginkgo.By("Waiting for all remaining expected CSI calls")
				err = wait.Poll(time.Second, csiUnstageWaitTimeout, func() (done bool, err error) {
					_, index, err := compareCSICalls(trackedCalls, test.expectedCalls, m.cs, m.config.DriverNamespace.Name, driverPodName, driverContainerName)
					if err != nil {
						return true, err
					}
					if index == 0 {
						// No CSI call received yet
						return false, nil
					}
					if len(test.expectedCalls) == index {
						// all calls received
						return true, nil
					}
					return false, nil
				})
				framework.ExpectNoError(err, "while waiting for all CSI calls")
			})
		}
	})

	ginkgo.Context("storage capacity", func() {
		tests := []struct {
			name              string
			resourceExhausted bool
			lateBinding       bool
			topology          bool
		}{
			{
				name: "unlimited",
			},
			{
				name:              "exhausted, immediate binding",
				resourceExhausted: true,
			},
			{
				name:              "exhausted, late binding, no topology",
				resourceExhausted: true,
				lateBinding:       true,
			},
			{
				name:              "exhausted, late binding, with topology",
				resourceExhausted: true,
				lateBinding:       true,
				topology:          true,
			},
		}

		createVolume := "CreateVolume"
		deleteVolume := "DeleteVolume"
		// publishVolume := "NodePublishVolume"
		unpublishVolume := "NodeUnpublishVolume"
		// stageVolume := "NodeStageVolume"
		unstageVolume := "NodeUnstageVolume"

		// These calls are assumed to occur in this order for
		// each test run. NodeStageVolume and
		// NodePublishVolume should also be deterministic and
		// only get called once, but sometimes kubelet calls
		// both multiple times, which breaks this test
		// (https://github.com/kubernetes/kubernetes/issues/90250).
		// Therefore they are temporarily commented out until
		// that issue is resolved.
		deterministicCalls := []string{
			createVolume,
			// stageVolume,
			// publishVolume,
			unpublishVolume,
			unstageVolume,
			deleteVolume,
		}

		for _, t := range tests {
			test := t
			ginkgo.It(test.name, func() {
				var err error
				params := testParameters{
					lateBinding:    test.lateBinding,
					enableTopology: test.topology,

					// Not strictly necessary, but runs a bit faster this way
					// and for a while there also was a problem with a two minuted delay
					// due to a bug (https://github.com/kubernetes-csi/csi-test/pull/250).
					disableAttach:  true,
					registerDriver: true,
				}

				if test.resourceExhausted {
					params.javascriptHooks = map[string]string{
						"globals": `counter=0; console.log("globals loaded", OK, INVALIDARGUMENT)`,
						// Every second call returns RESOURCEEXHAUSTED, starting with the first one.
						"createVolumeStart": `console.log("Counter:", ++counter); if (counter % 2) { RESOURCEEXHAUSTED; } else { OK; }`,
					}
				}

				init(params)
				defer cleanup()

				ctx, cancel := context.WithTimeout(context.Background(), csiPodRunningTimeout)
				defer cancel()

				// In contrast to the raw watch, RetryWatcher is expected to deliver all events even
				// when the underlying raw watch gets closed prematurely
				// (https://github.com/kubernetes/kubernetes/pull/93777#discussion_r467932080).
				// This is important because below the test is going to make assertions about the
				// PVC state changes.
				initResource, err := f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).List(ctx, metav1.ListOptions{})
				framework.ExpectNoError(err, "Failed to fetch initial PVC resource")
				listWatcher := &cachetools.ListWatch{
					WatchFunc: func(listOptions metav1.ListOptions) (watch.Interface, error) {
						return f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Watch(ctx, listOptions)
					},
				}
				pvcWatch, err := watchtools.NewRetryWatcher(initResource.GetResourceVersion(), listWatcher)
				framework.ExpectNoError(err, "create PVC watch")
				defer pvcWatch.Stop()

				sc, claim, pod := createPod(false)
				gomega.Expect(pod).NotTo(gomega.BeNil(), "while creating pod")
				bindingMode := storagev1.VolumeBindingImmediate
				if test.lateBinding {
					bindingMode = storagev1.VolumeBindingWaitForFirstConsumer
				}
				framework.ExpectEqual(*sc.VolumeBindingMode, bindingMode, "volume binding mode")

				err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
				framework.ExpectNoError(err, "failed to start pod")
				err = e2epod.DeletePodWithWait(m.cs, pod)
				framework.ExpectNoError(err, "failed to delete pod")
				err = m.cs.CoreV1().PersistentVolumeClaims(claim.Namespace).Delete(ctx, claim.Name, metav1.DeleteOptions{})
				framework.ExpectNoError(err, "failed to delete claim")

				normal := []csiCall{}
				for _, method := range deterministicCalls {
					normal = append(normal, csiCall{expectedMethod: method})
				}
				expected := normal
				// When simulating limited capacity,
				// we expect exactly two CreateVolume
				// calls because the first one should
				// have failed.
				if test.resourceExhausted {
					expected = []csiCall{
						{expectedMethod: createVolume, expectedError: codes.ResourceExhausted},
					}
					expected = append(expected, normal...)
				}

				var calls []mockCSICall
				err = wait.PollImmediateUntil(time.Second, func() (done bool, err error) {
					c, index, err := compareCSICalls(deterministicCalls, expected, m.cs, m.config.DriverNamespace.Name, driverPodName, driverContainerName)
					if err != nil {
						return true, fmt.Errorf("error waiting for expected CSI calls: %s", err)
					}
					calls = c
					if index == 0 {
						// No CSI call received yet
						return false, nil
					}
					if len(expected) == index {
						// all calls received
						return true, nil
					}
					return false, nil
				}, ctx.Done())
				framework.ExpectNoError(err, "while waiting for all CSI calls")

				// The capacity error is dealt with in two different ways.
				//
				// For delayed binding, the external-provisioner should unset the node annotation
				// to give the scheduler the opportunity to reschedule the pod onto a different
				// node.
				//
				// For immediate binding, the external-scheduler must keep retrying.
				//
				// Unfortunately, the call log is the same in both cases. We have to collect
				// additional evidence that rescheduling really happened. What we have observed
				// above is how the PVC changed over time. Now we can analyze that.
				ginkgo.By("Checking PVC events")
				nodeAnnotationSet := false
				nodeAnnotationReset := false
				watchFailed := false
			loop:
				for {
					select {
					case event, ok := <-pvcWatch.ResultChan():
						if !ok {
							watchFailed = true
							break loop
						}

						framework.Logf("PVC event %s: %#v", event.Type, event.Object)
						switch event.Type {
						case watch.Modified:
							pvc, ok := event.Object.(*v1.PersistentVolumeClaim)
							if !ok {
								framework.Failf("PVC watch sent %#v instead of a PVC", event.Object)
							}
							_, set := pvc.Annotations["volume.kubernetes.io/selected-node"]
							if set {
								nodeAnnotationSet = true
							} else if nodeAnnotationSet {
								nodeAnnotationReset = true
							}
						case watch.Deleted:
							break loop
						case watch.Error:
							watchFailed = true
							break
						}
					case <-ctx.Done():
						framework.Failf("Timeout while waiting to observe PVC list")
					}
				}

				// More tests when capacity is limited.
				if test.resourceExhausted {
					for _, call := range calls {
						if call.Method == createVolume {
							gomega.Expect(call.Error).To(gomega.ContainSubstring("code = ResourceExhausted"), "first CreateVolume error in\n%s", calls)
							break
						}
					}

					switch {
					case watchFailed:
						// If the watch failed or stopped prematurely (which can happen at any time), then we cannot
						// verify whether the annotation was set as expected. This is still considered a successful
						// test.
						framework.Logf("PVC watch delivered incomplete data, cannot check annotation")
					case test.lateBinding:
						gomega.Expect(nodeAnnotationSet).To(gomega.BeTrue(), "selected-node should have been set")
						// Whether it gets reset depends on whether we have topology enabled. Without
						// it, rescheduling is unnecessary.
						if test.topology {
							gomega.Expect(nodeAnnotationReset).To(gomega.BeTrue(), "selected-node should have been set")
						} else {
							gomega.Expect(nodeAnnotationReset).To(gomega.BeFalse(), "selected-node should not have been reset")
						}
					default:
						gomega.Expect(nodeAnnotationSet).To(gomega.BeFalse(), "selected-node should not have been set")
						gomega.Expect(nodeAnnotationReset).To(gomega.BeFalse(), "selected-node should not have been reset")
					}
				}
			})
		}
	})

	// These tests *only* work on a cluster which has the CSIStorageCapacity feature enabled.
	ginkgo.Context("CSIStorageCapacity [Feature:CSIStorageCapacity]", func() {
		var (
			err error
			yes = true
			no  = false
		)
		// Tests that expect a failure are slow because we have to wait for a while
		// to be sure that the volume isn't getting created.
		tests := []struct {
			name            string
			storageCapacity *bool
			capacities      []string
			expectFailure   bool
		}{
			{
				name: "CSIStorageCapacity unused",
			},
			{
				name:            "CSIStorageCapacity disabled",
				storageCapacity: &no,
			},
			{
				name:            "CSIStorageCapacity used, no capacity",
				storageCapacity: &yes,
				expectFailure:   true,
			},
			{
				name:            "CSIStorageCapacity used, insufficient capacity",
				storageCapacity: &yes,
				expectFailure:   true,
				capacities:      []string{"1Mi"},
			},
			{
				name:            "CSIStorageCapacity used, have capacity",
				storageCapacity: &yes,
				capacities:      []string{"100Gi"},
			},
			// We could add more test cases here for
			// various situations, but covering those via
			// the scheduler binder unit tests is faster.
		}
		for _, t := range tests {
			test := t
			ginkgo.It(t.name, func() {
				scName := "mock-csi-storage-capacity-" + f.UniqueName
				init(testParameters{
					registerDriver:  true,
					scName:          scName,
					storageCapacity: test.storageCapacity,
					lateBinding:     true,
				})
				defer cleanup()

				// The storage class uses a random name, therefore we have to create it first
				// before adding CSIStorageCapacity objects for it.
				for _, capacityStr := range test.capacities {
					capacityQuantity := resource.MustParse(capacityStr)
					capacity := &storagev1alpha1.CSIStorageCapacity{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "fake-capacity-",
						},
						// Empty topology, usable by any node.
						StorageClassName: scName,
						NodeTopology:     &metav1.LabelSelector{},
						Capacity:         &capacityQuantity,
					}
					createdCapacity, err := f.ClientSet.StorageV1alpha1().CSIStorageCapacities(f.Namespace.Name).Create(context.Background(), capacity, metav1.CreateOptions{})
					framework.ExpectNoError(err, "create CSIStorageCapacity %+v", *capacity)
					m.testCleanups = append(m.testCleanups, func() {
						f.ClientSet.StorageV1alpha1().CSIStorageCapacities(f.Namespace.Name).Delete(context.Background(), createdCapacity.Name, metav1.DeleteOptions{})
					})
				}

				// kube-scheduler may need some time before it gets the CSIDriver and CSIStorageCapacity objects.
				// Without them, scheduling doesn't run as expected by the test.
				syncDelay := 5 * time.Second
				time.Sleep(syncDelay)

				sc, _, pod := createPod(false /* persistent volume, late binding as specified above */)
				framework.ExpectEqual(sc.Name, scName, "pre-selected storage class name not used")

				waitCtx, cancel := context.WithTimeout(context.Background(), podStartTimeout)
				defer cancel()
				condition := anyOf(
					podRunning(waitCtx, f.ClientSet, pod.Name, pod.Namespace),
					// We only just created the CSIStorageCapacity objects, therefore
					// we have to ignore all older events, plus the syncDelay as our
					// safety margin.
					podHasStorage(waitCtx, f.ClientSet, pod.Name, pod.Namespace, time.Now().Add(syncDelay)),
				)
				err = wait.PollImmediateUntil(poll, condition, waitCtx.Done())
				if test.expectFailure {
					switch {
					case errors.Is(err, context.DeadlineExceeded),
						errors.Is(err, wait.ErrWaitTimeout),
						errors.Is(err, errNotEnoughSpace):
						// Okay, we expected that.
					case err == nil:
						framework.Fail("pod unexpectedly started to run")
					default:
						framework.Failf("unexpected error while waiting for pod: %v", err)
					}
				} else {
					framework.ExpectNoError(err, "failed to start pod")
				}

				ginkgo.By("Deleting the previously created pod")
				err = e2epod.DeletePodWithWait(m.cs, pod)
				framework.ExpectNoError(err, "while deleting")
			})
		}
	})

	ginkgo.Context("CSI Volume Snapshots [Feature:VolumeSnapshotDataSource]", func() {
		// Global variable in all scripts (called before each test)
		globalScript := `counter=0; console.log("globals loaded", OK, DEADLINEEXCEEDED)`
		tests := []struct {
			name                 string
			createVolumeScript   string
			createSnapshotScript string
		}{
			{
				name:                 "volumesnapshotcontent and pvc in Bound state with deletion timestamp set should not get deleted while snapshot finalizer exists",
				createVolumeScript:   `OK`,
				createSnapshotScript: `console.log("Counter:", ++counter); if (counter < 8) { DEADLINEEXCEEDED; } else { OK; }`,
			},
		}
		for _, test := range tests {
			ginkgo.It(test.name, func() {
				scripts := map[string]string{
					"globals":             globalScript,
					"createVolumeStart":   test.createVolumeScript,
					"createSnapshotStart": test.createSnapshotScript,
				}
				init(testParameters{
					disableAttach:   true,
					registerDriver:  true,
					enableSnapshot:  true,
					javascriptHooks: scripts,
				})
				sDriver, ok := m.driver.(testsuites.SnapshottableTestDriver)
				if !ok {
					e2eskipper.Skipf("mock driver %s does not support snapshots -- skipping", m.driver.GetDriverInfo().Name)

				}
				ctx, cancel := context.WithTimeout(context.Background(), csiPodRunningTimeout)
				defer cancel()
				defer cleanup()

				var sc *storagev1.StorageClass
				if dDriver, ok := m.driver.(testsuites.DynamicPVTestDriver); ok {
					sc = dDriver.GetDynamicProvisionStorageClass(m.config, "")
				}
				ginkgo.By("Creating storage class")
				class, err := m.cs.StorageV1().StorageClasses().Create(context.TODO(), sc, metav1.CreateOptions{})
				framework.ExpectNoError(err, "Failed to create class: %v", err)
				m.sc[class.Name] = class
				claim := e2epv.MakePersistentVolumeClaim(e2epv.PersistentVolumeClaimConfig{
					// Use static name so that the volumesnapshot can be created before the pvc.
					Name:             "snapshot-test-pvc",
					StorageClassName: &(class.Name),
				}, f.Namespace.Name)

				ginkgo.By("Creating snapshot")
				// TODO: Test VolumeSnapshots with Retain policy
				snapshotClass, snapshot := testsuites.CreateSnapshot(sDriver, m.config, testpatterns.DynamicSnapshotDelete, claim.Name, claim.Namespace)
				framework.ExpectNoError(err, "failed to create snapshot")
				m.vsc[snapshotClass.GetName()] = snapshotClass
				volumeSnapshotName := snapshot.GetName()

				ginkgo.By(fmt.Sprintf("Creating PVC %s/%s", claim.Namespace, claim.Name))
				claim, err = m.cs.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(context.TODO(), claim, metav1.CreateOptions{})
				framework.ExpectNoError(err, "Failed to create claim: %v", err)

				ginkgo.By(fmt.Sprintf("Wait for finalizer to be added to claim %s/%s", claim.Namespace, claim.Name))
				err = e2epv.WaitForPVCFinalizer(ctx, m.cs, claim.Name, claim.Namespace, pvcAsSourceProtectionFinalizer, 1*time.Millisecond, 1*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By("Wait for PVC to be Bound")
				_, err = e2epv.WaitForPVClaimBoundPhase(m.cs, []*v1.PersistentVolumeClaim{claim}, 1*time.Minute)
				framework.ExpectNoError(err, "Failed to create claim: %v", err)

				ginkgo.By(fmt.Sprintf("Delete PVC %s", claim.Name))
				err = e2epv.DeletePersistentVolumeClaim(m.cs, claim.Name, claim.Namespace)
				framework.ExpectNoError(err, "failed to delete pvc")

				ginkgo.By("Get PVC from API server and verify deletion timestamp is set")
				claim, err = m.cs.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Get(context.TODO(), claim.Name, metav1.GetOptions{})
				if err != nil {
					if !apierrors.IsNotFound(err) {
						framework.ExpectNoError(err, "Failed to get claim: %v", err)
					}
					framework.Logf("PVC not found. Continuing to test VolumeSnapshotContent finalizer")
				}
				if claim != nil && claim.DeletionTimestamp == nil {
					framework.Failf("Expected deletion timestamp to be set on PVC %s", claim.Name)
				}

				ginkgo.By(fmt.Sprintf("Get VolumeSnapshotContent bound to VolumeSnapshot %s", snapshot.GetName()))
				snapshotContent := testsuites.GetSnapshotContentFromSnapshot(m.config.Framework.DynamicClient, snapshot)
				volumeSnapshotContentName := snapshotContent.GetName()

				ginkgo.By(fmt.Sprintf("Verify VolumeSnapshotContent %s contains finalizer %s", snapshot.GetName(), volumeSnapshotContentFinalizer))
				err = utils.WaitForGVRFinalizer(ctx, m.config.Framework.DynamicClient, testsuites.SnapshotContentGVR, volumeSnapshotContentName, "", volumeSnapshotContentFinalizer, 1*time.Millisecond, 1*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By(fmt.Sprintf("Delete VolumeSnapshotContent %s", snapshotContent.GetName()))
				err = m.config.Framework.DynamicClient.Resource(testsuites.SnapshotContentGVR).Delete(ctx, snapshotContent.GetName(), metav1.DeleteOptions{})
				framework.ExpectNoError(err, "Failed to delete snapshotcontent: %v", err)

				ginkgo.By("Get VolumeSnapshotContent from API server and verify deletion timestamp is set")
				snapshotContent, err = m.config.Framework.DynamicClient.Resource(testsuites.SnapshotContentGVR).Get(context.TODO(), snapshotContent.GetName(), metav1.GetOptions{})
				framework.ExpectNoError(err)

				if snapshotContent.GetDeletionTimestamp() == nil {
					framework.Failf("Expected deletion timestamp to be set on snapshotcontent")
				}

				if claim != nil {
					ginkgo.By(fmt.Sprintf("Wait for PV %s to be deleted", claim.Spec.VolumeName))
					err = e2epv.WaitForPersistentVolumeDeleted(m.cs, claim.Spec.VolumeName, framework.Poll, 3*time.Minute)
					framework.ExpectNoError(err, fmt.Sprintf("failed to delete PV %s", claim.Spec.VolumeName))
				}

				ginkgo.By(fmt.Sprintf("Verify VolumeSnapshot %s contains finalizer %s", snapshot.GetName(), volumeSnapshotBoundFinalizer))
				err = utils.WaitForGVRFinalizer(ctx, m.config.Framework.DynamicClient, testsuites.SnapshotGVR, volumeSnapshotName, f.Namespace.Name, volumeSnapshotBoundFinalizer, 1*time.Millisecond, 1*time.Minute)
				framework.ExpectNoError(err)

				ginkgo.By("Delete VolumeSnapshot")
				err = testsuites.DeleteAndWaitSnapshot(m.config.Framework.DynamicClient, f.Namespace.Name, volumeSnapshotName, framework.Poll, framework.SnapshotDeleteTimeout)
				framework.ExpectNoError(err, fmt.Sprintf("failed to delete VolumeSnapshot %s", volumeSnapshotName))

				ginkgo.By(fmt.Sprintf("Wait for VolumeSnapshotContent %s to be deleted", volumeSnapshotContentName))
				err = utils.WaitForGVRDeletion(m.config.Framework.DynamicClient, testsuites.SnapshotContentGVR, volumeSnapshotContentName, framework.Poll, framework.SnapshotDeleteTimeout)
				framework.ExpectNoError(err, fmt.Sprintf("failed to delete VolumeSnapshotContent %s", volumeSnapshotContentName))
			})
		}
	})

	ginkgo.Context("CSIServiceAccountToken [Feature:CSIServiceAccountToken]", func() {
		var (
			err error
		)
		tests := []struct {
			desc                  string
			deployCSIDriverObject bool
			tokenRequests         []storagev1.TokenRequest
		}{
			{
				desc:                  "token should not be plumbed down when csiServiceAccountTokenEnabled=false",
				deployCSIDriverObject: true,
				tokenRequests:         nil,
			},
			{
				desc:                  "token should not be plumbed down when CSIDriver is not deployed",
				deployCSIDriverObject: false,
				tokenRequests:         []storagev1.TokenRequest{{}},
			},
			{
				desc:                  "token should be plumbed down when csiServiceAccountTokenEnabled=true",
				deployCSIDriverObject: true,
				tokenRequests:         []storagev1.TokenRequest{{ExpirationSeconds: utilptr.Int64Ptr(60 * 10)}},
			},
		}
		for _, test := range tests {
			test := test
			csiServiceAccountTokenEnabled := test.tokenRequests != nil
			ginkgo.It(test.desc, func() {
				init(testParameters{
					registerDriver:    test.deployCSIDriverObject,
					tokenRequests:     test.tokenRequests,
					requiresRepublish: &csiServiceAccountTokenEnabled,
				})

				defer cleanup()

				_, _, pod := createPod(false)
				if pod == nil {
					return
				}
				err = e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
				framework.ExpectNoError(err, "Failed to start pod: %v", err)

				// sleep to make sure RequiresRepublish triggers more than 1 NodePublishVolume
				numNodePublishVolume := 1
				if test.deployCSIDriverObject && csiServiceAccountTokenEnabled {
					time.Sleep(time.Second)
					numNodePublishVolume = 2
				}

				ginkgo.By("Deleting the previously created pod")
				err = e2epod.DeletePodWithWait(m.cs, pod)
				framework.ExpectNoError(err, "while deleting")

				ginkgo.By("Checking CSI driver logs")
				err = checkPodLogs(m.cs, m.config.DriverNamespace.Name, driverPodName, driverContainerName, pod, false, false, false, test.deployCSIDriverObject && csiServiceAccountTokenEnabled, numNodePublishVolume)
				framework.ExpectNoError(err)
			})
		}
	})
	// These tests *only* work on a cluster which has the CSIVolumeFSGroupPolicy feature enabled.
	ginkgo.Context("CSI FSGroupPolicy [LinuxOnly]", func() {
		tests := []struct {
			name          string
			fsGroupPolicy storagev1.FSGroupPolicy
			modified      bool
		}{
			{
				name:          "should modify fsGroup if fsGroupPolicy=default",
				fsGroupPolicy: storagev1.ReadWriteOnceWithFSTypeFSGroupPolicy,
				modified:      true,
			},
			{
				name:          "should modify fsGroup if fsGroupPolicy=File",
				fsGroupPolicy: storagev1.FileFSGroupPolicy,
				modified:      true,
			},
			{
				name:          "should not modify fsGroup if fsGroupPolicy=None",
				fsGroupPolicy: storagev1.NoneFSGroupPolicy,
				modified:      false,
			},
		}
		for _, t := range tests {
			test := t
			ginkgo.It(test.name, func() {
				if framework.NodeOSDistroIs("windows") {
					e2eskipper.Skipf("FSGroupPolicy is only applied on linux nodes -- skipping")
				}
				init(testParameters{
					disableAttach:  true,
					registerDriver: true,
					fsGroupPolicy:  &test.fsGroupPolicy,
				})
				defer cleanup()

				// kube-scheduler may need some time before it gets the CSIDriver object.
				// Without them, scheduling doesn't run as expected by the test.
				syncDelay := 5 * time.Second
				time.Sleep(syncDelay)

				fsGroupVal := int64(rand.Int63n(20000) + 1024)
				fsGroup := &fsGroupVal

				_, _, pod := createPodWithFSGroup(fsGroup) /* persistent volume */

				mountPath := pod.Spec.Containers[0].VolumeMounts[0].MountPath
				dirName := mountPath + "/" + f.UniqueName
				fileName := dirName + "/" + f.UniqueName

				err := e2epod.WaitForPodNameRunningInNamespace(m.cs, pod.Name, pod.Namespace)
				framework.ExpectNoError(err, "failed to start pod")

				// Create the subdirectory to ensure that fsGroup propagates
				createDirectory := fmt.Sprintf("mkdir %s", dirName)
				_, _, err = utils.PodExec(f, pod, createDirectory)
				framework.ExpectNoError(err, "failed: creating the directory: %s", err)

				// Inject the contents onto the mount
				createFile := fmt.Sprintf("echo '%s' > '%s'; sync", "filecontents", fileName)
				_, _, err = utils.PodExec(f, pod, createFile)
				framework.ExpectNoError(err, "failed: writing the contents: %s", err)

				// Delete the created file. This step is mandatory, as the mock driver
				// won't clean up the contents automatically.
				defer func() {
					delete := fmt.Sprintf("rm -fr %s", dirName)
					_, _, err = utils.PodExec(f, pod, delete)
					framework.ExpectNoError(err, "failed: deleting the directory: %s", err)
				}()

				// Ensure that the fsGroup matches what we expect
				if test.modified {
					utils.VerifyFSGroupInPod(f, fileName, strconv.FormatInt(*fsGroup, 10), pod)
				} else {
					utils.VerifyFSGroupInPod(f, fileName, "root", pod)
				}

				// The created resources will be removed by the cleanup() function,
				// so need to delete anything here.
			})
		}
	})
})

// A lot of this code was copied from e2e/framework. It would be nicer
// if it could be reused - see https://github.com/kubernetes/kubernetes/issues/92754
func podRunning(ctx context.Context, c clientset.Interface, podName, namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		pod, err := c.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch pod.Status.Phase {
		case v1.PodRunning:
			return true, nil
		case v1.PodFailed, v1.PodSucceeded:
			return false, errPodCompleted
		}
		return false, nil
	}
}

const (
	podStartTimeout                = 5 * time.Minute
	poll                           = 2 * time.Second
	pvcAsSourceProtectionFinalizer = "snapshot.storage.kubernetes.io/pvc-as-source-protection"
	volumeSnapshotContentFinalizer = "snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"
	volumeSnapshotBoundFinalizer   = "snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"
)

var (
	errPodCompleted   = fmt.Errorf("pod ran to completion")
	errNotEnoughSpace = errors.New(scheduling.ErrReasonNotEnoughSpace)
)

func podHasStorage(ctx context.Context, c clientset.Interface, podName, namespace string, when time.Time) wait.ConditionFunc {
	// Check for events of this pod. Copied from test/e2e/common/container_probe.go.
	expectedEvent := fields.Set{
		"involvedObject.kind":      "Pod",
		"involvedObject.name":      podName,
		"involvedObject.namespace": namespace,
		"reason":                   "FailedScheduling",
	}.AsSelector().String()
	options := metav1.ListOptions{
		FieldSelector: expectedEvent,
	}
	// copied from test/e2e/framework/events/events.go
	return func() (bool, error) {
		// We cannot be sure here whether it has enough storage, only when
		// it hasn't. In that case we abort waiting with a special error.
		events, err := c.CoreV1().Events(namespace).List(ctx, options)
		if err != nil {
			return false, fmt.Errorf("got error while getting events: %w", err)
		}
		for _, event := range events.Items {
			if /* event.CreationTimestamp.After(when) &&
			 */strings.Contains(event.Message, scheduling.ErrReasonNotEnoughSpace) {
				return false, errNotEnoughSpace
			}
		}
		return false, nil
	}
}

func anyOf(conditions ...wait.ConditionFunc) wait.ConditionFunc {
	return func() (bool, error) {
		for _, condition := range conditions {
			done, err := condition()
			if err != nil {
				return false, err
			}
			if done {
				return true, nil
			}
		}
		return false, nil
	}
}

func waitForMaxVolumeCondition(pod *v1.Pod, cs clientset.Interface) error {
	waitErr := wait.PollImmediate(10*time.Second, csiPodUnschedulableTimeout, func() (bool, error) {
		pod, err := cs.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, c := range pod.Status.Conditions {
			// Conformance tests cannot rely on specific output of optional fields (e.g., Reason
			// and Message) because these fields are not suject to the deprecation policy.
			if c.Type == v1.PodScheduled && c.Status == v1.ConditionFalse && c.Reason != "" && c.Message != "" {
				return true, nil
			}
		}
		return false, nil
	})
	if waitErr != nil {
		return fmt.Errorf("error waiting for pod %s/%s to have max volume condition: %v", pod.Namespace, pod.Name, waitErr)
	}
	return nil
}

func waitForVolumeAttachmentTerminated(attachmentName string, cs clientset.Interface) error {
	waitErr := wait.PollImmediate(10*time.Second, csiVolumeAttachmentTimeout, func() (bool, error) {
		_, err := cs.StorageV1().VolumeAttachments().Get(context.TODO(), attachmentName, metav1.GetOptions{})
		if err != nil {
			// if the volumeattachment object is not found, it means it has been terminated.
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
	if waitErr != nil {
		return fmt.Errorf("error waiting volume attachment %v to terminate: %v", attachmentName, waitErr)
	}
	return nil
}

func checkCSINodeForLimits(nodeName string, driverName string, cs clientset.Interface) (int32, error) {
	var attachLimit int32

	waitErr := wait.PollImmediate(10*time.Second, csiNodeLimitUpdateTimeout, func() (bool, error) {
		csiNode, err := cs.StorageV1().CSINodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		attachLimit = getVolumeLimitFromCSINode(csiNode, driverName)
		if attachLimit > 0 {
			return true, nil
		}
		return false, nil
	})
	if waitErr != nil {
		return 0, fmt.Errorf("error waiting for non-zero volume limit of driver %s on node %s: %v", driverName, nodeName, waitErr)
	}
	return attachLimit, nil
}

func createClaim(cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, scName, ns string) (*storagev1.StorageClass, *v1.PersistentVolumeClaim) {
	class := newStorageClass(t, ns, "")
	if scName != "" {
		class.Name = scName
	}
	var err error
	_, err = cs.StorageV1().StorageClasses().Get(context.TODO(), class.Name, metav1.GetOptions{})
	if err != nil {
		class, err = cs.StorageV1().StorageClasses().Create(context.TODO(), class, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Failed to create class: %v", err)
	}

	claim := e2epv.MakePersistentVolumeClaim(e2epv.PersistentVolumeClaimConfig{
		ClaimSize:        t.ClaimSize,
		StorageClassName: &(class.Name),
		VolumeMode:       &t.VolumeMode,
	}, ns)
	claim, err = cs.CoreV1().PersistentVolumeClaims(ns).Create(context.TODO(), claim, metav1.CreateOptions{})
	framework.ExpectNoError(err, "Failed to create claim: %v", err)

	if !t.DelayBinding {
		pvcClaims := []*v1.PersistentVolumeClaim{claim}
		_, err = e2epv.WaitForPVClaimBoundPhase(cs, pvcClaims, framework.ClaimProvisionTimeout)
		framework.ExpectNoError(err, "Failed waiting for PVC to be bound: %v", err)
	}
	return class, claim
}

func startPausePod(cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, scName, ns string) (*storagev1.StorageClass, *v1.PersistentVolumeClaim, *v1.Pod) {
	class, claim := createClaim(cs, t, node, scName, ns)

	pod, err := startPausePodWithClaim(cs, claim, node, ns)
	framework.ExpectNoError(err, "Failed to create pause pod: %v", err)
	return class, claim, pod
}

func startBusyBoxPod(cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, scName, ns string, fsGroup *int64) (*storagev1.StorageClass, *v1.PersistentVolumeClaim, *v1.Pod) {
	class, claim := createClaim(cs, t, node, scName, ns)
	pod, err := startBusyBoxPodWithClaim(cs, claim, node, ns, fsGroup)
	framework.ExpectNoError(err, "Failed to create busybox pod: %v", err)
	return class, claim, pod
}

func startPausePodInline(cs clientset.Interface, t testsuites.StorageClassTest, node e2epod.NodeSelection, ns string) *v1.Pod {
	pod, err := startPausePodWithInlineVolume(cs,
		&v1.CSIVolumeSource{
			Driver: t.Provisioner,
		},
		node, ns)
	framework.ExpectNoError(err, "Failed to create pod: %v", err)
	return pod
}

func startPausePodWithClaim(cs clientset.Interface, pvc *v1.PersistentVolumeClaim, node e2epod.NodeSelection, ns string) (*v1.Pod, error) {
	return startPausePodWithVolumeSource(cs,
		v1.VolumeSource{
			PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc.Name,
				ReadOnly:  false,
			},
		},
		node, ns)
}

func startBusyBoxPodWithClaim(cs clientset.Interface, pvc *v1.PersistentVolumeClaim, node e2epod.NodeSelection, ns string, fsGroup *int64) (*v1.Pod, error) {
	return startBusyBoxPodWithVolumeSource(cs,
		v1.VolumeSource{
			PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc.Name,
				ReadOnly:  false,
			},
		},
		node, ns, fsGroup)
}

func startPausePodWithInlineVolume(cs clientset.Interface, inlineVolume *v1.CSIVolumeSource, node e2epod.NodeSelection, ns string) (*v1.Pod, error) {
	return startPausePodWithVolumeSource(cs,
		v1.VolumeSource{
			CSI: inlineVolume,
		},
		node, ns)
}

func startPausePodWithVolumeSource(cs clientset.Interface, volumeSource v1.VolumeSource, node e2epod.NodeSelection, ns string) (*v1.Pod, error) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-volume-tester-",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "volume-tester",
					Image: imageutils.GetE2EImage(imageutils.Pause),
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "my-volume",
							MountPath: "/mnt/test",
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name:         "my-volume",
					VolumeSource: volumeSource,
				},
			},
		},
	}
	e2epod.SetNodeSelection(&pod.Spec, node)
	return cs.CoreV1().Pods(ns).Create(context.TODO(), pod, metav1.CreateOptions{})
}

func startBusyBoxPodWithVolumeSource(cs clientset.Interface, volumeSource v1.VolumeSource, node e2epod.NodeSelection, ns string, fsGroup *int64) (*v1.Pod, error) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-volume-tester-",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "volume-tester",
					Image: framework.BusyBoxImage,
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "my-volume",
							MountPath: "/mnt/test",
						},
					},
					Command: e2evolume.GenerateScriptCmd("while true ; do sleep 2; done"),
				},
			},
			SecurityContext: &v1.PodSecurityContext{
				FSGroup: fsGroup,
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name:         "my-volume",
					VolumeSource: volumeSource,
				},
			},
		},
	}
	e2epod.SetNodeSelection(&pod.Spec, node)
	return cs.CoreV1().Pods(ns).Create(context.TODO(), pod, metav1.CreateOptions{})
}

// Dummy structure that parses just volume_attributes and error code out of logged CSI call
type mockCSICall struct {
	json string // full log entry

	Method  string
	Request struct {
		VolumeContext map[string]string `json:"volume_context"`
	}
	FullError struct {
		Code    codes.Code `json:"code"`
		Message string     `json:"message"`
	}
	Error string
}

// checkPodLogs tests that NodePublish was called with expected volume_context and (for ephemeral inline volumes)
// has the matching NodeUnpublish
func checkPodLogs(cs clientset.Interface, namespace, driverPodName, driverContainerName string, pod *v1.Pod, expectPodInfo, ephemeralVolume, csiInlineVolumesEnabled, csiServiceAccountTokenEnabled bool, expectedNumNodePublish int) error {
	expectedAttributes := map[string]string{}
	if expectPodInfo {
		expectedAttributes["csi.storage.k8s.io/pod.name"] = pod.Name
		expectedAttributes["csi.storage.k8s.io/pod.namespace"] = pod.Namespace
		expectedAttributes["csi.storage.k8s.io/pod.uid"] = string(pod.UID)
		expectedAttributes["csi.storage.k8s.io/serviceAccount.name"] = "default"

	}
	if csiInlineVolumesEnabled {
		// This is only passed in 1.15 when the CSIInlineVolume feature gate is set.
		expectedAttributes["csi.storage.k8s.io/ephemeral"] = strconv.FormatBool(ephemeralVolume)
	}

	if csiServiceAccountTokenEnabled {
		expectedAttributes["csi.storage.k8s.io/serviceAccount.tokens"] = "<nonempty>"
	}

	// Find NodePublish in the GRPC calls.
	foundAttributes := sets.NewString()
	numNodePublishVolume := 0
	numNodeUnpublishVolume := 0
	calls, err := parseMockLogs(cs, namespace, driverPodName, driverContainerName)
	if err != nil {
		return err
	}
	for _, call := range calls {
		switch call.Method {
		case "NodePublishVolume":
			numNodePublishVolume++
			if numNodePublishVolume == 1 {
				// Check that NodePublish had expected attributes for first volume
				for k, v := range expectedAttributes {
					vv, found := call.Request.VolumeContext[k]
					if found && (v == vv || (v == "<nonempty>" && len(vv) != 0)) {
						foundAttributes.Insert(k)
						framework.Logf("Found volume attribute %s: %s", k, vv)
					}
				}
			}
		case "NodeUnpublishVolume":
			framework.Logf("Found NodeUnpublishVolume: %+v", call)
			numNodeUnpublishVolume++
		}
	}
	if numNodePublishVolume < expectedNumNodePublish {
		return fmt.Errorf("NodePublish should be called at least %d", expectedNumNodePublish)
	}

	if numNodeUnpublishVolume == 0 {
		return fmt.Errorf("NodeUnpublish was never called")
	}
	if foundAttributes.Len() != len(expectedAttributes) {
		return fmt.Errorf("number of found volume attributes does not match, expected %d, got %d", len(expectedAttributes), foundAttributes.Len())
	}
	return nil
}

func parseMockLogs(cs clientset.Interface, namespace, driverPodName, driverContainerName string) ([]mockCSICall, error) {
	// Load logs of driver pod
	log, err := e2epod.GetPodLogs(cs, namespace, driverPodName, driverContainerName)
	if err != nil {
		return nil, fmt.Errorf("could not load CSI driver logs: %s", err)
	}

	logLines := strings.Split(log, "\n")
	var calls []mockCSICall
	for _, line := range logLines {
		index := strings.Index(line, grpcCallPrefix)
		if index == -1 {
			continue
		}
		line = line[index+len(grpcCallPrefix):]
		call := mockCSICall{
			json: string(line),
		}
		err := json.Unmarshal([]byte(line), &call)
		if err != nil {
			framework.Logf("Could not parse CSI driver log line %q: %s", line, err)
			continue
		}

		// Trim gRPC service name, i.e. "/csi.v1.Identity/Probe" -> "Probe"
		methodParts := strings.Split(call.Method, "/")
		call.Method = methodParts[len(methodParts)-1]

		calls = append(calls, call)
	}
	return calls, nil
}

// compareCSICalls compares expectedCalls with logs of the mock driver.
// It returns index of the first expectedCall that was *not* received
// yet or error when calls do not match.
// All repeated calls to the CSI mock driver (e.g. due to exponential backoff)
// are squashed and checked against single expectedCallSequence item.
//
// Only permanent errors are returned. Other errors are logged and no
// calls are returned. The caller is expected to retry.
func compareCSICalls(trackedCalls []string, expectedCallSequence []csiCall, cs clientset.Interface, namespace, driverPodName, driverContainerName string) ([]mockCSICall, int, error) {
	allCalls, err := parseMockLogs(cs, namespace, driverPodName, driverContainerName)
	if err != nil {
		framework.Logf("intermittent (?) log retrieval error, proceeding without output: %v", err)
		return nil, 0, nil
	}

	// Remove all repeated and ignored calls
	tracked := sets.NewString(trackedCalls...)
	var calls []mockCSICall
	var last mockCSICall
	for _, c := range allCalls {
		if !tracked.Has(c.Method) {
			continue
		}
		if c.Method != last.Method || c.FullError.Code != last.FullError.Code {
			last = c
			calls = append(calls, c)
		}
		// This call is the same as the last one, ignore it.
	}

	for i, c := range calls {
		if i >= len(expectedCallSequence) {
			// Log all unexpected calls first, return error below outside the loop.
			framework.Logf("Unexpected CSI driver call: %s (%d)", c.Method, c.FullError)
			continue
		}

		// Compare current call with expected call
		expectedCall := expectedCallSequence[i]
		if c.Method != expectedCall.expectedMethod || c.FullError.Code != expectedCall.expectedError {
			return allCalls, i, fmt.Errorf("Unexpected CSI call %d: expected %s (%d), got %s (%d)", i, expectedCall.expectedMethod, expectedCall.expectedError, c.Method, c.FullError.Code)
		}
	}
	if len(calls) > len(expectedCallSequence) {
		return allCalls, len(expectedCallSequence), fmt.Errorf("Received %d unexpected CSI driver calls", len(calls)-len(expectedCallSequence))
	}
	// All calls were correct
	return allCalls, len(calls), nil

}

func waitForCSIDriver(cs clientset.Interface, driverName string) error {
	timeout := 4 * time.Minute

	framework.Logf("waiting up to %v for CSIDriver %q", timeout, driverName)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(framework.Poll) {
		_, err := cs.StorageV1().CSIDrivers().Get(context.TODO(), driverName, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return fmt.Errorf("gave up after waiting %v for CSIDriver %q", timeout, driverName)
}

func destroyCSIDriver(cs clientset.Interface, driverName string) {
	driverGet, err := cs.StorageV1().CSIDrivers().Get(context.TODO(), driverName, metav1.GetOptions{})
	if err == nil {
		framework.Logf("deleting %s.%s: %s", driverGet.TypeMeta.APIVersion, driverGet.TypeMeta.Kind, driverGet.ObjectMeta.Name)
		// Uncomment the following line to get full dump of CSIDriver object
		// framework.Logf("%s", framework.PrettyPrint(driverGet))
		cs.StorageV1().CSIDrivers().Delete(context.TODO(), driverName, metav1.DeleteOptions{})
	}
}

func getVolumeHandle(cs clientset.Interface, claim *v1.PersistentVolumeClaim) string {
	// re-get the claim to the latest state with bound volume
	claim, err := cs.CoreV1().PersistentVolumeClaims(claim.Namespace).Get(context.TODO(), claim.Name, metav1.GetOptions{})
	if err != nil {
		framework.ExpectNoError(err, "Cannot get PVC")
		return ""
	}
	pvName := claim.Spec.VolumeName
	pv, err := cs.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err != nil {
		framework.ExpectNoError(err, "Cannot get PV")
		return ""
	}
	if pv.Spec.CSI == nil {
		gomega.Expect(pv.Spec.CSI).NotTo(gomega.BeNil())
		return ""
	}
	return pv.Spec.CSI.VolumeHandle
}

func getVolumeLimitFromCSINode(csiNode *storagev1.CSINode, driverName string) int32 {
	for _, d := range csiNode.Spec.Drivers {
		if d.Name != driverName {
			continue
		}
		if d.Allocatable != nil && d.Allocatable.Count != nil {
			return *d.Allocatable.Count
		}
	}
	return 0
}
