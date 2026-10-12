package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/awslabs/operatorpkg/status"
	api "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/apis/v1alpha1"
	"github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/bootstrap"
	ec "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/edgecenter"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	cp "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const managedLabel = api.Group + "/cluster"
const hashAnnotation = api.Group + "/class-hash"

type Provider struct {
	Client               client.Client // Uncached client: allocation and operation journals require current state.
	Namespace, ClusterID string
	mu                   sync.Mutex
	Bootstrap            func(context.Context, api.EdgeCenterNodeClassSpec, bootstrap.Secrets, string, []byte) error
	Probe                func(context.Context, []string, []byte, string) error
	Cloud                func(string, int, int) *ec.Client
}

func New(c client.Client, namespace, cluster string) *Provider {
	return &Provider{Client: c, Namespace: namespace, ClusterID: cluster, Bootstrap: bootstrap.Apply, Probe: bootstrap.Probe, Cloud: ec.New}
}
func (*Provider) Name() string { return "edgecenter" }
func (*Provider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&api.EdgeCenterNodeClass{}}
}
func (*Provider) RepairPolicies() []cp.RepairPolicy { return nil }
func (*Provider) Reboot(context.Context, *v1.NodeClaim, string) error {
	return cp.NewNodeRebootNotImplementedError()
}
func hash(s api.EdgeCenterNodeClassSpec) string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p *Provider) class(ctx context.Context, name string) (*api.EdgeCenterNodeClass, error) {
	n := new(api.EdgeCenterNodeClass)
	err := p.Client.Get(ctx, types.NamespacedName{Name: name}, n)
	if err != nil {
		return nil, err
	}
	if n.Spec.ClusterID != p.ClusterID {
		return nil, fmt.Errorf("NodeClass belongs to another cluster")
	}
	if err = n.Spec.Validate(); err != nil {
		return nil, err
	}
	return n, nil
}
func instanceType(s api.EdgeCenterNodeClassSpec, f api.Flavor) *cp.InstanceType {
	labels := map[string]string{corev1.LabelInstanceTypeStable: f.Name, corev1.LabelTopologyRegion: s.Region, corev1.LabelTopologyZone: s.Region, corev1.LabelArchStable: "amd64", corev1.LabelOSStable: "linux", v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand}
	capacity := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(strconv.FormatInt(f.CPU, 10)), corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", f.MemoryGiB)), corev1.ResourcePods: resource.MustParse("110"), corev1.ResourceEphemeralStorage: resource.MustParse(fmt.Sprintf("%dGi", s.BootSizeGiB-4))}
	return &cp.InstanceType{Name: f.Name, Requirements: scheduling.NewLabelRequirements(labels), Capacity: capacity, Overhead: &cp.InstanceTypeOverhead{KubeReserved: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("1Gi")}, EvictionThreshold: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse(fmt.Sprintf("%dGi", s.BootSizeGiB/10+1))}}, Offerings: cp.Offerings{&cp.Offering{Requirements: scheduling.NewLabelRequirements(map[string]string{corev1.LabelTopologyZone: s.Region, v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand}), Price: f.Price, Available: true}}}
}
func (p *Provider) GetInstanceTypes(ctx context.Context, np *v1.NodePool) ([]*cp.InstanceType, error) {
	n, err := p.class(ctx, np.Spec.Template.Spec.NodeClassRef.Name)
	if err != nil {
		return nil, err
	}
	out := []*cp.InstanceType{}
	for _, f := range n.Spec.Flavors {
		out = append(out, instanceType(n.Spec, f))
	}
	return out, nil
}

type Launch struct {
	Claim, UID, Class, ClassUID, ClassHash, NodeName string
	Spec                                             api.EdgeCenterNodeClassSpec
	Flavor                                           api.Flavor
	Slot                                             int
	IP                                               string
	Labels                                           map[string]string
	Taints                                           []corev1.Taint
	StartupTaints                                    []corev1.Taint
	PortID, InstanceID                               string
	ProviderID                                       string
	VolumeIDs                                        []string
	// Intent is written before a cloud mutation. Task is written after its response.
	Intent, Task string
	Configured   bool
}

