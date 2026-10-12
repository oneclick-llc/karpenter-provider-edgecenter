// Package bootstrap configures Talos through a regional SSH gateway.
package bootstrap

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	api "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/apis/v1alpha1"
	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talos "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

type Secrets struct{ Worker, Config, SSHKey, KnownHosts []byte }

func Render(base []byte, s api.EdgeCenterNodeClassSpec, name, ip, providerID string, slot int, labels map[string]string, taints []corev1.Taint, startup []corev1.Taint) ([]byte, error) {
	endpoints := []string{}
	for _, g := range s.Gateways {
		endpoints = append(endpoints, fmt.Sprintf("%s:%d", g.PublicIP, s.Addresses.KubeSpanPortBase+slot))
	}
	register := []string{"karpenter.sh/unregistered:NoExecute"}
	for _, t := range append(append([]corev1.Taint{}, taints...), startup...) {
		register = append(register, t.Key+"="+t.Value+":"+string(t.Effect))
	}
	tm := map[string]string{}
	for _, t := range taints {
		tm[t.Key] = t.Value + ":" + string(t.Effect)
	}
	docs := []map[string]any{
		{"apiVersion": "v1alpha1", "kind": "HostnameConfig", "auto": "off", "hostname": name},
		{"apiVersion": "v1alpha1", "kind": "KubeNodeConfig", "nodeIP": map[string]any{"validSubnets": []string{ip + "/32"}}, "labels": labels, "taints": tm},
		{"apiVersion": "v1alpha1", "kind": "KubeletConfig", "extraArgs": map[string]string{"provider-id": providerID, "register-with-taints": strings.Join(register, ",")}},
		{"apiVersion": "v1alpha1", "kind": "KubeSpanConfig", "filters": map[string]any{"endpoints": []string{ip + "/32"}}},
		{"apiVersion": "v1alpha1", "kind": "KubeSpanEndpointsConfig", "extraAnnouncedEndpoints": endpoints},
	}
	var patch []string
	for _, d := range docs {
		b, err := yaml.Marshal(d)
		if err != nil {
			return nil, err
		}
		patch = append(patch, string(b))
	}
	p, err := configpatcher.LoadPatch([]byte(strings.Join(patch, "\n---\n")))
	if err != nil {
		return nil, err
	}
	out, err := configpatcher.Apply(configpatcher.WithBytes(base), []configpatcher.Patch{p})
	if err != nil {
		return nil, err
	}
	return out.Bytes()
}

func Apply(ctx context.Context, s api.EdgeCenterNodeClassSpec, secrets Secrets, ip string, configuration []byte) error {
	signer, err := ssh.ParsePrivateKey(secrets.SSHKey)
	if err != nil {
		return fmt.Errorf("parse gateway key: %w", err)
	}
	kh, err := os.CreateTemp("", "ec-known-hosts-")
	if err != nil {
		return err
	}
	defer os.Remove(kh.Name())
	if _, err = kh.Write(secrets.KnownHosts); err != nil {
		kh.Close()
		return err
	}
	kh.Close()
	verify, err := knownhosts.New(kh.Name())
	if err != nil {
		return err
	}
	var last error
	for _, g := range s.Gateways {
		addr := g.Address
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "22")
		}
		conn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", addr)
		if err != nil {
			last = err
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		cc, ch, rq, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{User: g.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: verify, HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}})
		if err != nil {
			conn.Close()
			last = err
			continue
		}
		_ = conn.SetDeadline(time.Time{})
		sc := ssh.NewClient(cc, ch, rq)
		last = applyThrough(ctx, sc, secrets, ip, configuration)
		sc.Close()
		if last == nil {
			return nil
		}
	}
	return fmt.Errorf("Talos configuration through gateways failed: %w", last)
}
func applyThrough(ctx context.Context, sc *ssh.Client, secrets Secrets, ip string, configuration []byte) error {
	endpoint := net.JoinHostPort(ip, "50000")
	dial := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return sc.DialContext(ctx, "tcp", endpoint) })
	cfg, err := talosconfig.FromBytes(secrets.Config)
	if err != nil {
		return fmt.Errorf("parse Talos client configuration: %w", err)
	}
	// A successful authenticated read also recovers an interrupted ApplyConfiguration.
	authenticated, err := talos.New(ctx, talos.WithConfig(cfg), talos.WithEndpoints(endpoint), talos.WithGRPCDialOptions(dial))
	if err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, authErr := authenticated.Version(probe)
	cancel()
	authenticated.Close()
	if authErr == nil {
		return nil
	}
	maintenance, err := talos.New(ctx, talos.WithMaintenanceMode(endpoint, nil), talos.WithGRPCDialOptions(dial))
	if err != nil {
		return err
	}
	defer maintenance.Close()
	applyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = maintenance.ApplyConfiguration(applyCtx, &machine.ApplyConfigurationRequest{Data: configuration, Mode: machine.ApplyConfigurationRequest_AUTO})
	return err
}
