// Copyright (c) 2018-2022 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package network

import (
	goctx "context"
	"fmt"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/pkg/errors"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ncpv1alpha1 "github.com/vmware-tanzu/vm-operator/external/ncp/api/v1alpha1"

	vmopv1alpha1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"

	netopv1alpha1 "github.com/vmware-tanzu/vm-operator/external/net-operator/api/v1alpha1"
	"github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/vmprovider/providers/vsphere/constants"
)

// IPFamily represents the IP Family (IPv4 or IPv6). This type is used
// to express the family of an IP expressed by a type (i.e. service.Spec.IPFamily)
// NOTE: Copied from k8s.io/api/core/v1" because VM Operator is using old version.
type IPFamily string

const (
	// IPv4Protocol indicates that this IP is IPv4 protocol.
	IPv4Protocol IPFamily = "IPv4"
	// IPv6Protocol indicates that this IP is IPv6 protocol.
	IPv6Protocol IPFamily = "IPv6"
)

// IPConfig represents an IP configuration.
type IPConfig struct {
	// IP setting.
	IP string
	// IPFamily specifies the IP family (IPv4 vs IPv6) the IP belongs to.
	IPFamily IPFamily
	// Gateway setting.
	Gateway string
	// SubnetMask setting.
	SubnetMask string
}

const (
	NsxtNetworkType = "nsx-t"
	VdsNetworkType  = "vsphere-distributed"

	defaultEthernetCardType = "vmxnet3"
	retryInterval           = 100 * time.Millisecond
	retryTimeout            = 15 * time.Second
)

type InterfaceInfo struct {
	Device          vimtypes.BaseVirtualDevice
	Customization   *vimtypes.CustomizationAdapterMapping
	IPConfiguration IPConfig
	NetplanEthernet NetplanEthernet
}

type InterfaceInfoList []InterfaceInfo

func (l InterfaceInfoList) GetVirtualDeviceList() object.VirtualDeviceList {
	var devList object.VirtualDeviceList
	for _, info := range l {
		devList = append(devList, info.Device)
	}
	return devList
}

// Netplan representation described in https://via.vmw.com/cloud-init-netplan
type Netplan struct {
	Version   int                        `yaml:"version,omitempty"`
	Ethernets map[string]NetplanEthernet `yaml:"ethernets,omitempty"`
}
type NetplanEthernet struct {
	Match       NetplanEthernetMatch      `yaml:"match,omitempty"`
	SetName     string                    `yaml:"set-name,omitempty"`
	Dhcp4       bool                      `yaml:"dhcp4,omitempty"`
	Addresses   []string                  `yaml:"addresses,omitempty"`
	Gateway4    string                    `yaml:"gateway4,omitempty"`
	Nameservers NetplanEthernetNameserver `yaml:"nameservers,omitempty"`
}
type NetplanEthernetMatch struct {
	MacAddress string `yaml:"macaddress,omitempty"`
}
type NetplanEthernetNameserver struct {
	Addresses []string `yaml:"addresses,omitempty"`
	Search    []string `yaml:"search,omitempty"`
}

func (l InterfaceInfoList) GetNetplan(
	currentEthCards object.VirtualDeviceList,
	dnsServers, searchSuffixes []string) Netplan {

	ethernets := make(map[string]NetplanEthernet)

	for index, info := range l {
		netplanEthernet := info.NetplanEthernet

		if netplanEthernet.Match.MacAddress == "" && len(currentEthCards) == 1 {
			curNic := currentEthCards[0].(vimtypes.BaseVirtualEthernetCard).GetVirtualEthernetCard()
			// This assumes we don't have multiple NICs in the same backing network. This is kind of, sort
			// of enforced by the webhook, but we lack a guaranteed way to match up the NICs.

			// NetOp (VDS) never assigns MacAddress to the NetworkInterface status, therefore
			// netplanEthernet.Match.MacAddress will be empty.
			// At this point, it is assumed that VirtualMachine.Config.Hardware.Device has MacAddress generated.
			netplanEthernet.Match.MacAddress = NormalizeNetplanMac(curNic.GetVirtualEthernetCard().MacAddress)
		}

		// Inject nameserver settings for each ethernet.
		netplanEthernet.Nameservers.Addresses = dnsServers
		netplanEthernet.Nameservers.Search = searchSuffixes
		name := fmt.Sprintf("eth%d", index)
		netplanEthernet.SetName = name
		ethernets[name] = netplanEthernet
	}

	return Netplan{
		Version:   constants.NetPlanVersion,
		Ethernets: ethernets,
	}
}

