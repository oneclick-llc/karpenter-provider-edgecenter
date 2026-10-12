package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/apis/v1alpha1"
	"github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/bootstrap"
	ec "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/edgecenter"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	cp "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/yaml"
)

func TestFitsRejectsTalosMemoryOvercommit(t *testing.T) {
	it := instanceType(api.EdgeCenterNodeClassSpec{Region: "test", BootSizeGiB: 64}, api.Flavor{Name: "4-16", CPU: 4, MemoryGiB: 16, Price: 1})
	nc := &v1.NodeClaim{}
	nc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("15600Mi")}
	if fits(it, nc) {
		t.Fatal("flavor accepted memory that the Talos node cannot provide")
	}
}

func TestLifecycleSmoke(t *testing.T) {
	for _, mode := range []string{"SSHGateway", "UserData", "BareMetal", "BareMetalStorage"} {
		t.Run(mode, func(t *testing.T) { lifecycleSmoke(t, mode) })
	}
}

func lifecycleSmoke(t *testing.T, mode string) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	scheme.AddKnownTypes(schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}, &v1.NodeClaim{}, &v1.NodeClaimList{}, &v1.NodePool{}, &v1.NodePoolList{})
	_ = api.AddToScheme(scheme)
	spec := api.EdgeCenterNodeClassSpec{ClusterID: "test", ProjectID: 1, RegionID: 10, Region: "moscow", NetworkID: "network", ImageID: "image", SecurityGroupIDs: []string{"sg"}, Flavors: []api.Flavor{{Name: "small", CPU: 4, MemoryGiB: 16, Price: 1}}, BootSizeGiB: 64, BootType: "standard", Addresses: api.AddressPool{CIDR: "10.64.0.0/18", HostOffset: 266, Start: 16, End: 17, KubeSpanPortBase: 52110}, Gateways: []api.Gateway{{Address: "gateway", User: "ops", PublicIP: "192.0.2.1"}}, Credentials: api.SecretRef{Name: "cloud"}, Talos: api.SecretRef{Name: "talos"}, SSH: api.SecretRef{Name: "ssh"}}
	storage := mode == "BareMetalStorage"
	bare := mode == "BareMetal" || storage
	if storage {
		spec.BootSizeGiB = 447
		spec.BareMetalStorage = &api.BareMetalStorage{EphemeralSizeGiB: 64, LocalVolumeName: "database"}
	}
	if bare {
		mode = "UserData"
		spec.ComputeType = "BareMetal"
		spec.BootType = ""
		spec.SecurityGroupIDs = nil
	}
	spec.BootstrapMode = mode
	if mode == "UserData" {
		spec.TalosEndpoints = []string{"192.0.2.10:50000"}
		spec.SSH = api.SecretRef{}
	}
	klass := &api.EdgeCenterNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "test", UID: "class-1"}, Spec: spec}
	secrets := []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "cloud", Namespace: "test"}, Data: map[string][]byte{"token": []byte("test")}}, {ObjectMeta: metav1.ObjectMeta{Name: "talos", Namespace: "test"}, Data: map[string][]byte{"worker.yaml": []byte("version: v1alpha1\nmachine:\n  type: worker\n"), "talosconfig": []byte("unused")}}, {ObjectMeta: metav1.ObjectMeta{Name: "ssh", Namespace: "test"}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(klass, secrets[0], secrets[1], secrets[2]).Build()
	port, vm, volume := false, false, false
	posts := 0
	foreign := false
	metadata := map[string]string{"managed_by": "karpenter-edgecenter", "cluster_id": "test", "nodeclaim_uid": "claim-1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/reserved_fixed_ips/1/10":
			port = true
			posts++
			reply(map[string]any{"tasks": []string{"port-task"}})
		case r.Method == "POST" && (r.URL.Path == "/v2/instances/1/10" || r.URL.Path == "/v1/bminstances/1/10"):
			if bare && r.URL.Path != "/v1/bminstances/1/10" {
				t.Error("bare metal used the VM create endpoint")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if mode == "UserData" {
				encoded, _ := body["user_data"].(string)
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || !strings.Contains(string(decoded), "edgecenter://1/10/claim-1") || !strings.Contains(string(decoded), "karpenter.sh/unregistered") {
					t.Error("user data lacks the stable provider ID or registration taint")
				}
				volumes := 0
				for _, document := range strings.Split(string(decoded), "---\n") {
					var doc map[string]any
					if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
						t.Fatal(err)
					}
					if doc["kind"] == "VolumeConfig" && doc["name"] == "EPHEMERAL" {
						provisioning := doc["provisioning"].(map[string]any)
						if provisioning["maxSize"] != "64GiB" || provisioning["minSize"] != "64GiB" || provisioning["grow"] != false {
							t.Fatal("incorrect EPHEMERAL partition")
						}
						volumes++
					}
					if doc["kind"] == "UserVolumeConfig" {
						provisioning := doc["provisioning"].(map[string]any)
						if doc["name"] != "database" || provisioning["grow"] != true {
							t.Fatal("incorrect local data partition")
						}
						volumes++
					}
				}
				if (storage && volumes != 2) || (!storage && volumes != 0) {
					t.Fatal("unexpected storage documents in user data")
				}
			} else if _, ok := body["user_data"]; ok {
				t.Error("SSHGateway unexpectedly sent user data")
			}
			if bare {
				if _, ok := body["volumes"]; ok {
					t.Error("bare metal allocated a Cinder volume")
				}
				if _, ok := body["security_groups"]; ok {
					t.Error("bare metal received VM security groups")
				}
				if body["image_id"] != "image" {
					t.Error("bare metal image is missing")
				}
			}
			vm = true
			volume = !bare
			posts++
			reply(map[string]any{"tasks": []string{"vm-task"}})
		case r.URL.Path == "/v1/tasks/port-task":
			reply(map[string]any{"state": "FINISHED", "created_resources": map[string]any{"ports": []string{"port"}}})
		case r.URL.Path == "/v1/tasks/delete":
			reply(map[string]any{"state": "FINISHED"})
		case r.URL.Path == "/v2/bmcapacity/10":
			reply(map[string]any{"capacity": map[string]int{"small": 1}})
		case r.URL.Path == "/v1/tasks/vm-task":
			disks := []string{}
			if !bare {
				disks = append(disks, "disk")
			}
			reply(map[string]any{"state": "FINISHED", "created_resources": map[string]any{"instances": []string{"vm"}, "volumes": disks}})
		case r.URL.Path == "/v1/instances/1/10/vm":
			if !vm {
				w.WriteHeader(404)
				return
			}
			if r.Method == "DELETE" {
				vm = false
				reply(map[string]any{"tasks": []string{"delete"}})
				return
			}
			m := metadata
			if foreign {
				m = map[string]string{"managed_by": "terraform"}
			}
			reply(map[string]any{"instance_id": "vm", "status": "ACTIVE", "metadata": m})
		case r.URL.Path == "/v1/volumes/1/10/disk":
			if !volume {
				w.WriteHeader(404)
				return
			}
			if r.Method == "DELETE" {
				volume = false
				reply(map[string]any{})
				return
			}
			reply(map[string]any{"id": "disk", "metadata": metadata, "status": "available"})
		case r.URL.Path == "/v1/reserved_fixed_ips/1/10/port":
			if !port {
				w.WriteHeader(404)
				return
			}
			if r.Method == "DELETE" {
				port = false
				reply(map[string]any{})
				return
			}
			ip, _ := spec.IP(16)
			reply(map[string]any{"port_id": "port", "network_id": "network", "fixed_ip_address": ip})
		case r.URL.Path == "/v1/reserved_fixed_ips/1/10":
			reply(map[string]any{"count": 0, "results": []any{}})
		case r.URL.Path == "/v1/securitygroups/1/10/sg":
			reply(map[string]any{"name": "group"})
		case r.Method == "POST" && (strings.HasSuffix(r.URL.Path, "enable_port_security") || strings.HasSuffix(r.URL.Path, "addsecuritygroup")):
			reply(map[string]any{})
		default:
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	makeProvider := func() *Provider {
		p := New(kube, "test", "test")
		p.Cloud = func(token string, project, region int) *ec.Client {
			c := ec.New(token, project, region)
			c.URL = server.URL
			return c
		}
		p.Probe = func(context.Context, []string, []byte, string) error { return nil }
		p.Bootstrap = func(context.Context, api.EdgeCenterNodeClassSpec, bootstrap.Secrets, string, []byte) error {
			return nil
		}
		return p
	}
	p := makeProvider()
	nc := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "smoke", UID: types.UID("claim-1")}, Spec: v1.NodeClaimSpec{NodeClassRef: &v1.NodeClassReference{Group: api.Group, Kind: "EdgeCenterNodeClass", Name: "test"}}}
	if _, err := p.Create(ctx, nc); err == nil {
		t.Fatal("expected pending port")
	}
	p = makeProvider() // Simulate a restart after the cloud task was recorded.
	if _, err := p.Create(ctx, nc); err == nil {
		t.Fatal("expected pending VM")
	}
	p = makeProvider()
	created, err := p.Create(ctx, nc)
	if err != nil {
		t.Fatal(err)
	}
	expectedID := "edgecenter://1/10/vm"
	if mode == "UserData" {
		expectedID = "edgecenter://1/10/claim-1"
	}
	if created.Status.ProviderID != expectedID {
		t.Fatal(created.Status.ProviderID)
	}
	if _, err = p.Create(ctx, nc); err != nil {
		t.Fatal(err)
	}
	if posts != 2 {
		t.Fatalf("duplicate create requests: %d", posts)
	}
	foreign = true
	if err = p.Delete(ctx, created); err == nil {
		t.Fatal("foreign VM deletion was allowed")
	}
	if !vm {
		t.Fatal("foreign VM was deleted")
	}
	foreign = false
	for i := 0; i < 5; i++ {
		err = p.Delete(ctx, created)
		if cp.IsNodeClaimNotFoundError(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !cp.IsNodeClaimNotFoundError(err) || vm || port || volume {
		t.Fatal(fmt.Sprintf("cleanup: %v vm=%v port=%v volume=%v", err, vm, port, volume))
	}
	records, err := p.records(ctx)
	if err != nil || len(records) != 0 {
		t.Fatal("slot was not released")
	}
}

func TestBareMetalStorageConfiguration(t *testing.T) {
	base := api.EdgeCenterNodeClassSpec{ComputeType: "BareMetal", BootstrapMode: "UserData", TalosEndpoints: []string{"192.0.2.1:50000"}, ClusterID: "test", ProjectID: 1, RegionID: 10, Region: "test", NetworkID: "network", ImageID: "image", BootSizeGiB: 447, Flavors: []api.Flavor{{Name: "bare", CPU: 16, MemoryGiB: 64, Price: 1}}, Addresses: api.AddressPool{CIDR: "10.64.0.0/18", HostOffset: 266, Start: 18, End: 18, KubeSpanPortBase: 52110}, Gateways: []api.Gateway{{PublicIP: "192.0.2.1"}}, Credentials: api.SecretRef{Name: "cloud"}, Talos: api.SecretRef{Name: "talos"}}
	for _, size := range []int{64, 128} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			spec := base
			spec.BareMetalStorage = &api.BareMetalStorage{EphemeralSizeGiB: size, LocalVolumeName: "local-data"}
			if err := spec.Validate(); err != nil {
				t.Fatal(err)
			}
			config, err := bootstrap.Render([]byte("version: v1alpha1\nmachine:\n  type: worker\n---\napiVersion: v1alpha1\nkind: VolumeConfig\nname: EPHEMERAL\nmount:\n  secure: true\n"), spec, "worker", "10.64.1.28", "edgecenter://1/10/id", 18, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(config), fmt.Sprintf("maxSize: %dGiB", size)) || !strings.Contains(string(config), "secure: true") {
				t.Fatal("storage size or existing mount options lost")
			}
			it := instanceType(spec, spec.Flavors[0])
			expected := resource.MustParse(fmt.Sprintf("%dGi", size-1))
			if it.Capacity.StorageEphemeral().Cmp(expected) != 0 {
				t.Fatal("local data capacity was advertised as ephemeral storage")
			}
			nc := &v1.NodeClaim{}
			nc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("200Gi")}
			if fits(it, nc) {
				t.Fatal("accepted more ephemeral storage than the partition provides")
			}
			if hash(spec) == hash(base) {
				t.Fatal("storage layout is missing from drift detection")
			}
		})
	}
	for _, tc := range []struct {
		name, compute, volume string
		size                  int
	}{
		{"vm", "VirtualMachine", "local-data", 64},
		{"too-small", "BareMetal", "local-data", 0},
		{"no-data-space", "BareMetal", "local-data", 443},
		{"bad-path", "BareMetal", "../data", 64},
		{"no-name", "BareMetal", "", 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			spec.ComputeType = tc.compute
			spec.BareMetalStorage = &api.BareMetalStorage{EphemeralSizeGiB: tc.size, LocalVolumeName: tc.volume}
			if err := spec.Validate(); err == nil {
				t.Fatal("invalid storage configuration accepted")
			}
		})
	}
	t.Run("data-disk", func(t *testing.T) {
		spec := base
		spec.BareMetalStorage = &api.BareMetalStorage{EphemeralSizeGiB: 64, LocalVolumeName: "local-data", DataDiskVolumeName: "local-data2"}
		if err := spec.Validate(); err != nil {
			t.Fatal(err)
		}
		config, err := bootstrap.Render([]byte("version: v1alpha1\nmachine:\n  type: worker\n"), spec, "worker", "10.64.1.28", "edgecenter://1/10/id", 18, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := string(config)
		if !strings.Contains(out, "name: local-data2") || !strings.Contains(out, "'"+bootstrap.DataDiskSelector+"'") {
			t.Fatalf("data disk volume missing or not limited to non-system disks:\n%s", out)
		}
		// Talos compiles the selector in this environment on the node; a typo would leave the disk unused.
		if _, err := cel.ParseBooleanExpression(bootstrap.DataDiskSelector, celenv.DiskLocator()); err != nil {
			t.Fatal(err)
		}
		if strings.Count(out, "kind: UserVolumeConfig") != 2 {
			t.Fatal("expected one user volume on each disk")
		}
		withoutDataDisk := spec
		withoutDataDisk.BareMetalStorage = &api.BareMetalStorage{EphemeralSizeGiB: 64, LocalVolumeName: "local-data"}
		if hash(spec) == hash(withoutDataDisk) {
			t.Fatal("data disk volume is missing from drift detection")
		}
	})
	for _, tc := range []struct{ name, volume string }{
		{"data-disk-same-name", "local-data"},
		{"data-disk-bad-path", "../data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			spec.BareMetalStorage = &api.BareMetalStorage{EphemeralSizeGiB: 64, LocalVolumeName: "local-data", DataDiskVolumeName: tc.volume}
			if err := spec.Validate(); err == nil {
				t.Fatal("invalid data disk configuration accepted")
			}
		})
	}
}
