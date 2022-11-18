/*
Copyright 2021 The Kubernetes Authors.

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

package controllers

import (
	goctx "context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	govc "github.com/vmware/govmomi"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	vtypes "github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apirecord "k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ipamv1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/context/fake"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/identity"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/record"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
	fake_svc "sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/fake"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/helpers/vcsim"
)

func TestReconcileNormal_WaitingForIPAddrAllocation(t *testing.T) {
	var (
		machine *clusterv1.Machine
		cluster *clusterv1.Cluster

		vsphereVM      *infrav1.VSphereVM
		vsphereMachine *infrav1.VSphereMachine
		vsphereCluster *infrav1.VSphereCluster

		initObjs       []client.Object
		ipAddressClaim *ipamv1.IPAddressClaim
	)

	poolAPIGroup := "some.ipam.api.group"

	// initializing a fake server to replace the vSphere endpoint
	model := simulator.VPX()
	model.Host = 0

	simr, err := vcsim.NewBuilder().WithModel(model).Build()
	if err != nil {
		t.Fatalf("unable to create simulator: %s", err)
	}
	defer simr.Destroy()

	create := func(netSpec infrav1.NetworkSpec) func() {
		return func() {
			vsphereCluster = &infrav1.VSphereCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "valid-vsphere-cluster",
					Namespace: "test",
				},
			}

			cluster = &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "valid-cluster",
					Namespace: "test",
				},
				Spec: clusterv1.ClusterSpec{
					InfrastructureRef: &corev1.ObjectReference{
						Name: vsphereCluster.Name,
					},
				},
			}

			machine = &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "test",
					Labels: map[string]string{
						clusterv1.ClusterLabelName: "valid-cluster",
					},
				},
			}
			initObjs = createMachineOwnerHierarchy(machine)

			vsphereMachine = &infrav1.VSphereMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo-vm",
					Namespace: "test",
					Labels: map[string]string{
						clusterv1.ClusterLabelName: "valid-cluster",
					},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "foo"}},
				},
			}

			vsphereVM = &infrav1.VSphereVM{
				TypeMeta: metav1.TypeMeta{
					Kind: "VSphereVM",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "test",
					Finalizers: []string{
						infrav1.VMFinalizer,
					},
					Labels: map[string]string{
						clusterv1.ClusterLabelName: "valid-cluster",
					},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: infrav1.GroupVersion.String(), Kind: "VSphereMachine", Name: "foo-vm"}},
					// To make sure PatchHelper does not error out
					ResourceVersion: "1234",
				},
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Server:     simr.ServerURL().Host,
						Datacenter: "",
						Datastore:  "",
						Network:    netSpec,
					},
				},
				Status: infrav1.VSphereVMStatus{},
			}

			ipAddressClaim = &ipamv1.IPAddressClaim{
				TypeMeta: metav1.TypeMeta{
					Kind: "IPAddressClaim",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo-0-0",
					Namespace: "test",
					Finalizers: []string{
						infrav1.IPAddressClaimFinalizer,
					},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: infrav1.GroupVersion.String(), Kind: vsphereVM.Kind, Name: "foo"}},
				},
				Spec: ipamv1.IPAddressClaimSpec{
					PoolRef: corev1.TypedLocalObjectReference{
						APIGroup: &poolAPIGroup,
						Kind:     "IPAMPools",
						Name:     "my-ip-pool",
					},
				},
			}
		}
	}

	setupReconciler := func(vmService services.VirtualMachineService) vmReconciler {
		initObjs = append(initObjs, vsphereVM, vsphereMachine, machine, cluster, vsphereCluster, ipAddressClaim)
		controllerMgrContext := fake.NewControllerManagerContext(initObjs...)
		password, _ := simr.ServerURL().User.Password()
		controllerMgrContext.Password = password
		controllerMgrContext.Username = simr.ServerURL().User.Username()

		controllerContext := &context.ControllerContext{
			ControllerManagerContext: controllerMgrContext,
			Recorder:                 record.New(apirecord.NewFakeRecorder(100)),
			Logger:                   log.Log,
		}
		return vmReconciler{
			ControllerContext: controllerContext,
			VMService:         vmService,
		}
	}

	t.Run("Waiting for static IP allocation", func(t *testing.T) {
		create(infrav1.NetworkSpec{
			Devices: []infrav1.NetworkDeviceSpec{
				{NetworkName: "nw-1"},
				{NetworkName: "nw-2"},
			},
		})()
		r := setupReconciler(fake_svc.NewVMServiceWithVM(infrav1.VirtualMachine{
			Name:     vsphereVM.Name,
			BiosUUID: "265104de-1472-547c-b873-6dc7883fb6cb",
			State:    infrav1.VirtualMachineStatePending,
			Network:  nil,
		}))
		_, err = r.Reconcile(goctx.Background(), ctrl.Request{NamespacedName: util.ObjectKey(vsphereVM)})
		g := NewWithT(t)
		g.Expect(err).NotTo(HaveOccurred())

		vm := &infrav1.VSphereVM{}
		vmKey := util.ObjectKey(vsphereVM)
		g.Expect(r.Client.Get(goctx.Background(), vmKey, vm)).NotTo(HaveOccurred())

		g.Expect(conditions.Has(vm, infrav1.VMProvisionedCondition)).To(BeTrue())
		vmProvisionCondition := conditions.Get(vm, infrav1.VMProvisionedCondition)
		g.Expect(vmProvisionCondition.Status).To(Equal(corev1.ConditionFalse))
		g.Expect(vmProvisionCondition.Reason).To(Equal(infrav1.WaitingForStaticIPAllocationReason))
	})

	t.Run("Waiting for IP addr allocation", func(t *testing.T) {
		create(infrav1.NetworkSpec{
			Devices: []infrav1.NetworkDeviceSpec{
				{NetworkName: "nw-1", DHCP4: true},
			},
		})()
		r := setupReconciler(fake_svc.NewVMServiceWithVM(infrav1.VirtualMachine{
			Name:     vsphereVM.Name,
			BiosUUID: "265104de-1472-547c-b873-6dc7883fb6cb",
			State:    infrav1.VirtualMachineStateReady,
			Network: []infrav1.NetworkStatus{{
				Connected:   true,
				IPAddrs:     []string{}, // empty array to show waiting for IP address
				MACAddr:     "blah-mac",
				NetworkName: vsphereVM.Spec.Network.Devices[0].NetworkName,
			}},
		}))
		_, err = r.Reconcile(goctx.Background(), ctrl.Request{NamespacedName: util.ObjectKey(vsphereVM)})
		g := NewWithT(t)
		g.Expect(err).NotTo(HaveOccurred())

		vm := &infrav1.VSphereVM{}
		vmKey := util.ObjectKey(vsphereVM)
		g.Expect(r.Client.Get(goctx.Background(), vmKey, vm)).NotTo(HaveOccurred())

		g.Expect(conditions.Has(vm, infrav1.VMProvisionedCondition)).To(BeTrue())
		vmProvisionCondition := conditions.Get(vm, infrav1.VMProvisionedCondition)
		g.Expect(vmProvisionCondition.Status).To(Equal(corev1.ConditionFalse))
		g.Expect(vmProvisionCondition.Reason).To(Equal(infrav1.WaitingForIPAllocationReason))
	})

	t.Run("Deleting a VM with IPAddressClaims", func(t *testing.T) {
		create(infrav1.NetworkSpec{
			Devices: []infrav1.NetworkDeviceSpec{
				{
					NetworkName: "nw-1",
					AddressesFromPools: []corev1.TypedLocalObjectReference{
						{
							APIGroup: &poolAPIGroup,
							Kind:     "IPAMPools",
							Name:     "my-ip-pool",
						},
					},
				},
			},
		})()
		vsphereVM.ObjectMeta.Finalizers = []string{infrav1.VMFinalizer}
		vsphereVM.ObjectMeta.DeletionTimestamp = &metav1.Time{Time: time.Now()}

		r := setupReconciler(fake_svc.NewVMServiceWithVM(infrav1.VirtualMachine{
			Name:     vsphereVM.Name,
			BiosUUID: "265104de-1472-547c-b873-6dc7883fb6cb",
			State:    infrav1.VirtualMachineStateNotFound,
			Network: []infrav1.NetworkStatus{{
				Connected:   true,
				IPAddrs:     []string{}, // empty array to show waiting for IP address
				MACAddr:     "blah-mac",
				NetworkName: vsphereVM.Spec.Network.Devices[0].NetworkName,
			}},
		}))

		g := NewWithT(t)

		_, err := r.Reconcile(goctx.Background(), ctrl.Request{NamespacedName: util.ObjectKey(vsphereVM)})
		g.Expect(err).To(HaveOccurred())

		vm := &infrav1.VSphereVM{}
		vmKey := util.ObjectKey(vsphereVM)
		g.Expect(apierrors.IsNotFound(r.Client.Get(goctx.Background(), vmKey, vm))).To(BeTrue())

		claim := &ipamv1.IPAddressClaim{}
		ipacKey := util.ObjectKey(ipAddressClaim)
		g.Expect(r.Client.Get(goctx.Background(), ipacKey, claim)).NotTo(HaveOccurred())
		g.Expect(claim.ObjectMeta.Finalizers).NotTo(ContainElement(infrav1.IPAddressClaimFinalizer))
	})
}

func TestVmReconciler_WaitingForStaticIPAllocation(t *testing.T) {
	tests := []struct {
		name       string
		devices    []infrav1.NetworkDeviceSpec
		shouldWait bool
	}{
		{
			name:       "for one n/w device with DHCP set to true",
			devices:    []infrav1.NetworkDeviceSpec{{DHCP4: true, NetworkName: "nw-1"}},
			shouldWait: false,
		},
		{
			name: "for multiple n/w devices with DHCP set and unset",
			devices: []infrav1.NetworkDeviceSpec{
				{DHCP4: true, NetworkName: "nw-1"},
				{NetworkName: "nw-2"},
			},
			shouldWait: true,
		},
		{
			name: "for multiple n/w devices with static IP address specified",
			devices: []infrav1.NetworkDeviceSpec{
				{NetworkName: "nw-1", IPAddrs: []string{"192.168.1.2/32"}},
				{NetworkName: "nw-2"},
			},
			shouldWait: true,
		},
		{
			name: "for single n/w devices with DHCP4, DHCP6 & IP address unset",
			devices: []infrav1.NetworkDeviceSpec{
				{NetworkName: "nw-1"},
			},
			shouldWait: true,
		},
		{
			name: "for multiple n/w devices with DHCP4, DHCP6 & IP address unset",
			devices: []infrav1.NetworkDeviceSpec{
				{NetworkName: "nw-1"},
				{NetworkName: "nw-2"},
			},
			shouldWait: true,
		},
	}

	controllerCtx := fake.NewControllerContext(fake.NewControllerManagerContext())
	vmContext := fake.NewVMContext(controllerCtx)
	r := vmReconciler{ControllerContext: controllerCtx}

	for _, tt := range tests {
		// Need to explicitly reinitialize test variable, looks odd, but needed
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			vmContext.VSphereVM.Spec.Network = infrav1.NetworkSpec{Devices: tt.devices}
			isWaiting := r.isWaitingForStaticIPAllocation(vmContext)
			g := NewWithT(t)
			g.Expect(isWaiting).To(Equal(tt.shouldWait))
		})
	}
}

func TestRetrievingVCenterCredentialsFromCluster(t *testing.T) {
	// initializing a fake server to replace the vSphere endpoint
	model := simulator.VPX()
	model.Host = 0

	simr, err := vcsim.NewBuilder().WithModel(model).Build()
	if err != nil {
		t.Fatalf("unable to create simulator: %s", err)
	}
	defer simr.Destroy()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "creds-secret",
			Namespace: "test",
		},
		Data: map[string][]byte{
			identity.UsernameKey: []byte(simr.Username()),
			identity.PasswordKey: []byte(simr.Password()),
		},
	}

	vsphereCluster := &infrav1.VSphereCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-vsphere-cluster",
			Namespace: "test",
		},
		Spec: infrav1.VSphereClusterSpec{
			IdentityRef: &infrav1.VSphereIdentityReference{
				Kind: infrav1.SecretKind,
				Name: secret.Name,
			},
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-cluster",
			Namespace: "test",
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: &corev1.ObjectReference{
				Name: vsphereCluster.Name,
			},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "test",
			Labels: map[string]string{
				clusterv1.ClusterLabelName: "valid-cluster",
			},
		},
	}

	initObjs := createMachineOwnerHierarchy(machine)

	vsphereMachine := &infrav1.VSphereMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo-vm",
			Namespace: "test",
			Labels: map[string]string{
				clusterv1.ClusterLabelName: "valid-cluster",
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "foo"}},
		},
	}

	vsphereVM := &infrav1.VSphereVM{
		TypeMeta: metav1.TypeMeta{
			Kind: "VSphereVM",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "test",
			Labels: map[string]string{
				clusterv1.ClusterLabelName: "valid-cluster",
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: infrav1.GroupVersion.String(), Kind: "VSphereMachine", Name: "foo-vm"}},
			// To make sure PatchHelper does not error out
			ResourceVersion: "1234",
		},
		Spec: infrav1.VSphereVMSpec{
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				Server: simr.ServerURL().Host,
				Network: infrav1.NetworkSpec{
					Devices: []infrav1.NetworkDeviceSpec{
						{NetworkName: "nw-1"},
						{NetworkName: "nw-2"},
					},
				},
			},
		},
		Status: infrav1.VSphereVMStatus{},
	}

	initObjs = append(initObjs, secret, vsphereVM, vsphereMachine, machine, cluster, vsphereCluster)
	controllerMgrContext := fake.NewControllerManagerContext(initObjs...)

	controllerContext := &context.ControllerContext{
		ControllerManagerContext: controllerMgrContext,
		Recorder:                 record.New(apirecord.NewFakeRecorder(100)),
		Logger:                   log.Log,
	}
	r := vmReconciler{ControllerContext: controllerContext}

	_, err = r.Reconcile(goctx.Background(), ctrl.Request{NamespacedName: util.ObjectKey(vsphereVM)})
	g := NewWithT(t)
	g.Expect(err).NotTo(HaveOccurred())

	vm := &infrav1.VSphereVM{}
	vmKey := util.ObjectKey(vsphereVM)
	g.Expect(r.Client.Get(goctx.Background(), vmKey, vm)).NotTo(HaveOccurred())
	g.Expect(conditions.Has(vm, infrav1.VCenterAvailableCondition)).To(BeTrue())
	vCenterCondition := conditions.Get(vm, infrav1.VCenterAvailableCondition)
	g.Expect(vCenterCondition.Status).To(Equal(corev1.ConditionTrue))
}

func TestDHCPOverridesSetInVMMetadata(t *testing.T) {
	// initializing a fake server to replace the vSphere endpoint
	model := simulator.VPX()
	model.Host = 0

	simr, err := vcsim.NewBuilder().WithModel(model).Build()
	if err != nil {
		t.Fatalf("unable to create simulator: %s", err)
	}
	defer simr.Destroy()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "creds-secret",
			Namespace: "test",
		},
		Data: map[string][]byte{
			identity.UsernameKey: []byte(simr.Username()),
			identity.PasswordKey: []byte(simr.Password()),
		},
	}

	vsphereCluster := &infrav1.VSphereCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-vsphere-cluster",
			Namespace: "test",
		},
		Spec: infrav1.VSphereClusterSpec{
			IdentityRef: &infrav1.VSphereIdentityReference{
				Kind: infrav1.SecretKind,
				Name: secret.Name,
			},
		},
	}

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-cluster",
			Namespace: "test",
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: &corev1.ObjectReference{
				Name: vsphereCluster.Name,
			},
		},
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "test",
			Labels: map[string]string{
				clusterv1.ClusterLabelName: "valid-cluster",
			},
		},
	}

	initObjs := createMachineOwnerHierarchy(machine)

	vsphereMachine := &infrav1.VSphereMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo-vm",
			Namespace: "test",
			Labels: map[string]string{
				clusterv1.ClusterLabelName: "valid-cluster",
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "foo"}},
		},
	}

	vsphereVM := &infrav1.VSphereVM{
		TypeMeta: metav1.TypeMeta{
			Kind: "VSphereVM",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "DC0_C0_RP0_VM0",
			Namespace: "test",
			Labels: map[string]string{
				clusterv1.ClusterLabelName: "valid-cluster",
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: infrav1.GroupVersion.String(), Kind: "VSphereMachine", Name: "foo-vm"}},
			// To make sure PatchHelper does not error out
			ResourceVersion: "1234",
		},
		Spec: infrav1.VSphereVMSpec{
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				Server:     simr.ServerURL().Host,
				Datacenter: "DC0",
				Network: infrav1.NetworkSpec{
					Devices: []infrav1.NetworkDeviceSpec{
						{
							NetworkName: "nw-1",
							DHCP4:       true,
							DHCP4Overrides: &infrav1.DHCPOverrides{
								UseDNS: toBoolPtr(false),
							},
						},
					},
				},
			},
		},
		Status: infrav1.VSphereVMStatus{},
	}

	initObjs = append(initObjs, secret, vsphereVM, vsphereMachine, machine, cluster, vsphereCluster)
	controllerMgrContext := fake.NewControllerManagerContext(initObjs...)

	controllerContext := &context.ControllerContext{
		ControllerManagerContext: controllerMgrContext,
		Recorder:                 record.New(apirecord.NewFakeRecorder(100)),
		Logger:                   log.Log,
	}
	r := vmReconciler{
		ControllerContext: controllerContext,
		VMService:         &govmomi.VMService{},
	}

	g := NewWithT(t)
	_, err = r.Reconcile(goctx.Background(), ctrl.Request{NamespacedName: util.ObjectKey(vsphereVM)})
	g.Expect(err).NotTo(HaveOccurred())

	c, _ := govc.NewClient(ctx, simr.ServerURL(), true)
	var obj mo.VirtualMachine
	ref := vtypes.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: "vm-44",
	}
	err = c.RetrieveOne(goctx.Background(), ref, []string{"config.extraConfig"}, &obj)
	g.Expect(err).NotTo(HaveOccurred())
	expected := `
instance-id: "DC0_C0_RP0_VM0"
local-hostname: "DC0_C0_RP0_VM0"
wait-on-network:
  ipv4: true
  ipv6: false
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:0c:29:33:34:38"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: true
      dhcp4-overrides:
        use-dns: false
      dhcp6: false
`
	found := false
	for _, ec := range obj.Config.ExtraConfig {
		opt := ec.GetOptionValue()
		if opt.Key == "guestinfo.metadata" {
			found = true
			v, _ := opt.Value.(string)
			metadata, err := base64.StdEncoding.DecodeString(v)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(metadata)).To(Equal(expected))
		}
	}
	g.Expect(found).To(BeTrue(), "did not find \"guestinfo.metadata\" on the vSphereVM")
	// TODO: Should probably check both VMs have override
}

func createMachineOwnerHierarchy(machine *clusterv1.Machine) []client.Object {
	machine.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "MachineSet",
			Name:       fmt.Sprintf("%s-ms", machine.Name),
		},
	}

	var (
		objs           []client.Object
		clusterName, _ = machine.Labels[clusterv1.ClusterLabelName]
	)

	objs = append(objs, &clusterv1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-ms", machine.Name),
			Namespace: machine.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterLabelName: clusterName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "MachineDeployment",
					Name:       fmt.Sprintf("%s-md", machine.Name),
				},
			},
		},
	})

	objs = append(objs, &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-md", machine.Name),
			Namespace: machine.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterLabelName: clusterName,
			},
		},
	})
	return objs
}

func toBoolPtr(b bool) *bool {
	return &b
}
