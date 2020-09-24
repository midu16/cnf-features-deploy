package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"

	netattdefv1 "github.com/openshift/sriov-network-operator/pkg/apis/k8s/v1"
	sriovv1 "github.com/openshift/sriov-network-operator/pkg/apis/sriovnetwork/v1"
	"github.com/openshift/sriov-network-operator/test/util/cluster"
	"github.com/openshift/sriov-network-operator/test/util/discovery"
	"github.com/openshift/sriov-network-operator/test/util/execute"

	"github.com/openshift/sriov-network-operator/test/util/namespaces"
	"github.com/openshift/sriov-network-operator/test/util/network"
	"github.com/openshift/sriov-network-operator/test/util/nodes"
	"github.com/openshift/sriov-network-operator/test/util/pod"
	corev1 "k8s.io/api/core/v1"
	k8sv1 "k8s.io/api/core/v1"
	v1core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	admission "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var waitingTime time.Duration = 20 * time.Minute
var sriovNetworkName = "test-sriovnetwork"

const (
	operatorNetworkInjectorFlag = "network-resources-injector"
	operatorWebhookFlag         = "operator-webhook"
)

type patchBody struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

func init() {
	waitingEnv := os.Getenv("SRIOV_WAITING_TIME")
	newTime, err := strconv.Atoi(waitingEnv)
	if err == nil && newTime != 0 {
		waitingTime = time.Duration(newTime) * time.Minute
	}
}

