package main

import (
	"testing"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	caerrors "k8s.io/autoscaler/cluster-autoscaler/utils/errors"
)

type cleanupTestProvider struct {
	cleanupCalls int
}

var _ cloudprovider.CloudProvider = (*cleanupTestProvider)(nil)
func (p *cleanupTestProvider) Name() string { return "test" }

func (p *cleanupTestProvider) NodeGroups() []cloudprovider.NodeGroup { return nil }

func (p *cleanupTestProvider) NodeGroupForNode(*apiv1.Node) (cloudprovider.NodeGroup, error) {
	return nil, nil
}

func (p *cleanupTestProvider) HasInstance(*apiv1.Node) (bool, error) { return true, nil }

func (p *cleanupTestProvider) Pricing() (cloudprovider.PricingModel, caerrors.AutoscalerError) {
	return nil, nil
}

func (p *cleanupTestProvider) GetAvailableMachineTypes() ([]string, error) { return nil, nil }

func (p *cleanupTestProvider) NewNodeGroup(string, map[string]string, map[string]string, []apiv1.Taint, map[string]resource.Quantity) (cloudprovider.NodeGroup, error) {
	return nil, nil
}

func (p *cleanupTestProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) { return nil, nil }

func (p *cleanupTestProvider) GPULabel() string { return "" }

func (p *cleanupTestProvider) GetAvailableGPUTypes() map[string]struct{} { return nil }

func (p *cleanupTestProvider) GetNodeGpuConfig(*apiv1.Node) *cloudprovider.GpuConfig { return nil }

func (p *cleanupTestProvider) Cleanup() error {
	p.cleanupCalls++
	return nil
}

func (p *cleanupTestProvider) Refresh() error { return nil }

func TestCleanupOnceProviderCleanupIsIdempotent(t *testing.T) {
	t.Parallel()

	inner := &cleanupTestProvider{}
	provider := newCleanupOnceProvider(inner)

	if err := provider.Cleanup(); err != nil {
		t.Fatalf("first Cleanup() failed: %v", err)
	}
	if err := provider.Cleanup(); err != nil {
		t.Fatalf("second Cleanup() failed: %v", err)
	}
	if inner.cleanupCalls != 1 {
		t.Fatalf("Cleanup() called inner provider %d times, want 1", inner.cleanupCalls)
	}
}