func (l InterfaceInfoList) GetInterfaceCustomizations() []vimtypes.CustomizationAdapterMapping {
	mappings := make([]vimtypes.CustomizationAdapterMapping, 0, len(l))
	for _, info := range l {
		mappings = append(mappings, *info.Customization)
	}
	return mappings
}

func (l InterfaceInfoList) GetIPConfigs() []IPConfig {
	ipConfigs := make([]IPConfig, 0, len(l))
	for _, info := range l {
		ipConfigs = append(ipConfigs, info.IPConfiguration)
	}
	return ipConfigs
}

// Provider sets up network for different type of network.
type Provider interface {
	// EnsureNetworkInterface returns the NetworkInterfaceInfo for the vif.
	EnsureNetworkInterface(vmCtx context.VirtualMachineContext, vif *vmopv1alpha1.VirtualMachineNetworkInterface) (*InterfaceInfo, error)
}

type networkProvider struct {
	nsxt  Provider
	netOp Provider
	named Provider

	scheme *runtime.Scheme
}

func NewProvider(
	k8sClient ctrlruntime.Client,
	vimClient *vim25.Client,
	finder *find.Finder,
	cluster *object.ClusterComputeResource) Provider {

	return &networkProvider{
		nsxt:   newNsxtNetworkProvider(k8sClient, finder, cluster),
		netOp:  newNetOpNetworkProvider(k8sClient, vimClient, finder, cluster),
		named:  newNamedNetworkProvider(finder),
		scheme: k8sClient.Scheme(),
	}
}

func (np *networkProvider) EnsureNetworkInterface(vmCtx context.VirtualMachineContext, vif *vmopv1alpha1.VirtualMachineNetworkInterface) (*InterfaceInfo, error) {
	if providerRef := vif.ProviderRef; providerRef != nil {
		// ProviderRef is only supported for NetOP types.
		gvk, err := apiutil.GVKForObject(&netopv1alpha1.NetworkInterface{}, np.scheme)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot get GroupVersionKind for NetworkInterface object")
		}

		if gvk.Group != providerRef.APIGroup || gvk.Version != providerRef.APIVersion || gvk.Kind != providerRef.Kind {
			err := fmt.Errorf("unsupported NetworkInterface ProviderRef: %+v Supported: %+v", providerRef, gvk)
			return nil, err
		}

		return np.netOp.EnsureNetworkInterface(vmCtx, vif)
	}

	switch vif.NetworkType {
	case NsxtNetworkType:
		return np.nsxt.EnsureNetworkInterface(vmCtx, vif)
	case VdsNetworkType:
		return np.netOp.EnsureNetworkInterface(vmCtx, vif)
	case "":
		return np.named.EnsureNetworkInterface(vmCtx, vif)
	default:
		return nil, fmt.Errorf("failed to create network provider for network type %q", vif.NetworkType)
	}
}

// createEthernetCard creates an ethernet card with the network reference backing.
func createEthernetCard(ctx goctx.Context, network object.NetworkReference, ethCardType string) (vimtypes.BaseVirtualDevice, error) {
	if ethCardType == "" {
		ethCardType = defaultEthernetCardType
	}

	backing, err := network.EthernetCardBackingInfo(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get ethernet card backing info for network %v", network.Reference())
	}

	dev, err := object.EthernetCardTypes().CreateEthernetCard(ethCardType, backing)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create ethernet card %q for network %v", ethCardType, network.Reference())
	}

	return dev, nil
}

func configureEthernetCard(ethDev vimtypes.BaseVirtualDevice, externalID, macAddress string) {
	card := ethDev.(vimtypes.BaseVirtualEthernetCard).GetVirtualEthernetCard()

	card.ExternalId = externalID
	if macAddress != "" {
		card.MacAddress = macAddress
		card.AddressType = string(vimtypes.VirtualEthernetCardMacTypeManual)
	} else {
		card.AddressType = string(vimtypes.VirtualEthernetCardMacTypeGenerated)
	}
}

func newNamedNetworkProvider(finder *find.Finder) *namedNetworkProvider {
	return &namedNetworkProvider{
		finder: finder,
	}
}