var _ = Describe("[sriov] operator", func() {
	var sriovInfos *cluster.EnabledNodes
	var initPassed bool
	execute.BeforeAll(func() {
		Expect(clients).NotTo(BeNil(), "Client misconfigured, check the $KUBECONFIG env variable")
		err := namespaces.Create(namespaces.Test, clients)
		Expect(err).ToNot(HaveOccurred())

		waitForSRIOVStable()
		sriovInfos, err = cluster.DiscoverSriov(clients, operatorNamespace)
		Expect(err).ToNot(HaveOccurred())
		initPassed = true
	})

	BeforeEach(func() {
		Expect(initPassed).To(BeTrue(), "Global setup failed")
	})

	Describe("No SriovNetworkNodePolicy", func() {
		Context("SR-IOV network config daemon can be set by nodeselector", func() {
			// 26186
			It("Should schedule the config daemon on selected nodes", func() {
				if discovery.Enabled() {
					Skip("Test unsuitable to be run in discovery mode")
				}

				By("Checking that a daemon is scheduled on each worker node")
				Eventually(func() bool {
					return daemonsScheduledOnNodes("node-role.kubernetes.io/worker=")
				}, 3*time.Minute, 1*time.Second).Should(Equal(true))

				By("Labelling one worker node with the label needed for the daemon")
				allNodes, err := clients.Nodes().List(context.Background(), metav1.ListOptions{
					LabelSelector: "node-role.kubernetes.io/worker",
				})
				Expect(err).ToNot(HaveOccurred())

				selectedNodes, err := nodes.MatchingOptionalSelector(clients, allNodes.Items)
				Expect(err).ToNot(HaveOccurred())

				Expect(len(selectedNodes)).To(BeNumerically(">", 0), "There must be at least one worker")
				candidate := selectedNodes[0]
				candidate.Labels["sriovenabled"] = "true"
				_, err = clients.Nodes().Update(context.Background(), &candidate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Setting the node selector for each daemon")
				cfg := sriovv1.SriovOperatorConfig{}
				err = clients.Get(context.TODO(), runtimeclient.ObjectKey{
					Name:      "default",
					Namespace: operatorNamespace,
				}, &cfg)
				Expect(err).ToNot(HaveOccurred())
				cfg.Spec.ConfigDaemonNodeSelector = map[string]string{
					"sriovenabled": "true",
				}
				err = clients.Update(context.TODO(), &cfg)
				Expect(err).ToNot(HaveOccurred())

				By("Checking that a daemon is scheduled only on selected node")
				Eventually(func() bool {
					return !daemonsScheduledOnNodes("sriovenabled!=true") &&
						daemonsScheduledOnNodes("sriovenabled=true")
				}, 1*time.Minute, 1*time.Second).Should(Equal(true))

				By("Restoring the node selector for daemons")
				err = clients.Get(context.TODO(), runtimeclient.ObjectKey{
					Name:      "default",
					Namespace: operatorNamespace,
				}, &cfg)
				Expect(err).ToNot(HaveOccurred())
				cfg.Spec.ConfigDaemonNodeSelector = map[string]string{}
				err = clients.Update(context.TODO(), &cfg)
				Expect(err).ToNot(HaveOccurred())

				By("Checking that a daemon is scheduled on each worker node")
				Eventually(func() bool {
					return daemonsScheduledOnNodes("node-role.kubernetes.io/worker")
				}, 1*time.Minute, 1*time.Second).Should(Equal(true))
			})
		})

	})

	Describe("Generic SriovNetworkNodePolicy", func() {
		numVfs := 5
		resourceName := "testresource"
		var node string
		var sriovDevice *sriovv1.InterfaceExt
		var discoveryFailed bool

		execute.BeforeAll(func() {
			var err error

			if discovery.Enabled() {
				node, resourceName, numVfs, sriovDevice, err = discovery.DiscoveredResources(clients,
					sriovInfos, operatorNamespace, defaultFilterPolicy,
					func(node string, sriovDeviceList []*sriovv1.InterfaceExt) (*sriovv1.InterfaceExt, bool) {
						if len(sriovDeviceList) == 0 {
							return nil, false
						}
						return sriovDeviceList[0], true
					},
				)

				Expect(err).ToNot(HaveOccurred())
				discoveryFailed = node == "" || resourceName == "" || numVfs < 5
			} else {
				node = sriovInfos.Nodes[0]
				createVanillaNetworkPolicy(node, sriovInfos, numVfs, resourceName)
				waitForSRIOVStable()
				sriovDevice, err = sriovInfos.FindOneSriovDevice(node)
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() int64 {
					testedNode, err := clients.Nodes().Get(context.Background(), node, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					resNum, _ := testedNode.Status.Allocatable[corev1.ResourceName("openshift.io/"+resourceName)]
					allocatable, _ := resNum.AsInt64()
					return allocatable
				}, 10*time.Minute, time.Second).Should(Equal(int64(numVfs)))
			}
		})

		BeforeEach(func() {
			if discovery.Enabled() && discoveryFailed {
				Skip("Insufficient resources to run tests in discovery mode")
			}
			err := namespaces.CleanPods(namespaces.Test, clients)
			Expect(err).ToNot(HaveOccurred())
			err = namespaces.CleanNetworks(operatorNamespace, clients)
			Expect(err).ToNot(HaveOccurred())
		})
		Context("Resource Injector", func() {
			// 25815
			It("Should inject downward api volume", func() {
				sriovNetwork := &sriovv1.SriovNetwork{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-apivolnetwork",
						Namespace: operatorNamespace,
					},
					Spec: sriovv1.SriovNetworkSpec{
						ResourceName:     resourceName,
						IPAM:             `{"type":"host-local","subnet":"10.10.10.0/24","rangeStart":"10.10.10.171","rangeEnd":"10.10.10.181","routes":[{"dst":"0.0.0.0/0"}],"gateway":"10.10.10.1"}`,
						NetworkNamespace: namespaces.Test,
					}}
				err := clients.Create(context.Background(), sriovNetwork)
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: "test-apivolnetwork", Namespace: namespaces.Test}, netAttDef)
				}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				podDefinition := pod.DefineWithNetworks([]string{sriovNetwork.Name})
				created, err := clients.Pods(namespaces.Test).Create(context.Background(), podDefinition, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				var runningPod *corev1.Pod
				Eventually(func() corev1.PodPhase {
					runningPod, err = clients.Pods(namespaces.Test).Get(context.Background(), created.Name, metav1.GetOptions{})
					if errors.IsNotFound(err) {
						return corev1.PodUnknown
					}
					Expect(err).ToNot(HaveOccurred())

					return runningPod.Status.Phase
				}, 3*time.Minute, time.Second).Should(Equal(corev1.PodRunning))

				var downwardVolume *corev1.Volume
				for _, v := range runningPod.Spec.Volumes {
					if v.Name == "podnetinfo" {
						downwardVolume = v.DeepCopy()
						break
					}
				}

				Expect(downwardVolume).ToNot(BeNil(), "Downward volume not found")
				Expect(downwardVolume.DownwardAPI).ToNot(BeNil(), "Downward api not found in volume")
				Expect(downwardVolume.DownwardAPI.Items).To(SatisfyAll(
					ContainElement(corev1.DownwardAPIVolumeFile{
						Path: "labels",
						FieldRef: &corev1.ObjectFieldSelector{
							APIVersion: "v1",
							FieldPath:  "metadata.labels",
						},
					}), ContainElement(corev1.DownwardAPIVolumeFile{
						Path: "annotations",
						FieldRef: &corev1.ObjectFieldSelector{
							APIVersion: "v1",
							FieldPath:  "metadata.annotations",
						},
					})))
			})

		})

		Context("VF flags", func() {
			hostNetPod := &corev1.Pod{} // Initialized in BeforeEach
			intf := &sriovv1.InterfaceExt{}

			validationFunction := func(networks []string, containsFunc func(line string) bool) {
				podObj := pod.DefineWithNetworks(networks)
				err := clients.Create(context.Background(), podObj)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() corev1.PodPhase {
					podObj, err = clients.Pods(namespaces.Test).Get(context.Background(), podObj.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return podObj.Status.Phase
				}, 5*time.Minute, time.Second).Should(Equal(corev1.PodRunning))

				vfIndex, err := podVFIndexInHost(hostNetPod, podObj, "net1")
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() bool {
					stdout, stderr, err := pod.ExecCommand(clients, hostNetPod, "ip", "link", "show")
					Expect(err).ToNot(HaveOccurred())
					Expect(stderr).To(Equal(""))

					found := false
					for _, line := range strings.Split(stdout, "\n") {
						if strings.Contains(line, fmt.Sprintf("vf %d ", vfIndex)) && containsFunc(line) {
							found = true
							break
						}
					}
					if !found {
						return found
					}

					err = clients.Pods(namespaces.Test).Delete(context.Background(), podObj.Name, metav1.DeleteOptions{
						GracePeriodSeconds: pointer.Int64Ptr(0)})
					Expect(err).ToNot(HaveOccurred())

					return found
				}, time.Minute, time.Second).Should(BeTrue())

			}

			validateNetworkFields := func(sriovNetwork *sriovv1.SriovNetwork, validationString string) {
				netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
				Eventually(func() error {
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetwork.Name, Namespace: namespaces.Test}, netAttDef)
				}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				checkFunc := func(line string) bool {
					if strings.Contains(line, validationString) {
						return true
					}
					return false
				}

				validationFunction([]string{sriovNetwork.Name}, checkFunc)
			}

			BeforeEach(func() {
				node := sriovInfos.Nodes[0]
				Eventually(func() int64 {
					testedNode, err := clients.Nodes().Get(context.Background(), node, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					resNum, _ := testedNode.Status.Allocatable[corev1.ResourceName("openshift.io/"+resourceName)]
					allocatable, _ := resNum.AsInt64()
					return allocatable
				}, 3*time.Minute, time.Second).Should(Equal(int64(numVfs)))

				hostNetPod = pod.DefineWithHostNetwork(node)
				err := clients.Create(context.Background(), hostNetPod)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() corev1.PodPhase {
					hostNetPod, err = clients.Pods(namespaces.Test).Get(context.Background(), hostNetPod.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return hostNetPod.Status.Phase
				}, 3*time.Minute, time.Second).Should(Equal(corev1.PodRunning))
			})

			// 25959
			It("Should configure the spoofChk boolean variable", func() {
				sriovNetwork := &sriovv1.SriovNetwork{
					ObjectMeta: metav1.ObjectMeta{Name: "test-spoofnetwork", Namespace: operatorNamespace},
					Spec: sriovv1.SriovNetworkSpec{
						ResourceName: resourceName,
						IPAM: `{"type":"host-local",
								"subnet":"10.10.10.0/24",
								"rangeStart":"10.10.10.171",
								"rangeEnd":"10.10.10.181",
								"routes":[{"dst":"0.0.0.0/0"}],
								"gateway":"10.10.10.1"}`,
						NetworkNamespace: namespaces.Test,
					}}

				By("configuring spoofChk on")
				copyObj := sriovNetwork.DeepCopy()
				copyObj.Spec.SpoofChk = "on"
				spoofChkStatusValidation := "spoof checking on"
				err := clients.Create(context.Background(), copyObj)
				Expect(err).ToNot(HaveOccurred())

				validateNetworkFields(copyObj, spoofChkStatusValidation)

				By("removing sriov network")
				err = clients.Delete(context.Background(), sriovNetwork)
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() bool {
					networkDef := &sriovv1.SriovNetwork{}
					err := clients.Get(context.Background(), runtimeclient.ObjectKey{Name: "test-spoofnetwork",
						Namespace: operatorNamespace}, networkDef)
					return k8serrors.IsNotFound(err)
				}, 10*time.Second, 1*time.Second).Should(BeTrue())

				By("configuring spoofChk off")
				copyObj = sriovNetwork.DeepCopy()
				copyObj.Spec.SpoofChk = "off"
				spoofChkStatusValidation = "spoof checking off"
				err = clients.Create(context.Background(), copyObj)
				Expect(err).ToNot(HaveOccurred())

				validateNetworkFields(copyObj, spoofChkStatusValidation)
			})

			// 25960
			It("Should configure the trust boolean variable", func() {
				sriovNetwork := &sriovv1.SriovNetwork{
					ObjectMeta: metav1.ObjectMeta{Name: "test-trustnetwork", Namespace: operatorNamespace},
					Spec: sriovv1.SriovNetworkSpec{
						ResourceName: resourceName,
						IPAM: `{"type":"host-local",
								"subnet":"10.10.10.0/24",
								"rangeStart":"10.10.10.171",
								"rangeEnd":"10.10.10.181",
								"routes":[{"dst":"0.0.0.0/0"}],
								"gateway":"10.10.10.1"}`,
						NetworkNamespace: namespaces.Test,
					}}

				By("configuring trust on")
				copyObj := sriovNetwork.DeepCopy()
				copyObj.Spec.Trust = "on"
				trustChkStatusValidation := "trust on"
				err := clients.Create(context.Background(), copyObj)
				Expect(err).ToNot(HaveOccurred())

				validateNetworkFields(copyObj, trustChkStatusValidation)

				By("removing sriov network")
				err = clients.Delete(context.Background(), sriovNetwork)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() bool {
					networkDef := &sriovv1.SriovNetwork{}
					err := clients.Get(context.Background(), runtimeclient.ObjectKey{Name: "test-trustnetwork",
						Namespace: operatorNamespace}, networkDef)
					return k8serrors.IsNotFound(err)
				}, 10*time.Second, 1*time.Second).Should(BeTrue())

				By("configuring trust off")
				copyObj = sriovNetwork.DeepCopy()
				copyObj.Spec.Trust = "off"
				trustChkStatusValidation = "trust off"
				err = clients.Create(context.Background(), copyObj)
				Expect(err).ToNot(HaveOccurred())

				validateNetworkFields(copyObj, trustChkStatusValidation)
			})

			// 25961
			It("Should configure the the link state variable", func() {
				sriovNetwork := &sriovv1.SriovNetwork{
					ObjectMeta: metav1.ObjectMeta{Name: "test-statenetwork", Namespace: operatorNamespace},
					Spec: sriovv1.SriovNetworkSpec{
						ResourceName: resourceName,
						IPAM: `{"type":"host-local",
								"subnet":"10.10.10.0/24",
								"rangeStart":"10.10.10.171",
								"rangeEnd":"10.10.10.181",
								"routes":[{"dst":"0.0.0.0/0"}],
								"gateway":"10.10.10.1"}`,
						NetworkNamespace: namespaces.Test,
					}}

				By("configuring link-state as enabled")
				enabledLinkNetwork := sriovNetwork.DeepCopy()
				enabledLinkNetwork.Spec.LinkState = "enable"
				linkStateChkStatusValidation := "link-state enable"
				err := clients.Create(context.Background(), enabledLinkNetwork)
				Expect(err).ToNot(HaveOccurred())

				validateNetworkFields(enabledLinkNetwork, linkStateChkStatusValidation)

				By("removing sriov network")
				err = clients.Delete(context.Background(), enabledLinkNetwork)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() bool {
					networkDef := &sriovv1.SriovNetwork{}
					err := clients.Get(context.Background(), runtimeclient.ObjectKey{Name: "test-statenetwork",
						Namespace: operatorNamespace}, networkDef)
					return k8serrors.IsNotFound(err)
				}, 10*time.Second, 1*time.Second).Should(BeTrue())

				By("configuring link-state as disable")
				disabledLinkNetwork := sriovNetwork.DeepCopy()
				disabledLinkNetwork.Spec.LinkState = "disable"
				linkStateChkStatusValidation = "link-state disable"
				err = clients.Create(context.Background(), disabledLinkNetwork)
				Expect(err).ToNot(HaveOccurred())

				validateNetworkFields(disabledLinkNetwork, linkStateChkStatusValidation)

				By("removing sriov network")
				err = clients.Delete(context.Background(), disabledLinkNetwork)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() bool {
					networkDef := &sriovv1.SriovNetwork{}
					err := clients.Get(context.Background(), runtimeclient.ObjectKey{Name: "test-statenetwork",
						Namespace: operatorNamespace}, networkDef)
					return k8serrors.IsNotFound(err)
				}, 10*time.Second, 1*time.Second).Should(BeTrue())

				By("configuring link-state as auto")
				autoLinkNetwork := sriovNetwork.DeepCopy()
				autoLinkNetwork.Spec.LinkState = "auto"
				linkStateChkStatusValidation = "link-state auto"
				err = clients.Create(context.Background(), autoLinkNetwork)
				Expect(err).ToNot(HaveOccurred())

				validateNetworkFields(autoLinkNetwork, linkStateChkStatusValidation)
			})

			// 25963
			Describe("rate limit", func() {
				It("Should configure the requested rate limit flags under the vf", func() {
					if intf.Driver != "mlx5_core" {
						// There is an issue with the intel cards both driver i40 and ixgbe
						// BZ 1772847
						// BZ 1772815
						// BZ 1236146
						Skip("Skip rate limit test no mellanox driver found")
					}

					var maxTxRate = 100
					var minTxRate = 40
					sriovNetwork := &sriovv1.SriovNetwork{ObjectMeta: metav1.ObjectMeta{Name: "test-ratenetwork", Namespace: operatorNamespace},
						Spec: sriovv1.SriovNetworkSpec{
							ResourceName: resourceName,
							IPAM: `{"type":"host-local",
								"subnet":"10.10.10.0/24",
								"rangeStart":"10.10.10.171",
								"rangeEnd":"10.10.10.181",
								"routes":[{"dst":"0.0.0.0/0"}],
								"gateway":"10.10.10.1"}`,
							MaxTxRate:        &maxTxRate,
							MinTxRate:        &minTxRate,
							NetworkNamespace: namespaces.Test,
						}}
					err := clients.Create(context.Background(), sriovNetwork)
					Expect(err).ToNot(HaveOccurred())

					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					Eventually(func() error {
						return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: "test-ratenetwork", Namespace: namespaces.Test}, netAttDef)
					}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

					checkFunc := func(line string) bool {
						if strings.Contains(line, "max_tx_rate 100Mbps") &&
							strings.Contains(line, "min_tx_rate 40Mbps") {
							return true
						}
						return false
					}

					validationFunction([]string{"test-ratenetwork"}, checkFunc)
				})
			})

			// 25963
			Describe("vlan and Qos vlan", func() {
				It("Should configure the requested vlan and Qos vlan flags under the vf", func() {
					sriovNetwork := &sriovv1.SriovNetwork{ObjectMeta: metav1.ObjectMeta{Name: "test-quosnetwork", Namespace: operatorNamespace},
						Spec: sriovv1.SriovNetworkSpec{
							ResourceName: resourceName,
							IPAM: `{"type":"host-local",
								"subnet":"10.10.10.0/24",
								"rangeStart":"10.10.10.171",
								"rangeEnd":"10.10.10.181",
								"routes":[{"dst":"0.0.0.0/0"}],
								"gateway":"10.10.10.1"}`,
							Vlan:             1,
							VlanQoS:          2,
							NetworkNamespace: namespaces.Test,
						}}
					err := clients.Create(context.Background(), sriovNetwork)
					Expect(err).ToNot(HaveOccurred())

					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					Eventually(func() error {
						return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: "test-quosnetwork", Namespace: namespaces.Test}, netAttDef)
					}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

					checkFunc := func(line string) bool {
						if strings.Contains(line, "vlan 1") &&
							strings.Contains(line, "qos 2") {
							return true
						}
						return false
					}

					validationFunction([]string{"test-quosnetwork"}, checkFunc)
				})
			})
		})

		Context("Multiple sriov device and attachment", func() {
			// 25834
			It("Should configure multiple network attachments", func() {
				ipam := `{"type": "host-local","ranges": [[{"subnet": "1.1.1.0/24"}]],"dataDir": "/run/my-orchestrator/container-ipam-state"}`
				err := network.CreateSriovNetwork(clients, sriovDevice, sriovNetworkName, namespaces.Test, operatorNamespace, resourceName, ipam)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: namespaces.Test}, netAttDef)
				}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				pod := createTestPod(node, []string{sriovNetworkName, sriovNetworkName})
				nics, err := network.GetNicsByPrefix(pod, "net")
				Expect(err).ToNot(HaveOccurred())
				Expect(len(nics)).To(Equal(2), "No sriov network interfaces found.")
			})
		})

		Context("IPv6 configured secondary interfaces on pods", func() {
			// 25874
			It("should be able to ping each other", func() {
				ipv6NetworkName := "test-ipv6network"
				ipam := `{"type": "host-local","ranges": [[{"subnet": "3ffe:ffff:0:01ff::/64"}]],"dataDir": "/run/my-orchestrator/container-ipam-state"}`
				err := network.CreateSriovNetwork(clients, sriovDevice, ipv6NetworkName, namespaces.Test, operatorNamespace, resourceName, ipam)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: ipv6NetworkName, Namespace: namespaces.Test}, netAttDef)
				}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				pod := createTestPod(node, []string{ipv6NetworkName})
				ips, err := network.GetSriovNicIPs(pod, "net1")
				Expect(err).ToNot(HaveOccurred())
				Expect(ips).NotTo(BeNil(), "No sriov network interface found.")
				Expect(len(ips)).Should(Equal(1))
				for _, ip := range ips {
					pingPod(ip, node, ipv6NetworkName)
				}
			})
		})

		Context("NAD update", func() {
			// 24713
			It("NAD is updated when SriovNetwork spec/networkNamespace is changed", func() {
				ns1 := "test-z1"
				ns2 := "test-z2"
				defer namespaces.DeleteAndWait(clients, ns1, 1*time.Minute)
				defer namespaces.DeleteAndWait(clients, ns2, 1*time.Minute)
				err := namespaces.Create(ns1, clients)
				Expect(err).ToNot(HaveOccurred())
				err = namespaces.Create(ns2, clients)
				Expect(err).ToNot(HaveOccurred())

				ipam := `{"type": "host-local","ranges": [[{"subnet": "1.1.1.0/24"}]],"dataDir": "/run/my-orchestrator/container-ipam-state"}`
				err = network.CreateSriovNetwork(clients, sriovDevice, sriovNetworkName, ns1, operatorNamespace, resourceName, ipam)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: ns1}, netAttDef)
				}, 30*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				body, _ := json.Marshal([]patchBody{{
					Op:    "replace",
					Path:  "/spec/networkNamespace",
					Value: ns2,
				}})
				clients.SriovnetworkV1Interface.RESTClient().Patch(types.JSONPatchType).Namespace(operatorNamespace).Resource("sriovnetworks").Name(sriovNetworkName).Body(body).Do(context.Background())

				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: ns2}, netAttDef)
				}, 30*time.Second, 1*time.Second).ShouldNot(HaveOccurred())
				Consistently(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: ns1}, netAttDef)
				}, 5*time.Second, 1*time.Second).Should(HaveOccurred())
			})
		})

		Context("NAD update", func() {
			// 24714
			It("NAD default gateway is updated when SriovNetwork ipam is changed", func() {

				ipam := `{
					"type": "host-local",
					"subnet": "10.11.11.0/24",
					"gateway": "%s"
				  }`
				err := network.CreateSriovNetwork(clients, sriovDevice, sriovNetworkName, namespaces.Test, operatorNamespace, resourceName, fmt.Sprintf(ipam, "10.11.11.1"))
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() bool {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					err := clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: namespaces.Test}, netAttDef)
					if errors.IsNotFound(err) {
						return false
					}
					return strings.Contains(netAttDef.Spec.Config, "10.11.11.1")
				}, 30*time.Second, 1*time.Second).Should(BeTrue())

				sriovNetwork := &sriovv1.SriovNetwork{}
				err = clients.Get(context.TODO(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: operatorNamespace}, sriovNetwork)
				Expect(err).ToNot(HaveOccurred())
				sriovNetwork.Spec.IPAM = fmt.Sprintf(ipam, "10.11.11.100")
				err = clients.Update(context.Background(), sriovNetwork)
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() bool {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: namespaces.Test}, netAttDef)
					return strings.Contains(netAttDef.Spec.Config, "10.11.11.100")
				}, 30*time.Second, 1*time.Second).Should(BeTrue())
			})
		})

		Context("SRIOV and macvlan", func() {
			// 25834
			It("Should be able to create a pod with both sriov and macvlan interfaces", func() {
				ipam := `{"type": "host-local","ranges": [[{"subnet": "1.1.1.0/24"}]],"dataDir": "/run/my-orchestrator/container-ipam-state"}`
				err := network.CreateSriovNetwork(clients, sriovDevice, sriovNetworkName, namespaces.Test, operatorNamespace, resourceName, ipam)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: namespaces.Test}, netAttDef)
				}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				macvlanNadName := "macvlan-nad"
				macvlanNad := network.CreateMacvlanNetworkAttachmentDefinition(macvlanNadName, namespaces.Test)
				err = clients.Create(context.Background(), &macvlanNad)
				Expect(err).ToNot(HaveOccurred())
				defer clients.Delete(context.Background(), &macvlanNad)
				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: macvlanNadName, Namespace: namespaces.Test}, netAttDef)
				}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				createdPod := createTestPod(node, []string{sriovNetworkName, macvlanNadName})

				nics, err := network.GetNicsByPrefix(createdPod, "net")
				Expect(err).ToNot(HaveOccurred())
				Expect(len(nics)).To(Equal(2), "Pod should have two multus nics.")

				stdout, _, err := pod.ExecCommand(clients, createdPod, "ethtool", "-i", "net1")
				Expect(err).ToNot(HaveOccurred())

				sriovVfDriver := getDriver(stdout)
				Expect(cluster.IsVFDriverSupported(sriovVfDriver)).To(BeTrue())

				stdout, _, err = pod.ExecCommand(clients, createdPod, "ethtool", "-i", "net2")
				macvlanDriver := getDriver(stdout)
				Expect(err).ToNot(HaveOccurred())
				Expect(macvlanDriver).To(Equal("macvlan"))

			})
		})

		Context("Virtual Functions", func() {
			// 21396
			It("should release the VFs once the pod deleted and same VFs can be used by the new created pods", func() {
				By("Create first Pod which consumes all available VFs")
				sriovDevice, err := sriovInfos.FindOneSriovDevice(node)
				ipam := `{"type": "host-local","ranges": [[{"subnet": "3ffe:ffff:0:01ff::/64"}]],"dataDir": "/run/my-orchestrator/container-ipam-state"}`
				err = network.CreateSriovNetwork(clients, sriovDevice, sriovNetworkName, namespaces.Test, operatorNamespace, resourceName, ipam)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: namespaces.Test}, netAttDef)
				}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				testPodA := pod.RedefineWithNodeSelector(
					pod.DefineWithNetworks([]string{sriovNetworkName, sriovNetworkName, sriovNetworkName, sriovNetworkName, sriovNetworkName}),
					node,
				)
				runningPodA, err := clients.Pods(testPodA.Namespace).Create(context.Background(), testPodA, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Error to create pod %s", testPodA.Name))
				By("Checking that first Pod is in Running state")
				Eventually(func() v1core.PodPhase {
					runningPodA, err = clients.Pods(namespaces.Test).Get(context.Background(), runningPodA.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return runningPodA.Status.Phase
				}, 3*time.Minute, time.Second).Should(Equal(v1core.PodRunning))
				By("Create second Pod which consumes one more VF")

				testPodB := pod.RedefineWithNodeSelector(
					pod.DefineWithNetworks([]string{sriovNetworkName}),
					node,
				)
				runningPodB, err := clients.Pods(testPodB.Namespace).Create(context.Background(), testPodB, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Error to create pod %s", testPodB.Name))
				By("Checking second that pod is in Pending state")
				Eventually(func() v1core.PodPhase {
					runningPodB, err = clients.Pods(namespaces.Test).Get(context.Background(), runningPodB.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return runningPodB.Status.Phase
				}, 3*time.Minute, time.Second).Should(Equal(v1core.PodPending))

				By("Checking that relevant error event was originated")
				Eventually(func() string {
					events, err := clients.Events(namespaces.Test).List(context.Background(), metav1.ListOptions{})
					Expect(err).ToNot(HaveOccurred())

					for _, val := range events.Items {
						if val.InvolvedObject.Name == runningPodB.Name {
							return val.Message
						}
					}
					return ""
				}, 2*time.Minute, 10*time.Second).Should(ContainSubstring("Insufficient openshift.io/%s", resourceName),
					"Error to detect Required Event")
				By("Delete first pod and release all VFs")
				err = clients.Pods(namespaces.Test).Delete(context.Background(), runningPodA.Name, metav1.DeleteOptions{
					GracePeriodSeconds: pointer.Int64Ptr(0),
				})
				Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Error to delete pod %s", runningPodA.Name))
				By("Checking that second pod is able to use released VF")
				Eventually(func() v1core.PodPhase {
					runningPodB, err = clients.Pods(namespaces.Test).Get(context.Background(), runningPodB.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return runningPodB.Status.Phase
				}, 3*time.Minute, time.Second).Should(Equal(v1core.PodRunning))
			})
		})
	})

	Describe("Custom SriovNetworkNodePolicy", func() {

		BeforeEach(func() {
			err := namespaces.Clean(operatorNamespace, namespaces.Test, clients, discovery.Enabled())
			Expect(err).ToNot(HaveOccurred())
			waitForSRIOVStable()
		})

		Describe("Configuration", func() {

			Context("PF Partitioning", func() {
				// 27633
				It("Should be possible to partition the pf's vfs", func() {
					if discovery.Enabled() {
						Skip("Test unsuitable to be run in discovery mode")
					}
					node := sriovInfos.Nodes[0]
					intf, err := sriovInfos.FindOneSriovDevice(node)
					Expect(err).ToNot(HaveOccurred())

					firstConfig := &sriovv1.SriovNetworkNodePolicy{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "test-policy",
							Namespace:    operatorNamespace,
						},

						Spec: sriovv1.SriovNetworkNodePolicySpec{
							NodeSelector: map[string]string{
								"kubernetes.io/hostname": node,
							},
							NumVfs:       5,
							ResourceName: "testresource",
							Priority:     99,
							NicSelector: sriovv1.SriovNetworkNicSelector{
								PfNames: []string{intf.Name + "#2-4"},
							},
							DeviceType: "netdevice",
						},
					}

					err = clients.Create(context.Background(), firstConfig)
					Expect(err).ToNot(HaveOccurred())

					Eventually(func() sriovv1.Interfaces {
						nodeState, err := clients.SriovNetworkNodeStates(operatorNamespace).Get(context.Background(), node, metav1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						return nodeState.Spec.Interfaces
					}, 1*time.Minute, 1*time.Second).Should(ContainElement(MatchFields(
						IgnoreExtras,
						Fields{
							"Name":   Equal(intf.Name),
							"NumVfs": Equal(5),
							"VfGroups": ContainElement(
								MatchFields(
									IgnoreExtras,
									Fields{
										"ResourceName": Equal("testresource"),
										"DeviceType":   Equal("netdevice"),
										"VfRange":      Equal("2-4"),
									})),
						})))

					waitForSRIOVStable()

					Eventually(func() int64 {
						testedNode, err := clients.Nodes().Get(context.Background(), node, metav1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						resNum, _ := testedNode.Status.Allocatable["openshift.io/testresource"]
						capacity, _ := resNum.AsInt64()
						return capacity
					}, 3*time.Minute, time.Second).Should(Equal(int64(3)))

					secondConfig := &sriovv1.SriovNetworkNodePolicy{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "test-policy",
							Namespace:    operatorNamespace,
						},

						Spec: sriovv1.SriovNetworkNodePolicySpec{
							NodeSelector: map[string]string{
								"kubernetes.io/hostname": node,
							},
							NumVfs:       5,
							ResourceName: "testresource1",
							Priority:     99,
							NicSelector: sriovv1.SriovNetworkNicSelector{
								PfNames: []string{intf.Name + "#0-1"},
							},
							DeviceType: "vfio-pci",
						},
					}

					err = clients.Create(context.Background(), secondConfig)
					Expect(err).ToNot(HaveOccurred())

					Eventually(func() sriovv1.Interfaces {
						nodeState, err := clients.SriovNetworkNodeStates(operatorNamespace).Get(context.Background(), node, metav1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						return nodeState.Spec.Interfaces
					}, 3*time.Minute, 1*time.Second).Should(ContainElement(MatchFields(
						IgnoreExtras,
						Fields{
							"Name":   Equal(intf.Name),
							"NumVfs": Equal(5),
							"VfGroups": SatisfyAll(
								ContainElement(
									MatchFields(
										IgnoreExtras,
										Fields{
											"ResourceName": Equal("testresource"),
											"DeviceType":   Equal("netdevice"),
											"VfRange":      Equal("2-4"),
										})),
								ContainElement(
									MatchFields(
										IgnoreExtras,
										Fields{
											"ResourceName": Equal("testresource1"),
											"DeviceType":   Equal("vfio-pci"),
											"VfRange":      Equal("0-1"),
										})),
							),
						},
					)))

					// The node may reset here so we put a larger timeout here
					Eventually(func() bool {
						res, err := cluster.SriovStable(operatorNamespace, clients)
						Expect(err).ToNot(HaveOccurred())
						return res
					}, 15*time.Minute, 5*time.Second).Should(BeTrue())

					Eventually(func() map[string]int64 {
						testedNode, err := clients.Nodes().Get(context.Background(), node, metav1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						resNum, _ := testedNode.Status.Allocatable["openshift.io/testresource"]
						capacity, _ := resNum.AsInt64()
						res := make(map[string]int64)
						res["openshift.io/testresource"] = capacity
						resNum, _ = testedNode.Status.Allocatable["openshift.io/testresource1"]
						capacity, _ = resNum.AsInt64()
						res["openshift.io/testresource1"] = capacity
						return res
					}, 2*time.Minute, time.Second).Should(Equal(map[string]int64{
						"openshift.io/testresource":  int64(3),
						"openshift.io/testresource1": int64(2),
					}))
				})

				// 27630
				/*It("Should not be possible to have overlapping pf ranges", func() {
					// Skipping this test as blocking the override will
					// be implemented in 4.5, as per bz #1798880
					Skip("Overlapping is still not blocked")
					node := sriovInfos.Nodes[0]
					intf, err := sriovInfos.FindOneSriovDevice(node)
					Expect(err).ToNot(HaveOccurred())

					firstConfig := &sriovv1.SriovNetworkNodePolicy{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "test-policy",
							Namespace:    operatorNamespace,
						},

						Spec: sriovv1.SriovNetworkNodePolicySpec{
							NodeSelector: map[string]string{
								"kubernetes.io/hostname": node,
							},
							NumVfs:       5,
							ResourceName: "testresource",
							Priority:     99,
							NicSelector: sriovv1.SriovNetworkNicSelector{
								PfNames: []string{intf.Name + "#1-4"},
							},
							DeviceType: "netdevice",
						},
					}

					err = clients.Create(context.Background(), firstConfig)
					Expect(err).ToNot(HaveOccurred())

					Eventually(func() sriovv1.Interfaces {
						nodeState, err := clients.SriovNetworkNodeStates(operatorNamespace).Get(node, metav1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						return nodeState.Spec.Interfaces
					}, 1*time.Minute, 1*time.Second).Should(ContainElement(MatchFields(
						IgnoreExtras,
						Fields{
							"Name":     Equal(intf.Name),
							"NumVfs":   Equal(5),
							"VfGroups": ContainElement(sriovv1.VfGroup{ResourceName: "testresource", DeviceType: "netdevice", VfRange: "1-4", PolicyName: firstConfig.Name}),
						})))

					secondConfig := &sriovv1.SriovNetworkNodePolicy{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "test-policy",
							Namespace:    operatorNamespace,
						},

						Spec: sriovv1.SriovNetworkNodePolicySpec{
							NodeSelector: map[string]string{
								"kubernetes.io/hostname": node,
							},
							NumVfs:       5,
							ResourceName: "testresource1",
							Priority:     99,
							NicSelector: sriovv1.SriovNetworkNicSelector{
								PfNames: []string{intf.Name + "#0-2"},
							},
							DeviceType: "vfio-pci",
						},
					}

					err = clients.Create(context.Background(), secondConfig)
					Expect(err).To(HaveOccurred())
				})*/
			})
			Context("PF shutdown", func() {
				// 29398
				It("Should be able to create pods successfully if PF is down.Pods are able to communicate with each other on the same node", func() {
					resourceName := "testresource"
					var testNode string
					var unusedSriovDevice *sriovv1.InterfaceExt

					if discovery.Enabled() {
						var numVfs int
						var err error
						testNode, resourceName, numVfs, unusedSriovDevice, err = discovery.DiscoveredResources(clients,
							sriovInfos, operatorNamespace, defaultFilterPolicy,
							func(node string, sriovDeviceList []*sriovv1.InterfaceExt) (*sriovv1.InterfaceExt, bool) {
								if len(sriovDeviceList) == 0 {
									return nil, false
								}
								unusedSriovDevices, err := findUnusedSriovDevices(node, sriovDeviceList)
								if err != nil && len(unusedSriovDevices) == 0 {
									return nil, false
								}
								return unusedSriovDevices[0], true
							},
						)

						Expect(err).ToNot(HaveOccurred())
						if testNode == "" || resourceName == "" || numVfs < 5 || unusedSriovDevice == nil {
							Skip("Insufficient resources to run tests in discovery mode")
						}
					} else {
						testNode = sriovInfos.Nodes[0]
						sriovDeviceList, err := sriovInfos.FindSriovDevices(testNode)
						Expect(err).ToNot(HaveOccurred())
						unusedSriovDevices, err := findUnusedSriovDevices(testNode, sriovDeviceList)
						Expect(err).ToNot(HaveOccurred())
						unusedSriovDevice = unusedSriovDevices[0]
						defer changeNodeInterfaceState(testNode, unusedSriovDevices[0].Name, true)
						Expect(err).ToNot(HaveOccurred())
						createSriovPolicy(unusedSriovDevice.Name, testNode, 2, resourceName)
					}

					ipam := `{
						"type":"host-local",
						"subnet":"10.10.10.0/24",
						"rangeStart":"10.10.10.171",
						"rangeEnd":"10.10.10.181",
						"routes":[{"dst":"0.0.0.0/0"}],
						"gateway":"10.10.10.1"
						}`
					err := network.CreateSriovNetwork(clients, unusedSriovDevice, sriovNetworkName, namespaces.Test, operatorNamespace, resourceName, ipam)
					Expect(err).ToNot(HaveOccurred())
					Eventually(func() error {
						netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
						return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: sriovNetworkName, Namespace: namespaces.Test}, netAttDef)
					}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())
					changeNodeInterfaceState(testNode, unusedSriovDevice.Name, false)
					pod := createTestPod(testNode, []string{sriovNetworkName})
					ips, err := network.GetSriovNicIPs(pod, "net1")
					Expect(err).ToNot(HaveOccurred())
					Expect(ips).NotTo(BeNil(), "No sriov network interface found.")
					Expect(len(ips)).Should(Equal(1))
					for _, ip := range ips {
						pingPod(ip, testNode, sriovNetworkName)
					}
				})
			})

			Context("MTU", func() {
				BeforeEach(func() {
					var node string
					resourceName := "mturesource"
					numVfs := 5
					var intf *sriovv1.InterfaceExt
					var err error

					if discovery.Enabled() {
						node, resourceName, numVfs, intf, err = discovery.DiscoveredResources(clients,
							sriovInfos, operatorNamespace,
							func(policy sriovv1.SriovNetworkNodePolicy) bool {
								if !defaultFilterPolicy(policy) {
									return false
								}
								if policy.Spec.Mtu != 9000 {
									return false
								}
								return true
							},
							func(node string, sriovDeviceList []*sriovv1.InterfaceExt) (*sriovv1.InterfaceExt, bool) {
								if len(sriovDeviceList) == 0 {
									return nil, false
								}
								nodeState, err := clients.SriovNetworkNodeStates(operatorNamespace).Get(context.Background(), node, metav1.GetOptions{})
								Expect(err).ToNot(HaveOccurred())

								for _, ifc := range nodeState.Spec.Interfaces {
									if ifc.Mtu == 9000 && ifc.NumVfs > 0 {
										for _, device := range sriovDeviceList {
											if device.Name == ifc.Name {
												return device, true
											}
										}
									}
								}
								return nil, false
							},
						)
						Expect(err).ToNot(HaveOccurred())
						if node == "" || resourceName == "" || numVfs < 5 || intf == nil {
							Skip("Insufficient resources to run test in discovery mode")
						}
					} else {
						node = sriovInfos.Nodes[0]
						sriovDeviceList, err := sriovInfos.FindSriovDevices(node)
						Expect(err).ToNot(HaveOccurred())
						unusedSriovDevices, err := findUnusedSriovDevices(node, sriovDeviceList)
						Expect(err).ToNot(HaveOccurred())
						intf = unusedSriovDevices[0]

						mtuPolicy := &sriovv1.SriovNetworkNodePolicy{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "test-mtupolicy",
								Namespace:    operatorNamespace,
							},

							Spec: sriovv1.SriovNetworkNodePolicySpec{
								NodeSelector: map[string]string{
									"kubernetes.io/hostname": node,
								},
								Mtu:          9000,
								NumVfs:       5,
								ResourceName: resourceName,
								Priority:     99,
								NicSelector: sriovv1.SriovNetworkNicSelector{
									PfNames: []string{intf.Name},
								},
								DeviceType: "netdevice",
							},
						}

						err = clients.Create(context.Background(), mtuPolicy)
						Expect(err).ToNot(HaveOccurred())

						waitForSRIOVStable()
					}

					sriovNetwork := &sriovv1.SriovNetwork{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-mtuvolnetwork",
							Namespace: operatorNamespace,
						},
						Spec: sriovv1.SriovNetworkSpec{
							ResourceName:     resourceName,
							IPAM:             `{"type":"host-local","subnet":"10.10.10.0/24","rangeStart":"10.10.10.171","rangeEnd":"10.10.10.181","routes":[{"dst":"0.0.0.0/0"}],"gateway":"10.10.10.1"}`,
							NetworkNamespace: namespaces.Test,
							LinkState:        "enable",
						}}

					// We need this to be able to run the connectivity checks on Mellanox cards
					if intf.DeviceID == "1015" {
						sriovNetwork.Spec.SpoofChk = "off"
					}

					err = clients.Create(context.Background(), sriovNetwork)

					Expect(err).ToNot(HaveOccurred())

					Eventually(func() error {
						netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
						return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: "test-mtuvolnetwork", Namespace: namespaces.Test}, netAttDef)
					}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				})

				// 27662
				It("Should support jumbo frames", func() {
					podDefinition := pod.DefineWithNetworks([]string{"test-mtuvolnetwork"})
					firstPod, err := clients.Pods(namespaces.Test).Create(context.Background(), podDefinition, metav1.CreateOptions{})
					Expect(err).ToNot(HaveOccurred())

					Eventually(func() corev1.PodPhase {
						firstPod, _ = clients.Pods(namespaces.Test).Get(context.Background(), firstPod.Name, metav1.GetOptions{})
						return firstPod.Status.Phase
					}, 3*time.Minute, time.Second).Should(Equal(corev1.PodRunning))

					var stdout, stderr string
					Eventually(func() error {
						stdout, stderr, err = pod.ExecCommand(clients, firstPod, "ip", "link", "show", "net1")
						if stdout == "" {
							return fmt.Errorf("empty response from pod exec")
						}

						if err != nil {
							return fmt.Errorf("Failed to show net1")
						}

						return nil
					}, 1*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
					Expect(stdout).To(ContainSubstring("mtu 9000"))
					firstPodIPs, err := network.GetSriovNicIPs(firstPod, "net1")
					Expect(err).ToNot(HaveOccurred())
					Expect(len(firstPodIPs)).To(Equal(1))

					podDefinition = pod.DefineWithNetworks([]string{"test-mtuvolnetwork"})
					secondPod, err := clients.Pods(namespaces.Test).Create(context.Background(), podDefinition, metav1.CreateOptions{})
					Expect(err).ToNot(HaveOccurred())

					Eventually(func() corev1.PodPhase {
						secondPod, _ = clients.Pods(namespaces.Test).Get(context.Background(), secondPod.Name, metav1.GetOptions{})
						return secondPod.Status.Phase
					}, 3*time.Minute, time.Second).Should(Equal(corev1.PodRunning))

					stdout, stderr, err = pod.ExecCommand(clients, secondPod,
						"ping", firstPodIPs[0], "-s", "8972", "-M", "do", "-c", "2")
					Expect(err).ToNot(HaveOccurred(), "Failed to ping first pod", stderr)
					Expect(stdout).To(ContainSubstring("2 packets transmitted, 2 received, 0% packet loss"))
				})
			})
		})

		Context("Nic Validation", func() {
			numVfs := 5
			resourceName := "testresource"

			BeforeEach(func() {
				if discovery.Enabled() {
					Skip("Test unsuitable to be run in discovery mode")
				}
			})

			findSriovDevice := func(vendorID, deviceID string) (string, sriovv1.InterfaceExt) {
				for _, node := range sriovInfos.Nodes {
					for _, nic := range sriovInfos.States[node].Status.Interfaces {
						if vendorID != "" && deviceID != "" && nic.Vendor == vendorID && nic.DeviceID == deviceID {
							return node, nic
						}
					}
				}

				return "", sriovv1.InterfaceExt{}
			}

			DescribeTable("Test connectivity using the requested nic", func(vendorID, deviceID string) {
				By("searching for the requested network card")
				node, nic := findSriovDevice(vendorID, deviceID)
				if node == "" {
					Skip(fmt.Sprintf("skip nic validate as wasn't able to find a nic with vendorID %s and deviceID %s", vendorID, deviceID))
				}

				By("creating a network policy")
				config := &sriovv1.SriovNetworkNodePolicy{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "test-policy",
						Namespace:    operatorNamespace,
					},

					Spec: sriovv1.SriovNetworkNodePolicySpec{
						NodeSelector: map[string]string{
							"kubernetes.io/hostname": node,
						},
						NumVfs:       numVfs,
						ResourceName: resourceName,
						Priority:     99,
						NicSelector: sriovv1.SriovNetworkNicSelector{
							PfNames: []string{nic.Name},
						},
						DeviceType: "netdevice",
					},
				}
				err := clients.Create(context.Background(), config)
				Expect(err).ToNot(HaveOccurred())

				By("waiting for the node state to be updated")
				Eventually(func() sriovv1.Interfaces {
					nodeState, err := clients.SriovNetworkNodeStates(operatorNamespace).Get(context.Background(), node, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					return nodeState.Spec.Interfaces
				}, 1*time.Minute, 1*time.Second).Should(ContainElement(MatchFields(
					IgnoreExtras,
					Fields{
						"Name":   Equal(nic.Name),
						"NumVfs": Equal(numVfs),
					})))

				By("waiting the sriov to be stable on the node")
				waitForSRIOVStable()

				By("creating a network object")
				ipv6NetworkName := "test-ipv6network"
				ipam := `{"type": "host-local","ranges": [[{"subnet": "3ffe:ffff:0:01ff::/64"}]],"dataDir": "/run/my-orchestrator/container-ipam-state"}`
				err = network.CreateSriovNetwork(clients, &nic, ipv6NetworkName, namespaces.Test, operatorNamespace, resourceName, ipam)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() error {
					netAttDef := &netattdefv1.NetworkAttachmentDefinition{}
					return clients.Get(context.Background(), runtimeclient.ObjectKey{Name: ipv6NetworkName, Namespace: namespaces.Test}, netAttDef)
				}, 10*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

				By("creating a pod")
				pod := createTestPod(node, []string{ipv6NetworkName})
				ips, err := network.GetSriovNicIPs(pod, "net1")
				Expect(err).ToNot(HaveOccurred())
				Expect(ips).NotTo(BeNil(), "No sriov network interface found.")
				Expect(len(ips)).Should(Equal(1))

				By("run pod from another pod")
				for _, ip := range ips {
					pingPod(ip, node, ipv6NetworkName)
				}
			},
				//25321
				Entry("Intel Corporation Ethernet Controller XXV710 for 25GbE SFP28", "8086", "158b"),
				Entry("Ethernet Controller XXV710 Intel(R) FPGA Programmable Acceleration Card N3000 for Networking", "8086", "0d58"))
		})

		Context("Resource Injector", func() {
			BeforeEach(func() {
				if discovery.Enabled() {
					Skip("Test unsuitable to be run in discovery mode")
				}
			})

			AfterEach(func() {
				if !discovery.Enabled() {
					setSriovOperatorSpecFlag(operatorNetworkInjectorFlag, true)
					setSriovOperatorSpecFlag(operatorWebhookFlag, true)
				}
			})

			DescribeTable("SR-IOV Operator Config, disable",
				func(resourceName string) {
					var err error
					setSriovOperatorSpecFlag(resourceName, false)

					Eventually(func() bool {
						_, err := clients.DaemonSets(operatorNamespace).Get(context.Background(), resourceName, metav1.GetOptions{})
						if k8serrors.IsNotFound(err) {
							return true
						}
						Expect(err).ToNot(HaveOccurred())
						return false
					}, 2*time.Minute, 5*time.Second).Should(BeTrue())

					Eventually(func() bool {
						podsList, err := clients.Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{
							LabelSelector: fmt.Sprintf("app=%s", resourceName)})
						Expect(err).ToNot(HaveOccurred())
						if len(podsList.Items) > 0 {
							return false
						}
						return true

					}, 1*time.Minute, 10*time.Second).Should(BeTrue())

					Eventually(func() bool {
						serviceList, err := clients.Services(operatorNamespace).List(context.Background(), metav1.ListOptions{})
						Expect(err).ToNot(HaveOccurred())
						for _, svc := range serviceList.Items {
							if strings.Contains(svc.ObjectMeta.Name, resourceName) {
								return false
							}
						}
						return true
					}, 1*time.Minute, 10*time.Second).Should(BeTrue())

					Eventually(func() bool {
						crs := rbacv1.ClusterRoleList{}
						err = clients.List(context.Background(), &crs, runtimeclient.InNamespace("openshift-sriov-network-operator"))
						Expect(err).ToNot(HaveOccurred())
						for _, cr := range crs.Items {
							if strings.Contains(cr.ObjectMeta.Name, resourceName) {
								return false
							}
						}
						return true
					}, 1*time.Minute, 10*time.Second).Should(BeTrue())

					Eventually(func() bool {
						crbs := rbacv1.ClusterRoleBindingList{}
						err = clients.List(context.Background(), &crbs, runtimeclient.InNamespace("openshift-sriov-network-operator"))
						Expect(err).ToNot(HaveOccurred())
						for _, crb := range crbs.Items {
							if strings.Contains(crb.ObjectMeta.Name, resourceName) {
								return false
							}
						}
						return true
					}, 1*time.Minute, 10*time.Second).Should(BeTrue())

					Eventually(func() bool {
						mwc := &admission.MutatingWebhookConfiguration{}
						err = clients.Get(context.Background(), runtimeclient.ObjectKey{Name: fmt.Sprintf("%s-config", resourceName), Namespace: operatorNamespace}, mwc)
						return err != nil && k8serrors.IsNotFound(err)
					}, 1*time.Minute, 10*time.Second).Should(BeTrue())

					Eventually(func() bool {
						cms := corev1.ConfigMapList{}
						err = clients.List(context.Background(), &cms, runtimeclient.InNamespace("openshift-sriov-network-operator"))
						Expect(err).ToNot(HaveOccurred())
						for _, cm := range cms.Items {
							if strings.Contains(cm.ObjectMeta.Name, resourceName) {
								return false
							}
						}
						return true
					}, 1*time.Minute, 10*time.Second).Should(BeTrue())
				},
				//25814
				Entry("Network resource injector", operatorNetworkInjectorFlag),
				//25847
				Entry("Webhook resource injector", operatorWebhookFlag))

		})

	})
})

