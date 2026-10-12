package provider

import (
	"context"
	"fmt"
	"time"

	api "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	cp "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type classReconciler struct{ p *Provider }

func (r *classReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	n := new(api.EdgeCenterNodeClass)
	if err := r.p.Client.Get(ctx, req.NamespacedName, n); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := n.DeepCopy()
	err := n.Spec.Validate()
	if err == nil && n.Spec.ClusterID != r.p.ClusterID {
		err = fmt.Errorf("NodeClass belongs to another cluster")
	}
	if err == nil {
		for _, ref := range []struct {
			name string
			keys []string
		}{{n.Spec.Credentials.Name, []string{"token"}}, {n.Spec.Talos.Name, []string{"worker.yaml", "talosconfig"}}, {n.Spec.SSH.Name, []string{"ssh-privatekey", "known_hosts"}}} {
			if ref.name == n.Spec.SSH.Name && n.Spec.BootstrapMode == "UserData" {
				continue
			}
			data, e := r.p.secret(ctx, ref.name)
			if e != nil {
				err = e
				break
			}
			for _, key := range ref.keys {
				if len(data[key]) == 0 {
					err = fmt.Errorf("secret %s lacks %s", ref.name, key)
				}
			}
		}
	}
	if err != nil {
		n.StatusConditions().SetFalse("Ready", "InvalidConfiguration", err.Error())
	} else {
		n.StatusConditions().SetTrue("Ready")
	}
	if e := r.p.Client.Status().Patch(ctx, n, client.MergeFrom(before)); e != nil {
		return ctrl.Result{}, e
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

func (p *Provider) Register(ctx context.Context, mgr manager.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).Named("edgecenter.nodeclass").For(&api.EdgeCenterNodeClass{}).Complete(&classReconciler{p}); err != nil {
		return err
	}
	return mgr.Add(&reaper{p: p})
}

type reaper struct{ p *Provider }

func (*reaper) NeedLeaderElection() bool { return true }
func (r *reaper) Start(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.p.reap(ctx); err != nil {
				log.FromContext(ctx).Error(err, "EdgeCenter orphan cleanup did not complete")
			}
		}
	}
}
func (p *Provider) reap(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	records, err := p.records(ctx)
	if err != nil {
		return err
	}
	for i := range records {
		r, err := decode(&records[i])
		if err != nil {
			return err
		}
		var nc v1.NodeClaim
		err = p.Client.Get(ctx, types.NamespacedName{Name: r.Claim}, &nc)
		if err == nil && string(nc.UID) == r.UID {
			continue
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		var node corev1.Node
		err = p.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, &node)
		if err == nil {
			return fmt.Errorf("orphan %s still has a Kubernetes Node; cleanup requires eviction first", r.Claim)
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		if r.Intent != "" && r.Task == "" {
			return fmt.Errorf("orphan %s has an uncertain cloud request; keep its slot for recovery", r.Claim)
		}
		if err = p.delete(ctx, &records[i], r); err != nil && !cp.IsNodeClaimNotFoundError(err) {
			return err
		}
	}
	return nil
}