type namedNetworkProvider struct {
	finder *find.Finder
}

func (np *namedNetworkProvider) EnsureNetworkInterface(
	vmCtx context.VirtualMachineContext,
	vif *vmopv1alpha1.VirtualMachineNetworkInterface) (*InterfaceInfo, error) {

	networkRef, err := np.finder.Network(vmCtx, vif.NetworkName)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to find network %q", vif.NetworkName)
	}

	ethDev, err := createEthernetCard(vmCtx, networkRef, vif.EthernetCardType)
	if err != nil {
		return nil, err
	}

	return &InterfaceInfo{
		Device: ethDev,
		Customization: &vimtypes.CustomizationAdapterMapping{
			Adapter: vimtypes.CustomizationIPSettings{
				Ip: &vimtypes.CustomizationDhcpIpGenerator{},
			},
		},
		IPConfiguration: IPConfig{},
		NetplanEthernet: NetplanEthernet{},
	}, nil
}

// +kubebuilder:rbac:groups=netoperator.vmware.com,resources=networkinterfaces;vmxnet3networkinterfaces,verbs=get;list;watch;create;update;patch;delete

// newNetOpNetworkProvider returns a netOpNetworkProvider instance.
func newNetOpNetworkProvider(
	k8sClient ctrlruntime.Client,
	vimClient *vim25.Client,
	finder *find.Finder,
	cluster *object.ClusterComputeResource) *netOpNetworkProvider {

	return &netOpNetworkProvider{
		k8sClient: k8sClient,
		scheme:    k8sClient.Scheme(),
		vimClient: vimClient,
		finder:    finder,
		cluster:   cluster,
	}
}

type netOpNetworkProvider struct {
	k8sClient ctrlruntime.Client
	scheme    *runtime.Scheme
	vimClient *vim25.Client
	finder    *find.Finder
	cluster   *object.ClusterComputeResource
}

// BMV: This is similar to what NSX does but isn't really right: we can only have one
// interface per network. Although if we had multiple interfaces per network, we really
// don't have a way to identify each NIC so true reconciliation is broken.
// If networkName is not specified, use vm name instead.
func (np *netOpNetworkProvider) networkInterfaceName(networkName, vmName string) string {
	if networkName != "" {
		return fmt.Sprintf("%s-%s", networkName, vmName)
	}
	return vmName
}

// createNetworkInterface creates a NetOP NetworkInterface for the VM network interface.
func (np *netOpNetworkProvider) createNetworkInterface(
	vmCtx context.VirtualMachineContext,
	vmIf *vmopv1alpha1.VirtualMachineNetworkInterface) (*netopv1alpha1.NetworkInterface, error) {

	if vmIf.ProviderRef == nil {
		// Create or Update our NetworkInterface CR when ProviderRef is unset.
		netIf := &netopv1alpha1.NetworkInterface{
			ObjectMeta: metav1.ObjectMeta{
				Name:      np.networkInterfaceName(vmIf.NetworkName, vmCtx.VM.Name),
				Namespace: vmCtx.VM.Namespace,
			},
		}

		// The only type defined by NetOP (but it doesn't care).
		cardType := netopv1alpha1.NetworkInterfaceTypeVMXNet3

		_, err := controllerutil.CreateOrUpdate(vmCtx, np.k8sClient, netIf, func() error {
			if err := controllerutil.SetOwnerReference(vmCtx.VM, netIf, np.scheme); err != nil {
				return err
			}

			netIf.Spec = netopv1alpha1.NetworkInterfaceSpec{
				NetworkName: vmIf.NetworkName,
				Type:        cardType,
			}
			return nil
		})

		if err != nil {
			return nil, err
		}
	}

	return np.waitForReadyNetworkInterface(vmCtx, vmIf)
}

func (np *netOpNetworkProvider) networkForPortGroupID(portGroupID string) (object.NetworkReference, error) {
	pgObjRef := vimtypes.ManagedObjectReference{
		Type:  "DistributedVirtualPortgroup",
		Value: portGroupID,
	}

	return object.NewDistributedVirtualPortgroup(np.vimClient, pgObjRef), nil
}