func getDriver(ethtoolstdout string) string {
	lines := strings.Split(ethtoolstdout, "\n")
	Expect(len(lines)).To(BeNumerically(">", 0))
	for _, line := range lines {
		if strings.HasPrefix(line, "driver:") {
			return strings.TrimSpace(line[len("driver:"):])
		}
	}
	Fail("Could not find device driver")
	return ""
}

func changeNodeInterfaceState(testNode string, ifcName string, enable bool) {
	state := "up"
	if !enable {
		state = "down"
	}
	podDefinition := pod.RedefineAsPrivileged(
		pod.RedefineWithRestartPolicy(
			pod.RedefineWithCommand(
				pod.DefineWithHostNetwork(testNode),
				[]string{"ip", "link", "set", "dev", ifcName, state}, []string{},
			),
			k8sv1.RestartPolicyNever,
		),
	)
	createdPod, err := clients.Pods(namespaces.Test).Create(context.Background(), podDefinition, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())
	Eventually(func() k8sv1.PodPhase {
		runningPod, err := clients.Pods(namespaces.Test).Get(context.Background(), createdPod.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		return runningPod.Status.Phase
	}, 3*time.Minute, 1*time.Second).Should(Equal(k8sv1.PodSucceeded))
}

func findUnusedSriovDevices(testNode string, sriovDevices []*sriovv1.InterfaceExt) ([]*sriovv1.InterfaceExt, error) {
	createdPod := createCustomTestPod(testNode, []string{}, true)
	filteredDevices := []*sriovv1.InterfaceExt{}
	for _, device := range sriovDevices {
		stdout, _, err := pod.ExecCommand(clients, createdPod, "ip", "route")
		Expect(err).ToNot(HaveOccurred())
		lines := strings.Split(stdout, "\n")
		if len(lines) > 0 {
			if strings.Index(lines[0], "default") == 0 && strings.Index(lines[0], "dev "+device.Name) > 0 {
				continue // The interface is a default route
			}
		}
		stdout, _, err = pod.ExecCommand(clients, createdPod, "ip", "link", "show", device.Name)
		Expect(err).ToNot(HaveOccurred())
		Expect(len(stdout)).Should(Not(Equal(0)), "Unable to query link state")
		if strings.Index(stdout, "state DOWN") > 0 {
			continue // The interface is not active
		}

		isInterfaceSlave, err := isInterfaceSlave(createdPod, device.Name)
		Expect(err).ToNot(HaveOccurred())
		if isInterfaceSlave {
			continue
		}
		filteredDevices = append(filteredDevices, device)
	}
	if len(filteredDevices) == 0 {
		return nil, fmt.Errorf("Unused sriov devices not found")
	}
	return filteredDevices, nil
}

func isInterfaceSlave(ifcPod *k8sv1.Pod, ifcName string) (bool, error) {
	stdout, _, err := pod.ExecCommand(clients, ifcPod, "bridge", "link")
	if err != nil {
		return false, err
	}
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		if strings.Contains(line, ifcName) && strings.Contains(line, "master") {
			return true, nil // The interface is part of a bridge (it has a master)
		}
	}
	return false, nil
}

