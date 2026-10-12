package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/bootstrap"
	ec "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/edgecenter"
	corev1 "k8s.io/api/core/v1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	cp "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func ids(t ec.Task, key string) []string {
	var out []string
	_ = json.Unmarshal(t.Created[key], &out)
	return out
}
func (p *Provider) finishTask(ctx context.Context, c *ec.Client, cm *corev1.ConfigMap, r *Launch) error {
	if r.Task == "" {
		return nil
	}
	t, err := c.Task(ctx, r.Task)
	if err != nil {
		return err
	}
	if t.State == "ERROR" {
		return fmt.Errorf("cloud task %s failed: %s", r.Task, t.Error)
	}
	if t.State != "FINISHED" {
		return fmt.Errorf("cloud task %s is %s", r.Task, t.State)
	}
	switch r.Intent {
	case "create-port":
		v := ids(t, "ports")
		if len(v) != 1 {
			return fmt.Errorf("port task returned %d ports", len(v))
		}
		r.PortID = v[0]
	case "create-instance":
		v := ids(t, "instances")
		if len(v) != 1 {
			return fmt.Errorf("VM task returned %d instances", len(v))
		}
		r.InstanceID = v[0]
		r.VolumeIDs = ids(t, "volumes")
	}
	r.Intent = ""
	r.Task = ""
	return p.save(ctx, cm, r)
}

func (p *Provider) Create(ctx context.Context, nc *v1.NodeClaim) (*v1.NodeClaim, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cm, r, err := p.allocate(ctx, nc)
	if err != nil {
		return nil, err
	}
	c, err := p.cloud(ctx, r)
	if err != nil {
		return nil, err
	}
	if err = p.finishTask(ctx, c, cm, r); err != nil {
		return nil, err
	}
	if r.PortID == "" {
		if r.Intent != "" {
			return nil, fmt.Errorf("port submission result is unknown; preserve slot %d and inspect the journal", r.Slot)
		}
		ports, err := ec.List[ec.Port](ctx, c, "reserved_fixed_ips")
		if err != nil {
			return nil, err
		}
		for _, port := range ports {
			if port.IP == r.IP && port.NetworkID == r.Spec.NetworkID {
				return nil, fmt.Errorf("address %s already exists outside this launch", r.IP)
			}
		}
		r.Intent = "create-port"
		if err = p.save(ctx, cm, r); err != nil {
			return nil, err
		}
		r.Task, err = c.Submit(ctx, c.Path("v1", "reserved_fixed_ips"), map[string]any{"type": "ip_address", "network_id": r.Spec.NetworkID, "ip_address": r.IP, "is_vip": false})
		if err != nil {
			return nil, err
		}
		if err = p.save(ctx, cm, r); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("reserved IP creation is in progress")
	}
	if r.InstanceID == "" {
		// Metadata allows recovery when the VM request succeeded but its response was lost.
		if r.Intent != "" {
			list, err := ec.List[ec.Instance](ctx, c, "instances")
			if err != nil {
				return nil, err
			}
			for _, v := range list {
				if v.Metadata["nodeclaim_uid"] == r.UID && owned(v, r) == nil {
					r.InstanceID = v.ID
					r.Intent = ""
					if err = p.save(ctx, cm, r); err != nil {
						return nil, err
					}
					break
				}
			}
			if r.InstanceID == "" {
				return nil, fmt.Errorf("VM submission result is unknown; no duplicate request was sent")
			}
		} else {
			meta := map[string]string{"managed_by": "karpenter-edgecenter", "cluster_id": r.Spec.ClusterID, "nodeclaim_uid": r.UID, "nodeclaim": r.Claim, "nodeclass": r.Class}
			groups := []map[string]string{}
			for _, id := range r.Spec.SecurityGroupIDs {
				groups = append(groups, map[string]string{"id": id})
			}
			body := map[string]any{"names": []string{r.NodeName}, "flavor": r.Flavor.Name, "metadata": meta, "availability_zone": "nova", "interfaces": []map[string]any{{"type": "reserved_fixed_ip", "port_id": r.PortID}}, "security_groups": groups, "volumes": []map[string]any{{"source": "image", "image_id": r.Spec.ImageID, "size": r.Spec.BootSizeGiB, "type_name": r.Spec.BootType, "boot_index": 0, "name": r.NodeName + "-boot", "metadata": meta}}}
			if r.Spec.ServerGroupID != "" {
				body["servergroup_id"] = r.Spec.ServerGroupID
			}
			r.Intent = "create-instance"
			if err = p.save(ctx, cm, r); err != nil {
				return nil, err
			}
			r.Task, err = c.Submit(ctx, c.Path("v2", "instances"), body)
			if err != nil {
				return nil, err
			}
			if err = p.save(ctx, cm, r); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("VM creation is in progress")
		}
	}
	vm, err := c.Instance(ctx, r.InstanceID)
	if err != nil {
		return nil, err
	}
	if err = owned(vm, r); err != nil {
		return nil, err
	}
	if vm.Status != "ACTIVE" {
		return nil, fmt.Errorf("VM is %s", vm.Status)
	}
	// Only volumes marked for this claim can enter the cleanup journal.
	if len(r.VolumeIDs) == 0 {
		volumes, err := ec.List[ec.Volume](ctx, c, "volumes")
		if err != nil {
			return nil, err
		}
		for _, v := range volumes {
			if v.Metadata["nodeclaim_uid"] == r.UID && v.Metadata["managed_by"] == "karpenter-edgecenter" {
				r.VolumeIDs = append(r.VolumeIDs, v.ID)
			}
		}
		if len(r.VolumeIDs) == 0 {
			return nil, fmt.Errorf("owned boot volume was not found")
		}
		if err = p.save(ctx, cm, r); err != nil {
			return nil, err
		}
	}
	if !r.Configured {
		// Reserved ports can retain their initial groups. Assign groups to the exact owned port.
		if err = c.Do(ctx, "POST", fmt.Sprintf("/v1/ports/%d/%d/%s/enable_port_security", r.Spec.ProjectID, r.Spec.RegionID, r.PortID), nil, nil); err != nil {
			return nil, err
		}
		names := []string{}
		for _, id := range r.Spec.SecurityGroupIDs {
			var group struct {
				Name string `json:"name"`
			}
			if err = c.Do(ctx, "GET", c.Path("v1", "securitygroups")+"/"+id, nil, &group); err != nil {
				return nil, err
			}
			names = append(names, group.Name)
		}
		if err = c.Do(ctx, "POST", c.Path("v1", "instances")+"/"+r.InstanceID+"/addsecuritygroup", map[string]any{"ports_security_group_names": []map[string]any{{"port_id": r.PortID, "security_group_names": names}}}, nil); err != nil {
			return nil, err
		}
		ts, err := p.secret(ctx, r.Spec.Talos.Name)
		if err != nil {
			return nil, err
		}
		ss, err := p.secret(ctx, r.Spec.SSH.Name)
		if err != nil {
			return nil, err
		}
		secrets := bootstrap.Secrets{Worker: ts["worker.yaml"], Config: ts["talosconfig"], SSHKey: ss["ssh-privatekey"], KnownHosts: ss["known_hosts"]}
		config, err := bootstrap.Render(secrets.Worker, r.Spec, r.NodeName, r.IP, hydrate(r).Status.ProviderID, r.Slot, r.Labels, r.Taints, r.StartupTaints)
		if err != nil {
			return nil, err
		}
		if err = p.Bootstrap(ctx, r.Spec, secrets, r.IP, config); err != nil {
			return nil, err
		}
		r.Configured = true
		if err = p.save(ctx, cm, r); err != nil {
			return nil, err
		}
	}
	return hydrate(r), nil
}