func (np *netOpNetworkProvider) getNetworkRef(ctx goctx.Context, networkType, networkID string) (object.NetworkReference, error) {
	switch networkType {
	case VdsNetworkType:
		return np.networkForPortGroupID(networkID)
	case NsxtNetworkType:
		return searchNsxtNetworkReference(ctx, np.cluster, networkID)
	default:
		return nil, fmt.Errorf("unsupported NetOP network type %s", networkType)
	}
}

func (np *netOpNetworkProvider) createEthernetCard(
	vmCtx context.VirtualMachineContext,
	vif *vmopv1alpha1.VirtualMachineNetworkInterface,
	netIf *netopv1alpha1.NetworkInterface) (vimtypes.BaseVirtualDevice, error) {

	networkRef, err := np.getNetworkRef(vmCtx, vif.NetworkType, netIf.Status.NetworkID)
	if err != nil {
		return nil, err
	}

	ethDev, err := createEthernetCard(vmCtx, networkRef, vif.EthernetCardType)
	if err != nil {
		return nil, err
	}

	configureEthernetCard(ethDev, netIf.Status.ExternalID, netIf.Status.MacAddress)

	return ethDev, nil
}

func (np *netOpNetworkProvider) waitForReadyNetworkInterface(
	vmCtx context.VirtualMachineContext,
	vmIf *vmopv1alpha1.VirtualMachineNetworkInterface) (*netopv1alpha1.NetworkInterface, error) {

	var name string
	if vmIf.ProviderRef != nil {
		name = vmIf.ProviderRef.Name
	} else {
		name = np.networkInterfaceName(vmIf.NetworkName, vmCtx.VM.Name)
	}

	var netIf *netopv1alpha1.NetworkInterface
	netIfKey := types.NamespacedName{Namespace: vmCtx.VM.Namespace, Name: name}

	// TODO: Watch() this type instead.
	err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		instance := &netopv1alpha1.NetworkInterface{}
		if err := np.k8sClient.Get(vmCtx, netIfKey, instance); err != nil {
			return false, ctrlruntime.IgnoreNotFound(err)
		}

		for _, cond := range instance.Status.Conditions {
			if cond.Type == netopv1alpha1.NetworkInterfaceReady && cond.Status == corev1.ConditionTrue {
				netIf = instance
				return true, nil
			}
		}

		return false, nil
	})

	return netIf, err
}

func (np *netOpNetworkProvider) goscCustomization(netIf *netopv1alpha1.NetworkInterface) *vimtypes.CustomizationAdapterMapping {
	var adapter *vimtypes.CustomizationIPSettings

	if len(netIf.Status.IPConfigs) == 0 {
		adapter = &vimtypes.CustomizationIPSettings{
			Ip: &vimtypes.CustomizationDhcpIpGenerator{},
		}
	} else {
		switch ipConfig := netIf.Status.IPConfigs[0]; ipConfig.IPFamily {
		case netopv1alpha1.IPv4Protocol:
			adapter = &vimtypes.CustomizationIPSettings{
				Ip:         &vimtypes.CustomizationFixedIp{IpAddress: ipConfig.IP},
				SubnetMask: ipConfig.SubnetMask,
				Gateway:    []string{ipConfig.Gateway},
			}
		case netopv1alpha1.IPv6Protocol:
			subnetMask := net.ParseIP(ipConfig.SubnetMask)
			var ipMask net.IPMask = make([]byte, net.IPv6len)
			copy(ipMask, subnetMask)
			ones, _ := ipMask.Size()

			adapter = &vimtypes.CustomizationIPSettings{
				IpV6Spec: &vimtypes.CustomizationIPSettingsIpV6AddressSpec{
					Ip: []vimtypes.BaseCustomizationIpV6Generator{
						&vimtypes.CustomizationFixedIpV6{
							IpAddress:  ipConfig.IP,
							SubnetMask: int32(ones),
						},
					},
					Gateway: []string{ipConfig.Gateway},
				},
			}
		default:
			adapter = &vimtypes.CustomizationIPSettings{}
		}
	}

	// Note that NetOP VDS doesn't current specify the MacAddress (we have VC generate it), so we
	// rely on the customization order matching the sorted bus order that GOSC does. This is quite
	// brittle, and something we're going to need to revisit. Assuming Reconfigure() generates the
	// MacAddress, we could later fix up the MacAddress, but interface matching is not straight
	// forward either (see reconcileVMNicDeviceChanges()).
	return &vimtypes.CustomizationAdapterMapping{
		MacAddress: netIf.Status.MacAddress,
		Adapter:    *adapter,
	}
}

