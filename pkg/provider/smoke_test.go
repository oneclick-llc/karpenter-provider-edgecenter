package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/apis/v1alpha1"
	"github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/bootstrap"
	ec "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/edgecenter"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	cp "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func TestLifecycleSmoke(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	scheme.AddKnownTypes(schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}, &v1.NodeClaim{}, &v1.NodeClaimList{}, &v1.NodePool{}, &v1.NodePoolList{})
	_ = api.AddToScheme(scheme)
	spec := api.EdgeCenterNodeClassSpec{ClusterID: "test", ProjectID: 1, RegionID: 10, Region: "moscow", NetworkID: "network", ImageID: "image", SecurityGroupIDs: []string{"sg"}, Flavors: []api.Flavor{{Name: "small", CPU: 4, MemoryGiB: 16, Price: 1}}, BootSizeGiB: 64, BootType: "standard", Addresses: api.AddressPool{CIDR: "10.64.0.0/18", HostOffset: 266, Start: 16, End: 17, KubeSpanPortBase: 52110}, Gateways: []api.Gateway{{Address: "gateway", User: "ops", PublicIP: "192.0.2.1"}}, Credentials: api.SecretRef{Name: "cloud"}, Talos: api.SecretRef{Name: "talos"}, SSH: api.SecretRef{Name: "ssh"}}
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
		case r.Method == "POST" && r.URL.Path == "/v2/instances/1/10":
			vm = true
			volume = true
			posts++
			reply(map[string]any{"tasks": []string{"vm-task"}})
		case r.URL.Path == "/v1/tasks/port-task":
			reply(map[string]any{"state": "FINISHED", "created_resources": map[string]any{"ports": []string{"port"}}})
		case r.URL.Path == "/v1/tasks/vm-task":
			reply(map[string]any{"state": "FINISHED", "created_resources": map[string]any{"instances": []string{"vm"}, "volumes": []string{"disk"}}})
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
	if created.Status.ProviderID != "edgecenter://1/10/vm" {
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