// podVFIndexInHost retrieves the vf index on the host network namespace related to the given
// interface that was passed to the pod, using the name in the pod's namespace.
func podVFIndexInHost(hostNetPod *corev1.Pod, targetPod *corev1.Pod, interfaceName string) (int, error) {
	var stdout, stderr string
	var err error
	Eventually(func() error {
		stdout, stderr, err = pod.ExecCommand(clients, targetPod, "readlink", "-f", fmt.Sprintf("/sys/class/net/%s", interfaceName))
		if stdout == "" {
			return fmt.Errorf("empty response from pod exec")
		}

		if err != nil {
			return fmt.Errorf("Failed to find %s interface address %v - %s", interfaceName, err, stderr)
		}

		return nil
	}, 1*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())

	// sysfs address looks like: /sys/devices/pci0000:17/0000:17:02.0/0000:19:00.5/net/net1
	pathSegments := strings.Split(stdout, "/")
	segNum := len(pathSegments)

	if !strings.HasPrefix(pathSegments[segNum-1], "net1") { // not checking equality because of rubbish like new line
		return 0, fmt.Errorf("Expecting net1 as last segment of %s", stdout)
	}

	podVFAddr := pathSegments[segNum-3] // 0000:19:00.5

	devicePath := strings.Join(pathSegments[0:segNum-2], "/") // /sys/devices/pci0000:17/0000:17:02.0/0000:19:00.5/
	findAllSiblingVfs := strings.Split(fmt.Sprintf("ls -gG %s/physfn/", devicePath), " ")

	res := 0
	Eventually(func() error {
		stdout, stderr, err = pod.ExecCommand(clients, hostNetPod, findAllSiblingVfs...)
		if stdout == "" {
			return fmt.Errorf("empty response from pod exec")
		}

		if err != nil {
			return fmt.Errorf("Failed to find %s siblings %v - %s", devicePath, err, stderr)
		}

		// lines of the format of
		// lrwxrwxrwx. 1        0 Mar  6 15:15 virtfn3 -> ../0000:19:00.5
		scanner := bufio.NewScanner(strings.NewReader(stdout))
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.Contains(line, "virtfn") {
				continue
			}

			columns := strings.Fields(line)

			if len(columns) != 9 {
				return fmt.Errorf("Expecting 9 columns in %s, found %d", line, len(columns))
			}

			vfAddr := strings.TrimPrefix(columns[8], "../") // ../0000:19:00.2

			if vfAddr == podVFAddr { // Found!
				vfName := columns[6] // virtfn0
				vfNumber := strings.TrimPrefix(vfName, "virtfn")
				res, err = strconv.Atoi(vfNumber)
				if err != nil {
					return fmt.Errorf("Could not get vf number from vfname %s", vfName)
				}
				return nil
			}
		}
		return fmt.Errorf("Could not find %s index in %s", podVFAddr, stdout)
	}, 1*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())

	return res, nil
}