func (np *netOpNetworkProvider) EnsureNetworkInterface(
	vmCtx context.VirtualMachineContext,
	vif *vmopv1alpha1.VirtualMachineNetworkInterface) (*InterfaceInfo, error) {

	netIf, err := np.createNetworkInterface(vmCtx, vif)
	if err != nil {
		return nil, err
	}

	ethDev, err := np.createEthernetCard(vmCtx, vif, netIf)
	if err != nil {
		return nil, err
	}

	return &InterfaceInfo{
		Device:          ethDev,
		Customization:   np.goscCustomization(netIf),
		IPConfiguration: np.getIPConfig(netIf),
		NetplanEthernet: np.getNetplanEthernet(netIf),
	}, nil
}

func (np *netOpNetworkProvider) getIPConfig(netIf *netopv1alpha1.NetworkInterface) IPConfig {
	var ipConfig IPConfig
	if len(netIf.Status.IPConfigs) > 0 {
		ipConfig = IPConfig{
			IP:         netIf.Status.IPConfigs[0].IP,
			Gateway:    netIf.Status.IPConfigs[0].Gateway,
			SubnetMask: netIf.Status.IPConfigs[0].SubnetMask,
			IPFamily:   IPFamily(netIf.Status.IPConfigs[0].IPFamily),
		}
	}

	return ipConfig
}

func (np *netOpNetworkProvider) getNetplanEthernet(netIf *netopv1alpha1.NetworkInterface) NetplanEthernet {
	eth := NetplanEthernet{
		Match: NetplanEthernetMatch{
			MacAddress: NormalizeNetplanMac(netIf.Status.MacAddress),
		},
	}

	if len(netIf.Status.IPConfigs) == 0 {
		eth.Dhcp4 = true
	} else {
		ipAddr := netIf.Status.IPConfigs[0]
		eth.Addresses = []string{ToCidrNotation(ipAddr.IP, ipAddr.SubnetMask)}
		eth.Gateway4 = ipAddr.Gateway
	}

	return eth
}

type nsxtNetworkProvider struct {
	k8sClient ctrlruntime.Client
	finder    *find.Finder
	cluster   *object.ClusterComputeResource
	scheme    *runtime.Scheme
}

// newNsxtNetworkProvider returns a nsxtNetworkProvider instance.
func newNsxtNetworkProvider(
	client ctrlruntime.Client,
	finder *find.Finder,
	cluster *object.ClusterComputeResource) *nsxtNetworkProvider {

	return &nsxtNetworkProvider{
		k8sClient: client,
		finder:    finder,
		cluster:   cluster,
		scheme:    client.Scheme(),
	}
}

// virtualNetworkInterfaceName returns the VirtualNetworkInterface name for the VM.
func (np *nsxtNetworkProvider) virtualNetworkInterfaceName(networkName, vmName string) string {
	vnetifName := fmt.Sprintf("%s-lsp", vmName)
	if networkName != "" {
		vnetifName = fmt.Sprintf("%s-%s", networkName, vnetifName)
	}
	return vnetifName
}

// createVirtualNetworkInterface creates a NCP VirtualNetworkInterface for a given VM network interface.
func (np *nsxtNetworkProvider) createVirtualNetworkInterface(
	vmCtx context.VirtualMachineContext,
	vmIf *vmopv1alpha1.VirtualMachineNetworkInterface) (*ncpv1alpha1.VirtualNetworkInterface, error) {

	vnetIf := &ncpv1alpha1.VirtualNetworkInterface{
		ObjectMeta: metav1.ObjectMeta{
			Name:      np.virtualNetworkInterfaceName(vmIf.NetworkName, vmCtx.VM.Name),
			Namespace: vmCtx.VM.Namespace,
		},
	}

	result, err := controllerutil.CreateOrUpdate(vmCtx, np.k8sClient, vnetIf, func() error {
		if err := controllerutil.SetOwnerReference(vmCtx.VM, vnetIf, np.scheme); err != nil {
			return err
		}

		vnetIf.Spec.VirtualNetwork = vmIf.NetworkName
		return nil
	})

	if err != nil {
		return nil, err
	}

	if result == controllerutil.OperationResultCreated {
		vmCtx.Logger.Info("Successfully created VirtualNetworkInterface",
			"name", types.NamespacedName{Namespace: vnetIf.Namespace, Name: vnetIf.Name})
	}

	return np.waitForReadyVirtualNetworkInterface(vmCtx, vmIf)
}

