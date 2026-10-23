package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/apis/v1alpha1"
	"github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/bootstrap"
	ec "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/edgecenter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	cp "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const gpuKernelModules = `apiVersion: v1alpha1
kind: KernelModuleConfig
name: nvidia
---
apiVersion: v1alpha1
kind: KernelModuleConfig
name: nvidia_uvm
`

func gpuSpec() api.EdgeCenterNodeClassSpec {
	return api.EdgeCenterNodeClassSpec{ClusterID: "test", ProjectID: 1, RegionID: 10, Region: "moscow", NetworkID: "network", ImageID: "image", SecurityGroupIDs: []string{"sg"}, Flavors: []api.Flavor{{Name: "cpu", CPU: 4, MemoryGiB: 16, Price: 1}, {Name: "gpu", CPU: 8, MemoryGiB: 32, GPU: 2, Price: 10}}, BootSizeGiB: 64, BootType: "standard", Addresses: api.AddressPool{CIDR: "10.64.0.0/18", HostOffset: 266, Start: 28, End: 29, KubeSpanPortBase: 52110}, Gateways: []api.Gateway{{PublicIP: "192.0.2.1"}}, BootstrapMode: "UserData", TalosEndpoints: []string{"192.0.2.10:50000"}, Credentials: api.SecretRef{Name: "cloud"}, Talos: api.SecretRef{Name: "talos"}}
}

func TestGPUFlavorAdvertisesDevices(t *testing.T) {
	spec := gpuSpec()
	nc := &v1.NodeClaim{}
	nc.Spec.Resources.Requests = corev1.ResourceList{gpuResource: resource.MustParse("1")}

	gpu := instanceType(spec, spec.Flavors[1])
	if got := gpu.Capacity[gpuResource]; got.Value() != 2 {
		t.Fatalf("GPU flavor advertises %s GPUs, want 2", got.String())
	}
	if !fits(gpu, nc) {
		t.Fatal("GPU flavor rejected a GPU request")
	}

	cpu := instanceType(spec, spec.Flavors[0])
	if _, ok := cpu.Capacity[gpuResource]; ok {
		t.Fatal("CPU flavor advertises GPUs")
	}
	if fits(cpu, nc) {
		t.Fatal("CPU flavor accepted a GPU request")
	}
}

func TestNewFieldsKeepExistingNodeClassHash(t *testing.T) {
	spec := gpuSpec()
	spec.Flavors = spec.Flavors[:1]
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"gpu"`) || strings.Contains(string(b), `"talosPatches"`) {
		t.Fatal("unset GPU fields change the hash of existing NodeClasses and would drift their nodes")
	}

	patched := spec
	patched.TalosPatches = []string{gpuKernelModules}
	if hash(patched) == hash(spec) {
		t.Fatal("Talos patches are missing from drift detection")
	}
}

func TestTalosPatchesAreRendered(t *testing.T) {
	spec := gpuSpec()
	spec.TalosPatches = []string{gpuKernelModules, "machine:\n  install:\n    image: factory.talos.dev/installer/gpu:v1.14.2\n"}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}

	config, err := bootstrap.Render([]byte("version: v1alpha1\nmachine:\n  type: worker\n  install:\n    image: ghcr.io/siderolabs/installer:v1.14.2\n"), spec, "gpu-node", "10.64.1.38", "edgecenter://1/10/id", 28, map[string]string{"type": "gpu"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := string(config)
	for _, want := range []string{"name: nvidia\n", "name: nvidia_uvm\n", "image: factory.talos.dev/installer/gpu:v1.14.2", "hostname: gpu-node", "type: gpu"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered config lacks %q", want)
		}
	}
	if strings.Contains(out, "ghcr.io/siderolabs/installer") {
		t.Fatal("the NodeClass patch did not replace the installer image")
	}
}

func TestValidateRejectsInvalidGPUSettings(t *testing.T) {
	negative := gpuSpec()
	negative.Flavors[1].GPU = -1
	if negative.Validate() == nil {
		t.Fatal("negative GPU count accepted")
	}

	broken := gpuSpec()
	broken.TalosPatches = []string{"machine: [unclosed"}
	if broken.Validate() == nil {
		t.Fatal("invalid Talos patch accepted")
	}
}

func TestFailedLaunchReportsInsufficientCapacity(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	scheme.AddKnownTypes(schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}, &v1.NodeClaim{}, &v1.NodeClaimList{})
	_ = api.AddToScheme(scheme)

	klass := &api.EdgeCenterNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "gpu", UID: "class-gpu"}, Spec: gpuSpec()}
	cloud := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Namespace: "test"}, Data: map[string][]byte{"token": []byte("test")}}
	talos := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "talos", Namespace: "test"}, Data: map[string][]byte{"worker.yaml": []byte("version: v1alpha1\nmachine:\n  type: worker\n"), "talosconfig": []byte("unused")}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(klass, cloud, talos).Build()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/reserved_fixed_ips/1/10":
			reply(map[string]any{"tasks": []string{"port-task"}})
		case r.URL.Path == "/v1/reserved_fixed_ips/1/10":
			reply(map[string]any{"count": 0, "results": []any{}})
		case r.URL.Path == "/v1/tasks/port-task":
			reply(map[string]any{"state": "FINISHED", "created_resources": map[string]any{"ports": []string{"port"}}})
		case r.Method == "POST" && r.URL.Path == "/v2/instances/1/10":
			reply(map[string]any{"tasks": []string{"vm-task"}})
		case r.URL.Path == "/v1/tasks/vm-task":
			reply(map[string]any{"state": "ERROR", "error": "No valid host was found"})
		default:
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()

	p := New(kube, "test", "test")
	p.Cloud = func(token string, project, region int) *ec.Client {
		c := ec.New(token, project, region)
		c.URL = server.URL
		return c
	}
	nc := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "gpu", UID: types.UID("claim-gpu")}, Spec: v1.NodeClaimSpec{NodeClassRef: &v1.NodeClassReference{Group: api.Group, Kind: "EdgeCenterNodeClass", Name: "gpu"}}}
	nc.Spec.Resources.Requests = corev1.ResourceList{gpuResource: resource.MustParse("1")}

	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if _, err = p.Create(ctx, nc); cp.IsInsufficientCapacityError(err) {
			break
		}
	}
	if !cp.IsInsufficientCapacityError(err) {
		t.Fatalf("a failed cloud launch must free the NodeClaim for another offering, got: %v", err)
	}
	if _, err = p.Create(ctx, nc); !cp.IsInsufficientCapacityError(err) {
		t.Fatalf("a recorded launch failure must stay an insufficient capacity error, got: %v", err)
	}
}
