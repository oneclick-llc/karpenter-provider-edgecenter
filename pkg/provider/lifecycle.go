package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

type failedTask struct{ message string }

func (e *failedTask) Error() string { return e.message }
func (p *Provider) finishTask(ctx context.Context, c *ec.Client, cm *corev1.ConfigMap, r *Launch) error {
	if r.Task == "" {
		return nil
	}
	t, err := c.Task(ctx, r.Task)
	if err != nil {
		return err
	}
	if t.State == "ERROR" {
		if v := ids(t, "instances"); len(v) == 1 {
			r.InstanceID = v[0]
		}
		if v := ids(t, "ports"); r.PortID == "" && len(v) == 1 {
			r.PortID = v[0]
		}
		r.VolumeIDs = append(r.VolumeIDs, ids(t, "volumes")...)
		r.Failure = fmt.Sprintf("cloud task %s failed: %s", r.Task, t.Error)
		r.Intent = ""
		r.Task = ""
		if err = p.save(ctx, cm, r); err != nil {
			return err
		}
		return &failedTask{r.Failure}
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
		var failed *failedTask
		if errors.As(err, &failed) {
			return nil, cp.NewInsufficientCapacityError(err)
		}
		return nil, err
	}
	if r.Failure != "" {
		return nil, cp.NewInsufficientCapacityError(fmt.Errorf("%s", r.Failure))
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
			return nil, p.recordRejection(ctx, cm, r, err)
		}
		if err = p.save(ctx, cm, r); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("reserved IP creation is in progress")
	}
	if r.InstanceID == "" {
		// Metadata allows recovery when the VM request succeeded but its response was lost.
		if r.Intent != "" {
			resourceName := "instances"
			if r.Spec.ComputeType == "BareMetal" {
				resourceName = "bminstances"
			}
			list, err := ec.List[ec.Instance](ctx, c, resourceName)
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
			if r.Spec.BootstrapMode == "UserData" {
				ts, err := p.secret(ctx, r.Spec.Talos.Name)
				if err != nil {
					return nil, err
				}
				config, err := bootstrap.Render(ts["worker.yaml"], r.Spec, r.NodeName, r.IP, r.ProviderID, r.Slot, r.Labels, r.Taints, r.StartupTaints)
				if err != nil {
					return nil, err
				}
				body["user_data"] = base64.StdEncoding.EncodeToString(config)
			}
			r.Intent = "create-instance"
			if err = p.save(ctx, cm, r); err != nil {
				return nil, err
			}
			path := c.Path("v2", "instances")
			if r.Spec.ComputeType == "BareMetal" {
				path = c.Path("v1", "bminstances")
				body["image_id"] = r.Spec.ImageID
				delete(body, "volumes")
				delete(body, "security_groups")
				delete(body, "availability_zone")
			}
			r.Task, err = c.Submit(ctx, path, body)
			if err != nil {
				return nil, p.recordRejection(ctx, cm, r, err)
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
	if len(r.VolumeIDs) == 0 && r.Spec.ComputeType != "BareMetal" {
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
		if r.Spec.ComputeType != "BareMetal" {
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
		}
		ts, err := p.secret(ctx, r.Spec.Talos.Name)
		if err != nil {
			return nil, err
		}
		if r.Spec.BootstrapMode == "UserData" {
			if err = p.Probe(ctx, r.Spec.TalosEndpoints, ts["talosconfig"], r.IP); err != nil {
				return nil, fmt.Errorf("waiting for Talos mTLS through control plane: %w", err)
			}
		} else {
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
	if cp.IsNodeClaimNotFoundError(err) && nc.Status.ProviderID == "" {
		records, e := p.records(ctx)
		if e != nil {
			return e
		}
		for i := range records {
			launch, e := decode(&records[i])
			if e != nil {
				return e
			}
			if launch.UID == string(nc.UID) {
				cm = &records[i]
				r = launch
				err = nil
				break
			}
		}
	}
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
			if _, ok := err.(*failedTask); !ok {
				return err
			}
		}
	}
	if r.Intent != "" && r.Task == "" {
		return fmt.Errorf("cloud request result is unknown; cleanup preserves the slot")
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
			var deletion ec.TaskResponse
			if err = c.Do(ctx, "DELETE", c.Path("v1", "instances")+"/"+r.InstanceID, nil, &deletion); err != nil && !ec.IsNotFound(err) {
				return err
			}
			if len(deletion.Tasks) == 1 {
				r.Intent = "delete-instance"
				r.Task = deletion.Tasks[0]
				if err = p.save(ctx, cm, r); err != nil {
					return err
				}
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

// A definitive HTTP rejection did not start a task. Transport failures remain uncertain.
func (p *Provider) recordRejection(ctx context.Context, cm *corev1.ConfigMap, r *Launch, err error) error {
	if e, ok := err.(*ec.APIError); ok && e.Status >= 400 && e.Status < 500 && e.Status != 408 {
		r.Intent = ""
		r.Task = ""
		if e.Status != 409 && e.Status != 429 {
			r.Failure = err.Error()
		}
		if saveErr := p.save(ctx, cm, r); saveErr != nil {
			return saveErr
		}
	}
	return err
}
