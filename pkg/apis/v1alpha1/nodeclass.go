// Package v1alpha1 defines the EdgeCenter infrastructure template.
package v1alpha1

import (
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/awslabs/operatorpkg/status"
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
type EdgeCenterNodeClassSpec struct {
	ClusterID        string      `json:"clusterID"`
	ProjectID        int         `json:"projectID"`
	RegionID         int         `json:"regionID"`
	Region           string      `json:"region"`
	NetworkID        string      `json:"networkID"`
	ImageID          string      `json:"imageID"`
	ServerGroupID    string      `json:"serverGroupID,omitempty"`
	SecurityGroupIDs []string    `json:"securityGroupIDs"`
	Flavors          []Flavor    `json:"flavors"`
	BootSizeGiB      int         `json:"bootSizeGiB"`
	BootType         string      `json:"bootType"`
	Addresses        AddressPool `json:"addresses"`
	Gateways         []Gateway   `json:"gateways"`
	// All secret references use the operator namespace.
	Credentials SecretRef `json:"credentials"`
	Talos       SecretRef `json:"talos"`
	SSH         SecretRef `json:"ssh"`
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
	if len(s.Flavors) == 0 || len(s.Gateways) == 0 || len(s.SecurityGroupIDs) == 0 || s.BootSizeGiB < 10 {
		return fmt.Errorf("flavors, gateways, security groups and boot disk are required")
	}
	for _, f := range s.Flavors {
		if f.Name == "" || f.CPU < 1 || f.MemoryGiB < 1 || f.Price <= 0 {
			return fmt.Errorf("each flavor requires a name, resources and a positive price")
		}
	}
	if s.Credentials.Name == "" || s.Talos.Name == "" || s.SSH.Name == "" {
		return fmt.Errorf("secret references are required")
	}
	return nil
}