func (p *Provider) records(ctx context.Context) ([]corev1.ConfigMap, error) {
	var list corev1.ConfigMapList
	err := p.Client.List(ctx, &list, client.InNamespace(p.Namespace), client.MatchingLabels{managedLabel: p.ClusterID})
	return list.Items, err
}
func decode(cm *corev1.ConfigMap) (*Launch, error) {
	var r Launch
	err := json.Unmarshal([]byte(cm.Data["launch.json"]), &r)
	return &r, err
}
func (p *Provider) save(ctx context.Context, cm *corev1.ConfigMap, r *Launch) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	cm.Data = map[string]string{"launch.json": string(b)}
	return p.Client.Update(ctx, cm)
}
func (p *Provider) secret(ctx context.Context, name string) (map[string][]byte, error) {
	var s corev1.Secret
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: name}, &s)
	return s.Data, err
}
func (p *Provider) cloud(ctx context.Context, r *Launch) (*ec.Client, error) {
	s, err := p.secret(ctx, r.Spec.Credentials.Name)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(s["token"]))
	if token == "" {
		return nil, fmt.Errorf("credentials secret has no token")
	}
	return p.Cloud(token, r.Spec.ProjectID, r.Spec.RegionID), nil
}
func fits(it *cp.InstanceType, nc *v1.NodeClaim) bool {
	req := scheduling.NewNodeSelectorRequirementsWithMinValues(nc.Spec.Requirements...)
	if it.Requirements.Intersects(req) != nil {
		return false
	}
	alloc := it.Allocatable()
	for k, q := range nc.Spec.Resources.Requests {
		available := alloc[k]
		if available.Cmp(q) < 0 {
			return false
		}
	}
	return true
}
func (p *Provider) allocate(ctx context.Context, nc *v1.NodeClaim) (*corev1.ConfigMap, *Launch, error) {
	records, err := p.records(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range records {
		r, e := decode(&records[i])
		if e != nil {
			return nil, nil, e
		}
		if r.UID == string(nc.UID) {
			return &records[i], r, nil
		}
	}
	n, err := p.class(ctx, nc.Spec.NodeClassRef.Name)
	if err != nil {
		return nil, nil, err
	}
	flavors := append([]api.Flavor(nil), n.Spec.Flavors...)
	sort.Slice(flavors, func(i, j int) bool { return flavors[i].Price < flavors[j].Price })
	var selected *api.Flavor
	for i := range flavors {
		if fits(instanceType(n.Spec, flavors[i]), nc) {
			selected = &flavors[i]
			break
		}
	}
	if selected == nil {
		return nil, nil, cp.NewInsufficientCapacityError(fmt.Errorf("no configured flavor fits NodeClaim"))
	}
	for slot := n.Spec.Addresses.Start; slot <= n.Spec.Addresses.End; slot++ {
		ip, _ := n.Spec.IP(slot)
		key := fmt.Sprintf("%d/%d/%s/%s", n.Spec.ProjectID, n.Spec.RegionID, n.Spec.NetworkID, ip)
		sum := sha256.Sum256([]byte(key))
		name := "ec-slot-" + hex.EncodeToString(sum[:12])
		labels := map[string]string{corev1.LabelInstanceTypeStable: selected.Name, corev1.LabelTopologyRegion: n.Spec.Region, corev1.LabelTopologyZone: n.Spec.Region, corev1.LabelArchStable: "amd64", corev1.LabelOSStable: "linux", v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand}
		for k, v := range nc.Labels {
			labels[k] = v
		}
		r := &Launch{Claim: nc.Name, UID: string(nc.UID), Class: n.Name, ClassUID: string(n.UID), ClassHash: hash(n.Spec), NodeName: "ec-" + nc.Name, Spec: n.Spec, Flavor: *selected, Slot: slot, IP: ip, Labels: labels, Taints: append([]corev1.Taint{}, nc.Spec.Taints...), StartupTaints: append([]corev1.Taint{}, nc.Spec.StartupTaints...)}
		if n.Spec.BootstrapMode == "UserData" {
			r.ProviderID = fmt.Sprintf("edgecenter://%d/%d/%s", n.Spec.ProjectID, n.Spec.RegionID, nc.UID)
		}
		if len(r.NodeName) > 63 {
			return nil, nil, fmt.Errorf("NodeClaim name is too long")
		}
		b, _ := json.Marshal(r)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace, Labels: map[string]string{managedLabel: p.ClusterID}}, Data: map[string]string{"launch.json": string(b)}}
		if err = p.Client.Create(ctx, cm); apierrors.IsAlreadyExists(err) {
			continue
		} else if err != nil {
			return nil, nil, err
		}
		return cm, r, nil
	}
	return nil, nil, cp.NewInsufficientCapacityError(fmt.Errorf("no address slots remain"))
}
func hydrate(r *Launch) *v1.NodeClaim {
	it := instanceType(r.Spec, r.Flavor)
	id := r.ProviderID
	if id == "" {
		id = fmt.Sprintf("edgecenter://%d/%d/%s", r.Spec.ProjectID, r.Spec.RegionID, r.InstanceID)
	}
	return &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: r.Claim, UID: types.UID(r.UID), Labels: r.Labels, Annotations: map[string]string{hashAnnotation: r.ClassHash}}, Spec: v1.NodeClaimSpec{NodeClassRef: &v1.NodeClassReference{Group: api.Group, Kind: "EdgeCenterNodeClass", Name: r.Class}}, Status: v1.NodeClaimStatus{ProviderID: id, ImageID: r.Spec.ImageID, Capacity: it.Capacity, Allocatable: it.Allocatable()}}
}
func (p *Provider) find(ctx context.Context, id string) (*corev1.ConfigMap, *Launch, error) {
	records, err := p.records(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range records {
		r, e := decode(&records[i])
		if e != nil {
			return nil, nil, e
		}
		if r.InstanceID != "" && hydrate(r).Status.ProviderID == id {
			return &records[i], r, nil
		}
	}
	return nil, nil, cp.NewNodeClaimNotFoundError(fmt.Errorf("instance is not owned by this provider"))
}
func (p *Provider) Get(ctx context.Context, id string) (*v1.NodeClaim, error) {
	_, r, err := p.find(ctx, id)
	if err != nil {
		return nil, err
	}
	c, err := p.cloud(ctx, r)
	if err != nil {
		return nil, err
	}
	v, err := c.Instance(ctx, r.InstanceID)
	if ec.IsNotFound(err) {
		return nil, cp.NewNodeClaimNotFoundError(err)
	}
	if err != nil {
		return nil, err
	}
	if err = owned(v, r); err != nil {
		return nil, err
	}
	return hydrate(r), nil
}
func (p *Provider) List(ctx context.Context) ([]*v1.NodeClaim, error) {
	records, err := p.records(ctx)
	if err != nil {
		return nil, err
	}
	out := []*v1.NodeClaim{}
	for i := range records {
		r, e := decode(&records[i])
		if e != nil {
			return nil, e
		}
		if r.InstanceID == "" {
			continue
		}
		n, e := p.Get(ctx, hydrate(r).Status.ProviderID)
		if cp.IsNodeClaimNotFoundError(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		out = append(out, n)
	}
	return out, nil
}
func (p *Provider) IsDrifted(ctx context.Context, nc *v1.NodeClaim) (cp.DriftReason, error) {
	_, r, err := p.find(ctx, nc.Status.ProviderID)
	if err != nil {
		return "", err
	}
	n, err := p.class(ctx, r.Class)
	if err != nil {
		return "", err
	}
	if r.ClassUID != string(n.UID) || r.ClassHash != hash(n.Spec) {
		return cp.DriftReason("NodeClassChanged"), nil
	}
	return "", nil
}
func owned(v ec.Instance, r *Launch) error {
	if v.Metadata["managed_by"] != "karpenter-edgecenter" || v.Metadata["cluster_id"] != r.Spec.ClusterID || v.Metadata["nodeclaim_uid"] != r.UID {
		return fmt.Errorf("VM ownership does not match the NodeClaim")
	}
	return nil
}
