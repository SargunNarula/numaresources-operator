/*
 * Copyright 2022 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/k8stopologyawareschedwg/deployer/pkg/clientutil/nodes"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	corev1qos "k8s.io/kubectl/pkg/util/qos"

	nrtv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2/helper/attribute"

	"github.com/openshift-kni/numaresources-operator/internal/podlist"
	e2ereslist "github.com/openshift-kni/numaresources-operator/internal/resourcelist"
	"github.com/openshift-kni/numaresources-operator/internal/wait"

	"github.com/openshift-kni/numaresources-operator/internal/baseload"
	intnrt "github.com/openshift-kni/numaresources-operator/internal/noderesourcetopology"
	"github.com/openshift-kni/numaresources-operator/test/e2e/label"
	serialconfig "github.com/openshift-kni/numaresources-operator/test/e2e/serial/config"
	e2efixture "github.com/openshift-kni/numaresources-operator/test/internal/fixture"
	"github.com/openshift-kni/numaresources-operator/test/internal/images"
	e2enrt "github.com/openshift-kni/numaresources-operator/test/internal/noderesourcetopologies"
	"github.com/openshift-kni/numaresources-operator/test/internal/nrosched"
	"github.com/openshift-kni/numaresources-operator/test/internal/objects"
	e2epadder "github.com/openshift-kni/numaresources-operator/test/internal/padder"
)

var _ = Describe("[serial][disruptive][scheduler] numaresources workload placement", Serial, Label("disruptive", "scheduler"), Label("feature:nonreg"), func() {
	var fxt *e2efixture.Fixture
	var padder *e2epadder.Padder
	var nrtList nrtv1alpha2.NodeResourceTopologyList
	var nrts []nrtv1alpha2.NodeResourceTopology
	tmSingleNUMANodeFuncsHandler := tmScopeFuncsHandler{
		intnrt.Pod:       newPodScopeSingleNUMANodeFuncs(),
		intnrt.Container: newContainerScopeSingleNUMANodeFuncs(),
	}

	BeforeEach(func() {
		Expect(serialconfig.Config).ToNot(BeNil())
		Expect(serialconfig.Config.Ready()).To(BeTrue(), "NUMA fixture initialization failed")

		var err error
		fxt, err = e2efixture.Setup("e2e-test-non-regression", serialconfig.Config.NRTList)
		Expect(err).ToNot(HaveOccurred(), "unable to setup test fixture")

		padder, err = e2epadder.New(fxt.Client, fxt.Namespace.Name)
		Expect(err).ToNot(HaveOccurred())

		err = fxt.Client.List(context.TODO(), &nrtList)
		Expect(err).ToNot(HaveOccurred())

		// so we can't support ATM zones > 2. HW with zones > 2 is rare anyway, so not to big of a deal now.
		By(fmt.Sprintf("filtering available nodes with at least %d NUMA zones", 2))
		nrtCandidates := e2enrt.FilterZoneCountEqual(nrtList.Items, 2)
		if len(nrtCandidates) < 2 {
			e2efixture.Skipf(fxt, "not enough nodes with 2 NUMA Zones: found %d", len(nrtCandidates))
		}
		klog.Infof("Found node with 2 NUMA zones: %d", len(nrtCandidates))

		// we're ok with any TM policy as long as the updater can handle it,
		// we use this as proxy for "there is valid NRT data for at least X nodes
		nrts = e2enrt.FilterByTopologyManagerPolicy(nrtCandidates, intnrt.SingleNUMANode)
		if len(nrts) < 2 {
			e2efixture.Skipf(fxt, "not enough nodes with valid policy - found %d", len(nrts))
		}
		klog.Infof("Found node with 2 NUMA zones: %d", len(nrts))

		// Note that this test, being part of "serial", expects NO OTHER POD being scheduled
		// in between, so we consider this information current and valid when the It()s run.
	})

	AfterEach(func() {
		err := padder.Clean()
		Expect(err).NotTo(HaveOccurred())
		err = e2efixture.Teardown(fxt)
		Expect(err).NotTo(HaveOccurred())
	})

	// note we hardcode the values we need here and when we pad node.
	// This is ugly, but automatically computing the values is not straightforward
	// and will we want to start lean and mean.

	Context("cluster has at least one suitable node", func() {
		timeout := 5 * time.Minute
		// will be called at the end of the test to make sure we're not polluting the cluster
		var cleanFuncs []func() error

		BeforeEach(func() {
			numOfNodeToBePadded := len(nrts) - 1

			rl := corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4G"),
			}
			By("padding the nodes before test start")
			labSel, err := labels.Parse(serialconfig.MultiNUMALabel + "=2")
			Expect(err).ToNot(HaveOccurred())

			err = padder.Nodes(numOfNodeToBePadded).UntilAvailableIsResourceListPerZone(rl).Pad(timeout, e2epadder.PaddingOptions{
				LabelSelector: labSel,
			})
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			By("unpadding the nodes after test finish")
			err := padder.Clean()
			Expect(err).ToNot(HaveOccurred())

			for _, f := range cleanFuncs {
				err := f()
				Expect(err).ToNot(HaveOccurred())
			}
		})

		It("[test_id:47584] should be able to schedule guaranteed pod in selective way", Label(label.Tier2), func() {
			nodesNameSet := e2enrt.AccumulateNames(nrts)
			targetNodeName, ok := e2efixture.PopNodeName(nodesNameSet)
			Expect(ok).To(BeTrue())

			var nrtInitial nrtv1alpha2.NodeResourceTopology
			err := fxt.Client.Get(context.TODO(), client.ObjectKey{Name: targetNodeName}, &nrtInitial)
			Expect(err).ToNot(HaveOccurred())

			testPod := objects.NewTestPodPause(fxt.Namespace.Name, "testpod")
			pSpec := &testPod.Spec

			By(fmt.Sprintf("explicitly mentioning which node we want the pod to land on: %q", targetNodeName))
			pSpec.NodeName = targetNodeName

			By("setting a fake schedule name under the pod to make sure pod not scheduled by any scheduler")
			noneExistingSchedulerName := "foo"
			testPod.Spec.SchedulerName = noneExistingSchedulerName

			cnt := &testPod.Spec.Containers[0]
			requiredRes := corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			}
			cnt.Resources.Requests = requiredRes
			cnt.Resources.Limits = requiredRes

			By(fmt.Sprintf("creating pod %s/%s", testPod.Namespace, testPod.Name))
			err = fxt.Client.Create(context.TODO(), testPod)
			Expect(err).ToNot(HaveOccurred())

			updatedPod, err := wait.With(fxt.Client).Timeout(timeout).ForPodPhase(context.TODO(), testPod.Namespace, testPod.Name, corev1.PodRunning)
			if err != nil {
				_ = objects.LogEventsForPod(fxt.K8sClient, updatedPod.Namespace, updatedPod.Name)
			}
			Expect(err).ToNot(HaveOccurred())

			By(fmt.Sprintf("checking the pod landed on the target node %q vs %q", updatedPod.Spec.NodeName, targetNodeName))
			Expect(updatedPod.Spec.NodeName).To(Equal(targetNodeName),
				"node landed on %q instead of on %v", updatedPod.Spec.NodeName, targetNodeName)

			By("Waiting for the NRT data to stabilize")
			e2efixture.MustSettleNRT(fxt)

			By(fmt.Sprintf("checking NRT for target node %q updated correctly", targetNodeName))
			rl := e2ereslist.FromGuaranteedPod(*updatedPod)
			nrtPostCreate := expectNRTConsumedResources(fxt, nrtInitial, rl, updatedPod)

			By("deleting the pod")
			err = fxt.Client.Delete(context.TODO(), updatedPod)
			Expect(err).ToNot(HaveOccurred())

			// the NRT updaters MAY be slow to react for a number of reasons including factors out of our control
			// (kubelet, runtime). This is a known behavior. We can only tolerate some delay in reporting on pod removal.
			By(fmt.Sprintf("checking the resources are restored as expected on %q", targetNodeName))
			nrtPostDelete, err := e2enrt.GetUpdatedForNode(fxt.Client, context.TODO(), nrtPostCreate, 1*time.Minute)
			Expect(err).ToNot(HaveOccurred())
			ok, err = e2enrt.CheckEqualAvailableResources(nrtInitial, nrtPostDelete)
			Expect(err).ToNot(HaveOccurred())
			Expect(ok).To(BeTrue(), "NRT resources not restored correctly on %q", targetNodeName)
		})

		It("[test_id:48964] should be able to schedule a guaranteed deployment pod to a specific node", Label(label.Tier3), func() {
			nrtInitialList := nrtv1alpha2.NodeResourceTopologyList{}

			err := fxt.Client.List(context.TODO(), &nrtInitialList)
			Expect(err).ToNot(HaveOccurred())

			workers, err := nodes.GetWorkers(fxt.DEnv())
			Expect(err).ToNot(HaveOccurred())

			targetIdx, ok := e2efixture.PickNodeIndex(workers)
			Expect(ok).To(BeTrue())
			targetNodeName := workers[targetIdx].Name

			By(fmt.Sprintf("explicitly specfying the node where the pod should land: %q. Scheduler Name does not matter in this case", targetNodeName))
			nonExistingSchedulerName := "foo"

			var replicas int32 = 1
			podLabels := map[string]string{
				"test": "test-dp",
			}

			// the pod is asking for 2 CPUS and 100Mi
			requiredRes := corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			}

			podSpec := &corev1.PodSpec{
				// the SchedulerName specified here does not matter here as the pod is pinned to a node. See below.
				SchedulerName: nonExistingSchedulerName,
				Containers: []corev1.Container{
					{
						Name:    "testdp-cnt",
						Image:   images.GetPauseImage(),
						Command: []string{images.PauseCommand},
						Resources: corev1.ResourceRequirements{
							Limits:   requiredRes,
							Requests: requiredRes,
						},
					},
				},
				// pod is explicitly pinned to a node
				NodeName:      targetNodeName,
				RestartPolicy: corev1.RestartPolicyAlways,
			}

			By(fmt.Sprintf("creating a deployment with a guaranteed pod with two containers requiring total %s", e2ereslist.ToString(e2ereslist.FromContainerLimits(podSpec.Containers))))
			dp := objects.NewTestDeploymentWithPodSpec(replicas, podLabels, nil, fxt.Namespace.Name, "testdp", *podSpec)

			err = fxt.Client.Create(context.TODO(), dp)
			Expect(err).ToNot(HaveOccurred())

			updatedDp, err := wait.With(fxt.Client).Interval(10*time.Second).Timeout(time.Minute).ForDeploymentComplete(context.TODO(), dp)
			Expect(err).ToNot(HaveOccurred())

			nrtPostCreateDeploymentList, err := e2enrt.GetUpdated(fxt.Client, nrtInitialList, time.Minute)
			Expect(err).ToNot(HaveOccurred())

			pods, err := podlist.With(fxt.Client).ByDeployment(context.TODO(), *updatedDp)
			Expect(err).ToNot(HaveOccurred())
			Expect(pods).To(HaveLen(1))

			updatedPod := pods[0]

			By(fmt.Sprintf("checking the pod landed on the target node %q vs %q", updatedPod.Spec.NodeName, targetNodeName))
			Expect(updatedPod.Spec.NodeName).To(Equal(targetNodeName),
				"node landed on %q instead of on %v", updatedPod.Spec.NodeName, targetNodeName)

			By(fmt.Sprintf("checking the pod was assigned to a specific node and not scheduled by a scheduler %q", nonExistingSchedulerName))
			schedOK, err := nrosched.CheckPODWasScheduledWith(fxt.K8sClient, updatedPod.Namespace, updatedPod.Name, nonExistingSchedulerName)
			Expect(err).ToNot(HaveOccurred())
			Expect(schedOK).To(BeFalse(), "pod %s/%s not assigned to a specific node without a scheduler %s", updatedPod.Namespace, updatedPod.Name, nonExistingSchedulerName)

			rl := e2ereslist.FromGuaranteedPod(updatedPod)
			klog.Infof("post-create pod resource list: spec=[%s] updated=[%s]", e2ereslist.ToString(e2ereslist.FromContainerLimits(podSpec.Containers)), e2ereslist.ToString(rl))

			nrtInitial, err := e2enrt.FindFromList(nrtInitialList.Items, updatedPod.Spec.NodeName)
			Expect(err).ToNot(HaveOccurred())

			By("wait for NRT data to settle")
			e2efixture.MustSettleNRT(fxt)

			nrtPostCreate, err := e2enrt.FindFromList(nrtPostCreateDeploymentList.Items, updatedPod.Spec.NodeName)
			Expect(err).ToNot(HaveOccurred())

			scope, ok := attribute.Get(nrtInitial.Attributes, intnrt.TopologyManagerScopeAttribute)
			Expect(ok).To(BeTrue(), fmt.Sprintf("Unable to get required attribute %q from NRT %q", intnrt.TopologyManagerScopeAttribute, nrtInitial.Name))

			policyFuncs := tmSingleNUMANodeFuncsHandler[scope.Value]

			By(fmt.Sprintf("checking post-create NRT for target node %q updated correctly", targetNodeName))
			dataBefore, err := yaml.Marshal(nrtInitial)
			Expect(err).ToNot(HaveOccurred())
			dataAfter, err := yaml.Marshal(nrtPostCreate)
			Expect(err).ToNot(HaveOccurred())
			match, err := policyFuncs.checkConsumedRes(*nrtInitial, *nrtPostCreate, rl, corev1qos.GetPodQOS(&updatedPod))
			Expect(err).ToNot(HaveOccurred())
			Expect(match).ToNot(BeEmpty(), "inconsistent accounting: no resources consumed by the running pod,\nNRT before test's pod: %s \nNRT after: %s \n total required resources: %s", dataBefore, dataAfter, e2ereslist.ToString(rl))

			By("deleting the deployment")
			err = fxt.Client.Delete(context.TODO(), updatedDp)
			Expect(err).ToNot(HaveOccurred())

			// the NRT updaters MAY be slow to react for a number of reasons including factors out of our control
			// (kubelet, runtime). This is a known behavior. We can only tolerate some delay in reporting on pod removal.
			Eventually(func() bool {
				By(fmt.Sprintf("checking the resources are restored as expected on %q", updatedPod.Spec.NodeName))

				nrtListPostDelete, err := e2enrt.GetUpdated(fxt.Client, nrtPostCreateDeploymentList, 1*time.Minute)
				Expect(err).ToNot(HaveOccurred())

				nrtPostDelete, err := e2enrt.FindFromList(nrtListPostDelete.Items, updatedPod.Spec.NodeName)
				Expect(err).ToNot(HaveOccurred())

				ok, err := e2enrt.CheckEqualAvailableResources(*nrtInitial, *nrtPostDelete)
				Expect(err).ToNot(HaveOccurred())
				return ok
			}, time.Minute, time.Second*5).Should(BeTrue(), "resources not restored on %q", updatedPod.Spec.NodeName)
		})
	})

	Context("Requesting resources that are greater than allocatable at numa level", func() {
		It("[test_id:47613] should not schedule a pod requesting resources that are not allocatable at numa level", Label(label.Tier3, "nonreg", "unsched"), Label("feature:unsched"), func() {
			//the test can run on node with any numa number, so no need to filter the nrts
			nrtNames := e2enrt.AccumulateNames(nrts)

			//select target node
			targetNodeName, ok := e2efixture.PopNodeName(nrtNames)
			Expect(ok).To(BeTrue(), "cannot select a node among %#v", e2efixture.ListNodeNames(nrtNames))
			By(fmt.Sprintf("selecting node to schedule the test pod: %q", targetNodeName))

			//pad non target nodes
			By("padding non target nodes leaving room for the baseload only")
			var paddingPods []*corev1.Pod
			for _, nodeName := range e2efixture.ListNodeNames(nrtNames) {

				//calculate base load on the node
				baseload, err := baseload.ForNode(fxt.Client, context.TODO(), nodeName)
				Expect(err).ToNot(HaveOccurred(), "missing node load info for %q", nodeName)
				klog.Infof("computed base load: %s", baseload)

				//get nrt info of the node
				klog.Infof("preparing node %q to fit the test case", nodeName)
				nrtInfo, err := e2enrt.FindFromList(nrts, nodeName)
				Expect(err).ToNot(HaveOccurred(), "missing NRT info for %q", nodeName)

				paddingRes, err := e2enrt.SaturateNodeUntilLeft(*nrtInfo, baseload.Resources)
				Expect(err).ToNot(HaveOccurred(), "could not get padding resources for node %q", nrtInfo.Name)

				for _, zone := range nrtInfo.Zones {
					By(fmt.Sprintf("fully padding node %q zone %q ", nrtInfo.Name, zone.Name))
					padPod := newPaddingPod(nrtInfo.Name, zone.Name, fxt.Namespace.Name, paddingRes[zone.Name])

					padPod, err = pinPodTo(padPod, nrtInfo.Name, zone.Name)
					Expect(err).ToNot(HaveOccurred(), "unable to pin pod %q to zone %q", padPod.Name, zone.Name)

					err = fxt.Client.Create(context.TODO(), padPod)
					Expect(err).ToNot(HaveOccurred())
					paddingPods = append(paddingPods, padPod)
				}
			}

			By("Waiting for padding pods to be ready")
			failedPodIds := e2efixture.WaitForPaddingPodsRunning(fxt, paddingPods)
			Expect(failedPodIds).To(BeEmpty(), "some padding pods have failed to run")

			By("waiting for the NRT data to settle")
			e2efixture.MustSettleNRT(fxt)

			//prepare the test pod
			//get maximum allocatable resources across all numas of the target node
			var targetNrtListInitial nrtv1alpha2.NodeResourceTopologyList
			err := fxt.Client.List(context.TODO(), &targetNrtListInitial)
			Expect(err).ToNot(HaveOccurred())
			targetNrtInitial, err := e2enrt.FindFromList(targetNrtListInitial.Items, targetNodeName)
			Expect(err).NotTo(HaveOccurred())

			requiredRes := corev1.ResourceList{
				corev1.ResourceCPU:    e2enrt.GetMaxAllocatableResourceNumaLevel(*targetNrtInitial, corev1.ResourceCPU),
				corev1.ResourceMemory: e2enrt.GetMaxAllocatableResourceNumaLevel(*targetNrtInitial, corev1.ResourceMemory),
			}
			//add minimal amount of cpu and memory to the max allocatable resources to ensure the requested amount is greater than allocatable would afford
			minRes := corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			}
			e2ereslist.AddCoreResources(requiredRes, minRes)

			pod := objects.NewTestPodPause(fxt.Namespace.Name, "testpod")
			pod.Spec.SchedulerName = serialconfig.Config.SchedulerName
			pod.Spec.Containers[0].Resources.Limits = requiredRes

			By(fmt.Sprintf("Scheduling the testing pod with resources that are not allocatable on any numa zone of the target node. requested resources of the test pod: %s", e2ereslist.ToString(e2ereslist.FromContainerLimits(pod.Spec.Containers))))
			err = fxt.Client.Create(context.TODO(), pod)
			Expect(err).NotTo(HaveOccurred(), "unable to create pod %q", pod.Name)

			By("check the pod keeps on pending")
			_, err = wait.With(fxt.Client).Interval(10*time.Second).Steps(3).WhileInPodPhase(context.TODO(), pod.Namespace, pod.Name, corev1.PodPending)
			if err != nil {
				_ = objects.LogEventsForPod(fxt.K8sClient, pod.Namespace, pod.Name)
			}
			Expect(err).ToNot(HaveOccurred())

			By("wait for NRT data to settle")
			e2efixture.MustSettleNRT(fxt)

			By("check the NRT has no changes")
			nrtListPostPodCreate, err := e2enrt.GetUpdated(fxt.Client, targetNrtListInitial, 1*time.Minute)
			Expect(err).ToNot(HaveOccurred())

			nrtPostCreate, err := e2enrt.FindFromList(nrtListPostPodCreate.Items, targetNodeName)
			Expect(err).ToNot(HaveOccurred())

			isEqual, err := e2enrt.CheckEqualAvailableResources(*targetNrtInitial, *nrtPostCreate)
			Expect(err).ToNot(HaveOccurred())
			Expect(isEqual).To(BeTrue(), "new changes were detected in the nrt of the target node, but expected no change")

		})
	})
})
