// Package v1alpha1 defines the EdgeCenter infrastructure template.
package v1alpha1

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"

	"github.com/awslabs/operatorpkg/status"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const Group = "karpenter.edgecenter.ru"

var GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}

type SecretRef struct {
	Name string `json:"name"`
}
type Flavor struct {
	Name      string `json:"name"`
	CPU       int64  `json:"cpu"`
	MemoryGiB int64  `json:"memoryGiB"`
	GPU       int64  `json:"gpu,omitempty"`
	// Price is the hourly price in one currency shared by all NodeClasses.
	Price float64 `json:"price"`
}
type AddressPool struct {
	CIDR             string `json:"cidr"`
	HostOffset       int    `json:"hostOffset"`
	Start            int    `json:"start"`
	End              int    `json:"end"`
	KubeSpanPortBase int    `json:"kubeSpanPortBase"`
}
type Gateway struct {
	Address  string `json:"address"`
	User     string `json:"user"`
	PublicIP string `json:"publicIP"`
}
type BareMetalStorage struct {
	EphemeralSizeGiB int    `json:"ephemeralSizeGiB"`
	LocalVolumeName  string `json:"localVolumeName"`
	// DataDiskVolumeName, when set, mounts the first non-system disk at /var/mnt/<name>.
	DataDiskVolumeName string `json:"dataDiskVolumeName,omitempty"`
}

var localVolumeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,33}$`)

type EdgeCenterNodeClassSpec struct {
	// Empty selects VirtualMachine for compatibility with existing NodeClasses.
	ComputeType string `json:"computeType,omitempty"`
	// UserData uses the OpenStack config drive and public Talos mTLS endpoints.
	// Empty preserves the original SSHGateway transport.
	BootstrapMode    string            `json:"bootstrapMode,omitempty"`
	TalosEndpoints   []string          `json:"talosEndpoints,omitempty"`
	ClusterID        string            `json:"clusterID"`
	ProjectID        int               `json:"projectID"`
	RegionID         int               `json:"regionID"`
	Region           string            `json:"region"`
	NetworkID        string            `json:"networkID"`
	ImageID          string            `json:"imageID"`
	ServerGroupID    string            `json:"serverGroupID,omitempty"`
	SecurityGroupIDs []string          `json:"securityGroupIDs,omitempty"`
	Flavors          []Flavor          `json:"flavors"`
	BootSizeGiB      int               `json:"bootSizeGiB"`
	BootType         string            `json:"bootType,omitempty"`
	BareMetalStorage *BareMetalStorage `json:"bareMetalStorage,omitempty"`
	Addresses        AddressPool       `json:"addresses"`
	Gateways         []Gateway         `json:"gateways"`
	// All secret references use the operator namespace.
	Credentials  SecretRef `json:"credentials"`
	Talos        SecretRef `json:"talos"`
	TalosPatches []string  `json:"talosPatches,omitempty"`
	SSH          SecretRef `json:"ssh,omitempty"`
}
type EdgeCenterNodeClassStatus struct {
	Conditions []status.Condition `json:"conditions,omitempty"`
}
type EdgeCenterNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EdgeCenterNodeClassSpec   `json:"spec"`
	Status            EdgeCenterNodeClassStatus `json:"status,omitempty"`
}
type EdgeCenterNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EdgeCenterNodeClass `json:"items"`
}

func (n *EdgeCenterNodeClass) GetConditions() []status.Condition  { return n.Status.Conditions }
func (n *EdgeCenterNodeClass) SetConditions(c []status.Condition) { n.Status.Conditions = c }
func (n *EdgeCenterNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions().For(n, opts...)
}
func (n *EdgeCenterNodeClass) DeepCopy() *EdgeCenterNodeClass {
	if n == nil {
		return nil
	}
	out := new(EdgeCenterNodeClass)
	b, _ := json.Marshal(n)
	_ = json.Unmarshal(b, out)
	return out
}
func (n *EdgeCenterNodeClass) DeepCopyObject() runtime.Object { return n.DeepCopy() }
func (n *EdgeCenterNodeClassList) DeepCopyObject() runtime.Object {
	if n == nil {
		return nil
	}
	out := new(EdgeCenterNodeClassList)
	b, _ := json.Marshal(n)
	_ = json.Unmarshal(b, out)
	return out
}
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &EdgeCenterNodeClass{}, &EdgeCenterNodeClassList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

func (s EdgeCenterNodeClassSpec) IP(slot int) (string, error) {
	p, err := netip.ParsePrefix(s.Addresses.CIDR)
	if err != nil || !p.Addr().Is4() {
		return "", fmt.Errorf("addresses.cidr must be IPv4")
	}
	a := p.Masked().Addr()
	offset := s.Addresses.HostOffset + slot
	if offset < 1 || offset > 65534 {
		return "", fmt.Errorf("invalid host offset")
	}
	for i := 0; i < offset; i++ {
		a = a.Next()
	}
	if !p.Contains(a) || !p.Contains(a.Next()) {
		return "", fmt.Errorf("slot is outside usable subnet addresses")
	}
	return a.String(), nil
}
func (s EdgeCenterNodeClassSpec) Validate() error {
	if b := s.BareMetalStorage; b != nil {
		if s.ComputeType != "BareMetal" {
			return fmt.Errorf("bareMetalStorage requires computeType BareMetal")
		}
		if b.EphemeralSizeGiB < 10 || b.EphemeralSizeGiB > s.BootSizeGiB-5 {
			return fmt.Errorf("bareMetalStorage.ephemeralSizeGiB must be at least 10 and leave 5 GiB for system partitions and local data")
		}
		if !localVolumeNamePattern.MatchString(b.LocalVolumeName) {
			return fmt.Errorf("bareMetalStorage.localVolumeName must match %s", localVolumeNamePattern)
		}
		if b.DataDiskVolumeName != "" && !localVolumeNamePattern.MatchString(b.DataDiskVolumeName) {
			return fmt.Errorf("bareMetalStorage.dataDiskVolumeName must match %s", localVolumeNamePattern)
		}
		if b.DataDiskVolumeName == b.LocalVolumeName {
			return fmt.Errorf("bareMetalStorage.dataDiskVolumeName must differ from localVolumeName")
		}
	}
	if s.ComputeType != "" && s.ComputeType != "VirtualMachine" && s.ComputeType != "BareMetal" {
		return fmt.Errorf("computeType must be VirtualMachine or BareMetal")
	}
	if s.ComputeType == "BareMetal" && (len(s.SecurityGroupIDs) != 0 || s.ServerGroupID != "" || s.BootType != "") {
		return fmt.Errorf("BareMetal uses local disks and does not accept VM security groups, server groups or bootType")
	}
	if s.ClusterID == "" || s.ProjectID <= 0 || s.RegionID <= 0 || s.Region == "" || s.NetworkID == "" || s.ImageID == "" {
		return fmt.Errorf("cluster, project, region, network and image are required")
	}
	if s.Addresses.Start < 0 || s.Addresses.End < s.Addresses.Start || s.Addresses.End-s.Addresses.Start > 255 {
		return fmt.Errorf("invalid address slot range")
	}
	if _, err := s.IP(s.Addresses.End); err != nil {
		return err
	}
	if s.Addresses.KubeSpanPortBase+s.Addresses.Start < 1024 || s.Addresses.KubeSpanPortBase+s.Addresses.End > 65535 {
		return fmt.Errorf("invalid KubeSpan port range")
	}
	if len(s.Flavors) == 0 || len(s.Gateways) == 0 || (s.ComputeType != "BareMetal" && (len(s.SecurityGroupIDs) == 0 || s.BootType == "")) || s.BootSizeGiB < 10 {
		return fmt.Errorf("flavors, gateways, security groups and boot disk are required")
	}
	for _, f := range s.Flavors {
		if f.Name == "" || f.CPU < 1 || f.MemoryGiB < 1 || f.GPU < 0 || f.Price <= 0 {
			return fmt.Errorf("each flavor requires a name, resources and a positive price")
		}
	}
	for i, patch := range s.TalosPatches {
		if _, err := configpatcher.LoadPatch([]byte(patch)); err != nil {
			return fmt.Errorf("talosPatches[%d] is not a valid Talos config patch: %w", i, err)
		}
	}
	if s.BootstrapMode != "" && s.BootstrapMode != "SSHGateway" && s.BootstrapMode != "UserData" {
		return fmt.Errorf("bootstrapMode must be UserData or SSHGateway")
	}
	if s.BootstrapMode == "UserData" && len(s.TalosEndpoints) == 0 {
		return fmt.Errorf("UserData requires public Talos endpoints")
	}
	if s.Credentials.Name == "" || s.Talos.Name == "" || (s.BootstrapMode != "UserData" && s.SSH.Name == "") {
		return fmt.Errorf("secret references are required")
	}
	return nil
}
