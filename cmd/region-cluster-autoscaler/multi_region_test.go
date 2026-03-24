package main

import (
	"errors"
	"reflect"
	"testing"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/simulator/framework"
	caerrors "k8s.io/autoscaler/cluster-autoscaler/utils/errors"
)

func TestParseAWSRegions(t *testing.T) {
	t.Parallel()

	got := parseAWSRegions([]string{"us-east-1, us-west-2", "us-east-1", "eu-west-1"})
	want := []string{"eu-west-1", "us-east-1", "us-west-2"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAWSRegions() = %v, want %v", got, want)
	}
}

func TestRegionFromProviderID(t *testing.T) {
	t.Parallel()

	if got := regionFromProviderID("aws:///us-east-1a/i-123"); got != "us-east-1" {
		t.Fatalf("regionFromProviderID() = %q, want %q", got, "us-east-1")
	}
}

func TestMultiRegionNodeGroupsPrefixIDs(t *testing.T) {
	t.Parallel()

	provider := newMultiRegionCloudProvider([]regionalProvider{
		{
			region:   "us-east-1",
			provider: &fakeCloudProvider{groups: []cloudprovider.NodeGroup{&fakeNodeGroup{id: "asg-a"}}},
		},
		{
			region:   "us-west-2",
			provider: &fakeCloudProvider{groups: []cloudprovider.NodeGroup{&fakeNodeGroup{id: "asg-a"}}},
		},
	})

	groups := provider.NodeGroups()
	if len(groups) != 2 {
		t.Fatalf("len(NodeGroups()) = %d, want 2", len(groups))
	}

	got := []string{groups[0].Id(), groups[1].Id()}
	want := []string{"us-east-1/asg-a", "us-west-2/asg-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NodeGroups IDs = %v, want %v", got, want)
	}
}

func TestMultiRegionNodeGroupForNodeRoutesByRegion(t *testing.T) {
	t.Parallel()

	eastProvider := &fakeCloudProvider{
		nodeGroupForNode: map[string]cloudprovider.NodeGroup{
			"aws:///us-east-1a/i-east": &fakeNodeGroup{id: "asg-east"},
		},
	}
	westProvider := &fakeCloudProvider{
		nodeGroupForNode: map[string]cloudprovider.NodeGroup{
			"aws:///us-west-2b/i-west": &fakeNodeGroup{id: "asg-west"},
		},
	}

	provider := newMultiRegionCloudProvider([]regionalProvider{
		{region: "us-east-1", provider: eastProvider},
		{region: "us-west-2", provider: westProvider},
	})

	group, err := provider.NodeGroupForNode(&apiv1.Node{Spec: apiv1.NodeSpec{ProviderID: "aws:///us-west-2b/i-west"}})
	if err != nil {
		t.Fatalf("NodeGroupForNode() error = %v", err)
	}
	if group == nil {
		t.Fatal("NodeGroupForNode() returned nil group")
	}
	if group.Id() != "us-west-2/asg-west" {
		t.Fatalf("NodeGroupForNode().Id() = %q, want %q", group.Id(), "us-west-2/asg-west")
	}
	if eastProvider.nodeGroupForNodeCalls != 0 {
		t.Fatalf("east provider calls = %d, want 0", eastProvider.nodeGroupForNodeCalls)
	}
}

type fakeCloudProvider struct {
	groups                []cloudprovider.NodeGroup
	nodeGroupForNode      map[string]cloudprovider.NodeGroup
	hasInstance           map[string]bool
	hasInstanceErr        error
	nodeGroupForNodeCalls int
}

func (f *fakeCloudProvider) Name() string { return cloudprovider.AwsProviderName }

func (f *fakeCloudProvider) NodeGroups() []cloudprovider.NodeGroup { return f.groups }

func (f *fakeCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	f.nodeGroupForNodeCalls++
	if group, ok := f.nodeGroupForNode[node.Spec.ProviderID]; ok {
		return group, nil
	}
	return nil, nil
}

func (f *fakeCloudProvider) HasInstance(node *apiv1.Node) (bool, error) {
	if f.hasInstanceErr != nil {
		return false, f.hasInstanceErr
	}
	return f.hasInstance[node.Spec.ProviderID], nil
}

func (f *fakeCloudProvider) Pricing() (cloudprovider.PricingModel, caerrors.AutoscalerError) {
	return nil, cloudprovider.ErrNotImplemented
}

func (f *fakeCloudProvider) GetAvailableMachineTypes() ([]string, error) { return nil, nil }

func (f *fakeCloudProvider) NewNodeGroup(string, map[string]string, map[string]string, []apiv1.Taint, map[string]resource.Quantity) (cloudprovider.NodeGroup, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeCloudProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return cloudprovider.NewResourceLimiter(nil, nil), nil
}

func (f *fakeCloudProvider) GPULabel() string { return "" }

func (f *fakeCloudProvider) GetAvailableGPUTypes() map[string]struct{} { return nil }

func (f *fakeCloudProvider) GetNodeGpuConfig(*apiv1.Node) *cloudprovider.GpuConfig { return nil }

func (f *fakeCloudProvider) Cleanup() error { return nil }

func (f *fakeCloudProvider) Refresh() error { return nil }

type fakeNodeGroup struct {
	id string
}

func (f *fakeNodeGroup) MaxSize() int                                   { return 10 }
func (f *fakeNodeGroup) MinSize() int                                   { return 1 }
func (f *fakeNodeGroup) TargetSize() (int, error)                       { return 1, nil }
func (f *fakeNodeGroup) IncreaseSize(int) error                         { return nil }
func (f *fakeNodeGroup) AtomicIncreaseSize(int) error                   { return cloudprovider.ErrNotImplemented }
func (f *fakeNodeGroup) DeleteNodes([]*apiv1.Node) error                { return nil }
func (f *fakeNodeGroup) ForceDeleteNodes([]*apiv1.Node) error           { return cloudprovider.ErrNotImplemented }
func (f *fakeNodeGroup) DecreaseTargetSize(int) error                   { return nil }
func (f *fakeNodeGroup) Id() string                                     { return f.id }
func (f *fakeNodeGroup) Debug() string                                  { return f.id }
func (f *fakeNodeGroup) Nodes() ([]cloudprovider.Instance, error)       { return nil, nil }
func (f *fakeNodeGroup) TemplateNodeInfo() (*framework.NodeInfo, error) { return nil, nil }
func (f *fakeNodeGroup) Exist() bool                                    { return true }
func (f *fakeNodeGroup) Create() (cloudprovider.NodeGroup, error)       { return f, nil }
func (f *fakeNodeGroup) Delete() error                                  { return nil }
func (f *fakeNodeGroup) Autoprovisioned() bool                          { return false }
func (f *fakeNodeGroup) GetOptions(config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	return nil, cloudprovider.ErrNotImplemented
}
