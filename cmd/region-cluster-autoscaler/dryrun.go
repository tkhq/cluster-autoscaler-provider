package main

import (
	"context"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/externalgrpc/protos"
	klog "k8s.io/klog/v2"
)

// dryRunWrapper wraps a CloudProviderServer and intercepts mutating calls,
// logging what would have been done instead of performing the action.
type dryRunWrapper struct {
	protos.UnimplementedCloudProviderServer
	inner protos.CloudProviderServer
}

func newDryRunWrapper(inner protos.CloudProviderServer) *dryRunWrapper {
	return &dryRunWrapper{inner: inner}
}

func (w *dryRunWrapper) NodeGroups(ctx context.Context, req *protos.NodeGroupsRequest) (*protos.NodeGroupsResponse, error) {
	return w.inner.NodeGroups(ctx, req)
}

func (w *dryRunWrapper) NodeGroupForNode(ctx context.Context, req *protos.NodeGroupForNodeRequest) (*protos.NodeGroupForNodeResponse, error) {
	return w.inner.NodeGroupForNode(ctx, req)
}

func (w *dryRunWrapper) PricingNodePrice(ctx context.Context, req *protos.PricingNodePriceRequest) (*protos.PricingNodePriceResponse, error) {
	return w.inner.PricingNodePrice(ctx, req)
}

func (w *dryRunWrapper) PricingPodPrice(ctx context.Context, req *protos.PricingPodPriceRequest) (*protos.PricingPodPriceResponse, error) {
	return w.inner.PricingPodPrice(ctx, req)
}

func (w *dryRunWrapper) GPULabel(ctx context.Context, req *protos.GPULabelRequest) (*protos.GPULabelResponse, error) {
	return w.inner.GPULabel(ctx, req)
}

func (w *dryRunWrapper) GetAvailableGPUTypes(ctx context.Context, req *protos.GetAvailableGPUTypesRequest) (*protos.GetAvailableGPUTypesResponse, error) {
	return w.inner.GetAvailableGPUTypes(ctx, req)
}

func (w *dryRunWrapper) Cleanup(ctx context.Context, req *protos.CleanupRequest) (*protos.CleanupResponse, error) {
	return w.inner.Cleanup(ctx, req)
}

func (w *dryRunWrapper) Refresh(ctx context.Context, req *protos.RefreshRequest) (*protos.RefreshResponse, error) {
	return w.inner.Refresh(ctx, req)
}

func (w *dryRunWrapper) NodeGroupTargetSize(ctx context.Context, req *protos.NodeGroupTargetSizeRequest) (*protos.NodeGroupTargetSizeResponse, error) {
	return w.inner.NodeGroupTargetSize(ctx, req)
}

func (w *dryRunWrapper) NodeGroupNodes(ctx context.Context, req *protos.NodeGroupNodesRequest) (*protos.NodeGroupNodesResponse, error) {
	return w.inner.NodeGroupNodes(ctx, req)
}

func (w *dryRunWrapper) NodeGroupTemplateNodeInfo(ctx context.Context, req *protos.NodeGroupTemplateNodeInfoRequest) (*protos.NodeGroupTemplateNodeInfoResponse, error) {
	return w.inner.NodeGroupTemplateNodeInfo(ctx, req)
}

func (w *dryRunWrapper) NodeGroupGetOptions(ctx context.Context, req *protos.NodeGroupAutoscalingOptionsRequest) (*protos.NodeGroupAutoscalingOptionsResponse, error) {
	return w.inner.NodeGroupGetOptions(ctx, req)
}

// Mutating methods: log and return success without performing the action.

func (w *dryRunWrapper) NodeGroupIncreaseSize(ctx context.Context, req *protos.NodeGroupIncreaseSizeRequest) (*protos.NodeGroupIncreaseSizeResponse, error) {
	klog.Infof("DRY-RUN: would increase size of node group %q by %d", req.GetId(), req.GetDelta())
	return &protos.NodeGroupIncreaseSizeResponse{}, nil
}

func (w *dryRunWrapper) NodeGroupDeleteNodes(ctx context.Context, req *protos.NodeGroupDeleteNodesRequest) (*protos.NodeGroupDeleteNodesResponse, error) {
	nodeIDs := make([]string, len(req.GetNodes()))
	for i, n := range req.GetNodes() {
		nodeIDs[i] = n.GetName()
		if n.GetProviderID() != "" {
			nodeIDs[i] = n.GetProviderID()
		}
	}
	klog.Infof("DRY-RUN: would delete %d node(s) from node group %q: %v", len(nodeIDs), req.GetId(), nodeIDs)
	return &protos.NodeGroupDeleteNodesResponse{}, nil
}

func (w *dryRunWrapper) NodeGroupDecreaseTargetSize(ctx context.Context, req *protos.NodeGroupDecreaseTargetSizeRequest) (*protos.NodeGroupDecreaseTargetSizeResponse, error) {
	klog.Infof("DRY-RUN: would decrease target size of node group %q by %d", req.GetId(), req.GetDelta())
	return &protos.NodeGroupDecreaseTargetSizeResponse{}, nil
}