func (p *Provider) Delete(ctx context.Context, nc *v1.NodeClaim) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cm, r, err := p.find(ctx, nc.Status.ProviderID)
	if err != nil {
		return err
	}
	return p.delete(ctx, cm, r)
}
func (p *Provider) delete(ctx context.Context, cm *corev1.ConfigMap, r *Launch) error {
	c, err := p.cloud(ctx, r)
	if err != nil {
		return err
	}
	if r.Task != "" {
		if err = p.finishTask(ctx, c, cm, r); err != nil {
			return err
		}
	}
	if r.InstanceID != "" {
		vm, err := c.Instance(ctx, r.InstanceID)
		if err == nil {
			if err = owned(vm, r); err != nil {
				return err
			}
			if vm.Status == "DELETING" {
				return nil
			}
			if err = c.Do(ctx, "DELETE", c.Path("v1", "instances")+"/"+r.InstanceID, nil, nil); err != nil && !ec.IsNotFound(err) {
				return err
			}
			return nil
		}
		if !ec.IsNotFound(err) {
			return err
		}
	}
	for _, id := range r.VolumeIDs {
		var v ec.Volume
		err = c.Do(ctx, "GET", c.Path("v1", "volumes")+"/"+id, nil, &v)
		if ec.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if v.Metadata["nodeclaim_uid"] != r.UID || v.Metadata["managed_by"] != "karpenter-edgecenter" {
			return fmt.Errorf("volume ownership mismatch")
		}
		if v.Status != "deleting" {
			if err = c.Do(ctx, "DELETE", c.Path("v1", "volumes")+"/"+id, nil, nil); err != nil && !ec.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
	if r.PortID != "" {
		var port ec.Port
		err = c.Do(ctx, "GET", c.Path("v1", "reserved_fixed_ips")+"/"+r.PortID, nil, &port)
		if !ec.IsNotFound(err) {
			if err != nil {
				return err
			}
			if port.IP != r.IP || port.NetworkID != r.Spec.NetworkID {
				return fmt.Errorf("port identity mismatch")
			}
			if err = c.Do(ctx, "DELETE", c.Path("v1", "reserved_fixed_ips")+"/"+r.PortID, nil, nil); err != nil && !ec.IsNotFound(err) {
				return err
			}
			return nil
		}
	}
	if err = p.Client.Delete(ctx, cm); err != nil {
		return err
	}
	return cp.NewNodeClaimNotFoundError(fmt.Errorf("owned VM, boot volume and port are deleted"))
}
