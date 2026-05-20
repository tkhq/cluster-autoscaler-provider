package main

import (
	"sync"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	caerrors "k8s.io/autoscaler/cluster-autoscaler/utils/errors"
)

type cleanupOnceProvider struct {
	inner cloudprovider.CloudProvider

	cleanupOnce sync.Once
	cleanupErr  error
}

var _ cloudprovider.CloudProvider = (*cleanupOnceProvider)(nil)
func newCleanupOnceProvider(inner cloudprovider.CloudProvider) cloudprovider.CloudProvider {
	return &cleanupOnceProvider{inner: inner}
}

func (p *cleanupOnceProvider) Name() string {
	return p.inner.Name()
}

func (p *cleanupOnceProvider) NodeGroups() []cloudprovider.NodeGroup {
	return p.inner.NodeGroups()
}

func (p *cleanupOnceProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	return p.inner.NodeGroupForNode(node)
}

func (p *cleanupOnceProvider) HasInstance(node *apiv1.Node) (bool, error) {
	return p.inner.HasInstance(node)
}

func (p *cleanupOnceProvider) Pricing() (cloudprovider.PricingModel, caerrors.AutoscalerError) {
	return p.inner.Pricing()
}

func (p *cleanupOnceProvider) GetAvailableMachineTypes() ([]string, error) {
	return p.inner.GetAvailableMachineTypes()
}

func (p *cleanupOnceProvider) NewNodeGroup(machineType string, labels map[string]string, systemLabels map[string]string, taints []apiv1.Taint, extraResources map[string]resource.Quantity) (cloudprovider.NodeGroup, error) {
	return p.inner.NewNodeGroup(machineType, labels, systemLabels, taints, extraResources)
}

func (p *cleanupOnceProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return p.inner.GetResourceLimiter()
}

func (p *cleanupOnceProvider) GPULabel() string {
	return p.inner.GPULabel()
}

func (p *cleanupOnceProvider) GetAvailableGPUTypes() map[string]struct{} {
	return p.inner.GetAvailableGPUTypes()
}

func (p *cleanupOnceProvider) GetNodeGpuConfig(node *apiv1.Node) *cloudprovider.GpuConfig {
	return p.inner.GetNodeGpuConfig(node)
}

func (p *cleanupOnceProvider) Cleanup() error {
	p.cleanupOnce.Do(func() {
		p.cleanupErr = p.inner.Cleanup()
	})
	return p.cleanupErr
}

func (p *cleanupOnceProvider) Refresh() error {
	return p.inner.Refresh()
}