func (np *nsxtNetworkProvider) createEthernetCard(
	vmCtx context.VirtualMachineContext,
	vif *vmopv1alpha1.VirtualMachineNetworkInterface,
	vnetIf *ncpv1alpha1.VirtualNetworkInterface) (vimtypes.BaseVirtualDevice, error) {

	if vnetIf.Status.ProviderStatus == nil || vnetIf.Status.ProviderStatus.NsxLogicalSwitchID == "" {
		err := fmt.Errorf("failed to get for nsx-t opaque network ID for vnetIf '%+v'", vnetIf)
		vmCtx.Logger.Error(err, "Ready VirtualNetworkInterface did not have expected Status.ProviderStatus")
		return nil, err
	}

	networkRef, err := searchNsxtNetworkReference(vmCtx, np.cluster, vnetIf.Status.ProviderStatus.NsxLogicalSwitchID)
	if err != nil {
		// Log message used by VMC LINT. Refer to before making changes
		vmCtx.Logger.Error(err, "Failed to search for nsx-t network associated with vnetIf", "vnetIf", vnetIf)
		return nil, err
	}

	ethDev, err := createEthernetCard(vmCtx, networkRef, vif.EthernetCardType)
	if err != nil {
		return nil, err
	}

	configureEthernetCard(ethDev, vnetIf.Status.InterfaceID, vnetIf.Status.MacAddress)

	return ethDev, nil
}

func (np *nsxtNetworkProvider) waitForReadyVirtualNetworkInterface(
	vmCtx context.VirtualMachineContext,
	vmIf *vmopv1alpha1.VirtualMachineNetworkInterface) (*ncpv1alpha1.VirtualNetworkInterface, error) {

	vnetIfName := np.virtualNetworkInterfaceName(vmIf.NetworkName, vmCtx.VM.Name)

	var vnetIf *ncpv1alpha1.VirtualNetworkInterface
	vnetIfKey := types.NamespacedName{Namespace: vmCtx.VM.Namespace, Name: vnetIfName}

	// TODO: Watch() this type instead.
	err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		instance := &ncpv1alpha1.VirtualNetworkInterface{}
		if err := np.k8sClient.Get(vmCtx, vnetIfKey, instance); err != nil {
			return false, ctrlruntime.IgnoreNotFound(err)
		}

		for _, condition := range instance.Status.Conditions {
			if strings.Contains(condition.Type, "Ready") && strings.Contains(condition.Status, "True") {
				vnetIf = instance
				return true, nil
			}
		}

		return false, nil
	})

	return vnetIf, err
}

func (np *nsxtNetworkProvider) goscCustomization(vnetIf *ncpv1alpha1.VirtualNetworkInterface) *vimtypes.CustomizationAdapterMapping {
	var adapter *vimtypes.CustomizationIPSettings

	addrs := vnetIf.Status.IPAddresses
	if len(addrs) == 0 || (len(addrs) == 1 && addrs[0].IP == "") {
		adapter = &vimtypes.CustomizationIPSettings{
			Ip: &vimtypes.CustomizationDhcpIpGenerator{},
		}
	} else {
		ipAddr := addrs[0]
		adapter = &vimtypes.CustomizationIPSettings{
			Ip:         &vimtypes.CustomizationFixedIp{IpAddress: ipAddr.IP},
			SubnetMask: ipAddr.SubnetMask,
			Gateway:    []string{ipAddr.Gateway},
		}
	}

	return &vimtypes.CustomizationAdapterMapping{
		MacAddress: vnetIf.Status.MacAddress,
		Adapter:    *adapter,
	}
}

func (np *nsxtNetworkProvider) EnsureNetworkInterface(
	vmCtx context.VirtualMachineContext,
	vif *vmopv1alpha1.VirtualMachineNetworkInterface) (*InterfaceInfo, error) {

	vnetIf, err := np.createVirtualNetworkInterface(vmCtx, vif)
	if err != nil {
		vmCtx.Logger.Error(err, "Failed to create vnetIf for vif", "vif", vif)
		return nil, err
	}

	ethDev, err := np.createEthernetCard(vmCtx, vif, vnetIf)
	if err != nil {
		return nil, err
	}

	return &InterfaceInfo{
		Device:          ethDev,
		Customization:   np.goscCustomization(vnetIf),
		IPConfiguration: np.getIPConfig(vnetIf),
		NetplanEthernet: np.getNetplanEthernet(vnetIf),
	}, nil
}

