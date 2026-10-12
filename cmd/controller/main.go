package main

import (
	"os"

	api "github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/apis/v1alpha1"
	"github.com/oneclick-llc/karpenter-provider-edgecenter/pkg/provider"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator"
)

func main() {
	if err := api.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}
	namespace := os.Getenv("SYSTEM_NAMESPACE")
	cluster := os.Getenv("EC_CLUSTER_ID")
	if namespace == "" || cluster == "" {
		panic("SYSTEM_NAMESPACE and EC_CLUSTER_ID are required")
	}
	ctx, op := operator.NewOperator()
	direct, err := client.New(op.GetConfig(), client.Options{Scheme: op.GetScheme()})
	if err != nil {
		panic(err)
	}
	raw := provider.New(direct, namespace, cluster)
	cloud := metrics.Decorate(raw)
	clusterState := state.NewCluster(op.Clock, op.GetClient(), cloud)
	if err = raw.Register(ctx, op.Manager); err != nil {
		panic(err)
	}
	op.WithControllers(ctx, controllers.NewControllers(ctx, op.Manager, op.Clock, op.GetClient(), op.EventRecorder, cloud, raw, clusterState, op.InstanceTypeStore, op.PredictionStore)...).Start(ctx)
}