func daemonsScheduledOnNodes(selector string) bool {
	nn, err := clients.Nodes().List(context.Background(), metav1.ListOptions{
		LabelSelector: selector,
	})
	nodes := nn.Items

	daemons, err := clients.Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app=sriov-network-config-daemon"})
	Expect(err).ToNot(HaveOccurred())
	for _, d := range daemons.Items {
		foundNode := false
		for i, n := range nodes {
			if d.Spec.NodeName == n.Name {
				foundNode = true
				// Removing the element from the list as we want to make sure
				// the daemons are running on different nodes
				nodes = append(nodes[:i], nodes[i+1:]...)
				break
			}
		}
		if !foundNode {
			return false
		}
	}
	return true

}

func createSriovPolicy(sriovDevice string, testNode string, numVfs int, resourceName string) {
	_, err := network.CreateSriovPolicy(clients, "test-policy-", operatorNamespace, sriovDevice, testNode, numVfs, resourceName)
	Expect(err).ToNot(HaveOccurred())
	waitForSRIOVStable()

	Eventually(func() int64 {
		testedNode, err := clients.Nodes().Get(context.Background(), testNode, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		resNum, _ := testedNode.Status.Allocatable[corev1.ResourceName("openshift.io/"+resourceName)]
		capacity, _ := resNum.AsInt64()
		return capacity
	}, 10*time.Minute, time.Second).Should(Equal(int64(numVfs)))
}

func createUnschedulableTestPod(node string, networks []string, resourceName string) {
	podDefinition := pod.RedefineWithNodeSelector(
		pod.DefineWithNetworks(networks),
		node,
	)
	createdPod, err := clients.Pods(namespaces.Test).Create(context.Background(), podDefinition, metav1.CreateOptions{})
	Consistently(func() k8sv1.PodPhase {
		runningPod, err := clients.Pods(namespaces.Test).Get(context.Background(), createdPod.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		return runningPod.Status.Phase
	}, 3*time.Minute, 1*time.Second).Should(Equal(k8sv1.PodPending))
	pod, err := clients.Pods(namespaces.Test).Get(context.Background(), createdPod.Name, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	for _, condition := range pod.Status.Conditions {
		if condition.Reason == "Unschedulable" && strings.Contains(condition.Message, "Insufficient openshift.io/"+resourceName) {
			return
		}
	}
	Fail("Pod should be Unschedulable due to: Insufficient openshift.io/" + resourceName)
}

func isPodConditionUnschedulable(pod *k8sv1.Pod, resourceName string) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Reason == "Unschedulable" && strings.Index(condition.Message, "Insufficient openshift.io/"+resourceName) != -1 {
			return true
		}
	}
	return false
}

func createTestPod(node string, networks []string) *k8sv1.Pod {
	return createCustomTestPod(node, networks, false)
}

func createCustomTestPod(node string, networks []string, hostNetwork bool) *k8sv1.Pod {
	var podDefinition *corev1.Pod
	if hostNetwork {
		podDefinition = pod.DefineWithHostNetwork(node)
	} else {
		podDefinition = pod.RedefineWithNodeSelector(
			pod.DefineWithNetworks(networks),
			node,
		)
	}
	createdPod, err := clients.Pods(namespaces.Test).Create(context.Background(), podDefinition, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())

	Eventually(func() k8sv1.PodPhase {
		runningPod, err := clients.Pods(namespaces.Test).Get(context.Background(), createdPod.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		return runningPod.Status.Phase
	}, 3*time.Minute, 1*time.Second).Should(Equal(k8sv1.PodRunning))
	pod, err := clients.Pods(namespaces.Test).Get(context.Background(), createdPod.Name, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	return pod
}

func pingPod(ip string, nodeSelector string, sriovNetworkAttachment string) {
	ipProtocolVersion := "6"
	if len(strings.Split(ip, ".")) == 4 {
		ipProtocolVersion = "4"
	}
	podDefinition := pod.RedefineWithNodeSelector(
		pod.RedefineWithRestartPolicy(
			pod.RedefineWithCommand(
				pod.DefineWithNetworks([]string{sriovNetworkAttachment}),
				[]string{"sh", "-c", fmt.Sprintf("ping -%s -c 3 %s", ipProtocolVersion, ip)}, []string{},
			),
			k8sv1.RestartPolicyNever,
		),
		nodeSelector,
	)
	createdPod, err := clients.Pods(namespaces.Test).Create(context.Background(), podDefinition, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())

	Eventually(func() k8sv1.PodPhase {
		runningPod, err := clients.Pods(namespaces.Test).Get(context.Background(), createdPod.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		return runningPod.Status.Phase
	}, 3*time.Minute, 1*time.Second).Should(Equal(k8sv1.PodSucceeded))
}

func waitForSRIOVStable() {
	// This used to be to check for sriov not to be stable first,
	// then stable. The issue is that if no configuration is applied, then
	// the status won't never go to not stable and the test will fail.
	// TODO: find a better way to handle this scenario

	time.Sleep(5 * time.Second)

	eofErrorCount := 0
	Eventually(func() bool {
		res, err := cluster.SriovStable(operatorNamespace, clients)
		// The check for EofError is done to temorarily work around an issue
		// occuring during tests run. The issue occurs very sporadicly and as such
		// is difficult to identify. eofErrorCount is introduced to allow us to respond
		// to real issues.
		if err == io.ErrUnexpectedEOF {
			eofErrorCount++
			Expect(eofErrorCount).To(BeNumerically("<", 2))
			return false
		}
		Expect(err).ToNot(HaveOccurred())
		return res
	}, waitingTime, 1*time.Second).Should(BeTrue())

	Eventually(func() bool {
		isClusterReady, err := cluster.IsClusterStable(clients)
		Expect(err).ToNot(HaveOccurred())
		return isClusterReady
	}, waitingTime, 1*time.Second).Should(BeTrue())
}

func createVanillaNetworkPolicy(node string, sriovInfos *cluster.EnabledNodes, numVfs int, resourceName string) {
	// For the context of tests is better to use a Mellanox card
	// as they support all the virtual function flags
	// if we don't find a Mellanox card we fall back to any sriov
	// capability interface and skip the rate limit test.
	intf, err := sriovInfos.FindOneMellanoxSriovDevice(node)
	if err != nil {
		intf, err = sriovInfos.FindOneSriovDevice(node)
		Expect(err).ToNot(HaveOccurred())
	}

	config := &sriovv1.SriovNetworkNodePolicy{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-policy",
			Namespace:    operatorNamespace,
		},

		Spec: sriovv1.SriovNetworkNodePolicySpec{
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": node,
			},
			NumVfs:       numVfs,
			ResourceName: resourceName,
			Priority:     99,
			NicSelector: sriovv1.SriovNetworkNicSelector{
				PfNames: []string{intf.Name},
			},
			DeviceType: "netdevice",
		},
	}
	err = clients.Create(context.Background(), config)
	Expect(err).ToNot(HaveOccurred())

	Eventually(func() sriovv1.Interfaces {
		nodeState, err := clients.SriovNetworkNodeStates(operatorNamespace).Get(context.Background(), node, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		return nodeState.Spec.Interfaces
	}, 1*time.Minute, 1*time.Second).Should(ContainElement(MatchFields(
		IgnoreExtras,
		Fields{
			"Name":   Equal(intf.Name),
			"NumVfs": Equal(numVfs),
		})))
}

func defaultFilterPolicy(policy sriovv1.SriovNetworkNodePolicy) bool {
	if policy.Spec.DeviceType != "netdevice" {
		return false
	}
	return true
}

func setSriovOperatorSpecFlag(flagName string, flagValue bool) {
	cfg := sriovv1.SriovOperatorConfig{}
	err := clients.Get(context.TODO(), runtimeclient.ObjectKey{
		Name:      "default",
		Namespace: operatorNamespace,
	}, &cfg)

	Expect(err).ToNot(HaveOccurred())
	if flagName == operatorNetworkInjectorFlag && *cfg.Spec.EnableInjector != flagValue {
		cfg.Spec.EnableInjector = &flagValue
		err = clients.Update(context.TODO(), &cfg)
		Expect(err).ToNot(HaveOccurred())
		Expect(*cfg.Spec.EnableInjector).To(Equal(flagValue))
	}

	if flagName == operatorWebhookFlag && *cfg.Spec.EnableOperatorWebhook != flagValue {
		cfg.Spec.EnableOperatorWebhook = &flagValue
		clients.Update(context.TODO(), &cfg)
		Expect(err).ToNot(HaveOccurred())
		Expect(*cfg.Spec.EnableOperatorWebhook).To(Equal(flagValue))
	}

	if flagValue {
		Eventually(func() bool {
			podsList, err := clients.Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app=%s", flagName)})
			Expect(err).ToNot(HaveOccurred())
			if len(podsList.Items) < 1 {
				return false
			}

			for _, pod := range podsList.Items {
				if pod.Status.Phase != v1core.PodRunning {
					return false
				}
			}

			return true
		}, 1*time.Minute, 10*time.Second).Should(BeTrue())

	}
}