func (np *nsxtNetworkProvider) getIPConfig(vnetIf *ncpv1alpha1.VirtualNetworkInterface) IPConfig {
	var ipConfig IPConfig
	if len(vnetIf.Status.IPAddresses) > 0 {
		ipAddr := vnetIf.Status.IPAddresses[0]
		ipConfig.IP = ipAddr.IP
		ipConfig.Gateway = ipAddr.Gateway
		ipConfig.SubnetMask = ipAddr.SubnetMask
		ipConfig.IPFamily = IPv4Protocol
	}

	return ipConfig
}

func (np *nsxtNetworkProvider) getNetplanEthernet(vnetIf *ncpv1alpha1.VirtualNetworkInterface) NetplanEthernet {
	eth := NetplanEthernet{
		Match: NetplanEthernetMatch{
			MacAddress: NormalizeNetplanMac(vnetIf.Status.MacAddress),
		},
	}

	addrs := vnetIf.Status.IPAddresses
	if len(addrs) == 0 || (len(addrs) == 1 && addrs[0].IP == "") {
		eth.Dhcp4 = true
	} else {
		ipAddr := addrs[0]
		eth.Addresses = []string{ToCidrNotation(ipAddr.IP, ipAddr.SubnetMask)}
		eth.Gateway4 = ipAddr.Gateway
	}

	return eth
}

// searchNsxtNetworkReference takes in nsx-t logical switch UUID and returns the reference of the network.
func searchNsxtNetworkReference(
	ctx goctx.Context,
	ccr *object.ClusterComputeResource,
	networkID string) (object.NetworkReference, error) {

	var obj mo.ClusterComputeResource
	if err := ccr.Properties(ctx, ccr.Reference(), []string{"network"}, &obj); err != nil {
		return nil, err
	}

	var dvpgsMoRefs []vimtypes.ManagedObjectReference
	for _, n := range obj.Network {
		if n.Type == "DistributedVirtualPortgroup" {
			dvpgsMoRefs = append(dvpgsMoRefs, n.Reference())
		}
	}

	if len(dvpgsMoRefs) == 0 {
		return nil, fmt.Errorf("ClusterComputeResource %s has no DVPGs", ccr.Reference().Value)
	}

	var dvpgs []mo.DistributedVirtualPortgroup
	err := property.DefaultCollector(ccr.Client()).Retrieve(ctx, dvpgsMoRefs, []string{"config.logicalSwitchUuid"}, &dvpgs)
	if err != nil {
		return nil, err
	}

	var dvpgMoRefs []vimtypes.ManagedObjectReference
	for _, dvpg := range dvpgs {
		if dvpg.Config.LogicalSwitchUuid == networkID {
			dvpgMoRefs = append(dvpgMoRefs, dvpg.Reference())
		}
	}

	switch len(dvpgMoRefs) {
	case 1:
		return object.NewDistributedVirtualPortgroup(ccr.Client(), dvpgMoRefs[0]), nil
	case 0:
		return nil, fmt.Errorf("no DVPG with NSX-T network ID %q found", networkID)
	default:
		// The LogicalSwitchUuid is supposed to be unique per CCR, so this is likely an NCP
		// misconfiguration, and we don't know which one to pick.
		return nil, fmt.Errorf("multiple DVPGs (%d) with NSX-T network ID %q found", len(dvpgMoRefs), networkID)
	}
}

// ToCidrNotation takes ip and mask as ip addresses and returns a cidr notation.
// It Assumes ipv4.
func ToCidrNotation(ip string, mask string) string {
	IPNet := net.IPNet{
		IP:   net.ParseIP(ip).To4(),
		Mask: net.IPMask(net.ParseIP(mask).To4()),
	}
	return IPNet.String()
}

// NormalizeNetplanMac normalizes the mac address format to one compatible with netplan.
func NormalizeNetplanMac(mac string) string {
	if len(mac) == 0 {
		return mac
	}
	mac = strings.ReplaceAll(mac, "-", ":")
	mac = strings.ToLower(mac)
	return mac
}
