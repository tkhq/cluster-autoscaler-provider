package main

import (
	"fmt"
	"os"
	"strings"
	"sync"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/simulator/framework"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
)

var awsRegionEnvMu sync.Mutex

type regionalProvider struct {
	region   string
	provider cloudprovider.CloudProvider
}

type multiRegionCloudProvider struct {
	providers []regionalProvider
	primary   cloudprovider.CloudProvider
}

type regionalNodeGroup struct {
	region string
	group  cloudprovider.NodeGroup
}

func parseAWSRegions(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	regions := make([]string, 0, len(values))

	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			region := strings.TrimSpace(item)
			if region == "" {
				continue
			}
			if _, ok := seen[region]; ok {
				continue
			}
			seen[region] = struct{}{}
			regions = append(regions, region)
		}
	}

	return regions
}

func buildProviderForRegion(
	region string,
	build func() cloudprovider.CloudProvider,
) cloudprovider.CloudProvider {
	awsRegionEnvMu.Lock()
	defer awsRegionEnvMu.Unlock()

	previous, hadPrevious := os.LookupEnv("AWS_REGION")
	if err := os.Setenv("AWS_REGION", region); err != nil {
		panic(fmt.Sprintf("failed to set AWS_REGION for %q: %v", region, err))
	}
	defer func() {
		if hadPrevious {
			_ = os.Setenv("AWS_REGION", previous)
			return
		}
		_ = os.Unsetenv("AWS_REGION")
	}()

	// Upstream's AWS provider is single-region and does not expose a public
	// constructor that accepts an explicit region. We keep one upstream provider
	// per region and scope region selection to construction time here rather than
	// reaching into upstream internals via reflection.
	return build()
}

func newMultiRegionCloudProvider(providers []regionalProvider) cloudprovider.CloudProvider {
	if len(providers) == 1 {
		return providers[0].provider
	}

	return &multiRegionCloudProvider{
		providers: providers,
		primary:   providers[0].provider,
	}
}

func regionFromProviderID(providerID string) string {
	if !strings.HasPrefix(providerID, "aws:///") {
		return ""
	}

	parts := strings.Split(strings.TrimPrefix(providerID, "aws:///"), "/")
	if len(parts) < 2 {
		return ""
	}

	zone := strings.TrimSpace(parts[0])
	if len(zone) < 2 {
		return ""
	}

	return zone[:len(zone)-1]
}

func (p *multiRegionCloudProvider) providerForNode(node *apiv1.Node) cloudprovider.CloudProvider {
	region := regionFromProviderID(node.Spec.ProviderID)
	if region == "" {
		return nil
	}

	for _, provider := range p.providers {
		if provider.region == region {
			return provider.provider
		}
	}

	return nil
}

func (p *multiRegionCloudProvider) providerByRegion(region string) cloudprovider.CloudProvider {
	for _, provider := range p.providers {
		if provider.region == region {
			return provider.provider
		}
	}
	return nil
}

func (p *multiRegionCloudProvider) Name() string {
	return cloudprovider.AwsProviderName
}

func (p *multiRegionCloudProvider) NodeGroups() []cloudprovider.NodeGroup {
	var groups []cloudprovider.NodeGroup
	for _, provider := range p.providers {
		for _, group := range provider.provider.NodeGroups() {
			groups = append(groups, &regionalNodeGroup{
				region: provider.region,
				group:  group,
			})
		}
	}
	return groups
}

func (p *multiRegionCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	if provider := p.providerForNode(node); provider != nil {
		group, err := provider.NodeGroupForNode(node)
		if group != nil || err != nil {
			return wrapRegionalNodeGroup(regionFromProviderID(node.Spec.ProviderID), group), err
		}
	}

	for _, regional := range p.providers {
		group, err := regional.provider.NodeGroupForNode(node)
		if group != nil || err != nil {
			return wrapRegionalNodeGroup(regional.region, group), err
		}
	}

	return nil, nil
}

func (p *multiRegionCloudProvider) HasInstance(node *apiv1.Node) (bool, error) {
	if provider := p.providerForNode(node); provider != nil {
		found, err := provider.HasInstance(node)
		if found || err != nil {
			return found, err
		}
	}

	for _, regional := range p.providers {
		found, err := regional.provider.HasInstance(node)
		if found || err != nil {
			return found, err
		}
	}

	return false, nil
}

func (p *multiRegionCloudProvider) Pricing() (cloudprovider.PricingModel, errors.AutoscalerError) {
	return p.primary.Pricing()
}

func (p *multiRegionCloudProvider) GetAvailableMachineTypes() ([]string, error) {
	return p.primary.GetAvailableMachineTypes()
}

func (p *multiRegionCloudProvider) NewNodeGroup(
	machineType string,
	labels map[string]string,
	systemLabels map[string]string,
	taints []apiv1.Taint,
	extraResources map[string]resource.Quantity,
) (cloudprovider.NodeGroup, error) {
	group, err := p.primary.NewNodeGroup(machineType, labels, systemLabels, taints, extraResources)
	return wrapRegionalNodeGroup(p.providers[0].region, group), err
}

func (p *multiRegionCloudProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return p.primary.GetResourceLimiter()
}

func (p *multiRegionCloudProvider) GPULabel() string {
	return p.primary.GPULabel()
}

func (p *multiRegionCloudProvider) GetAvailableGPUTypes() map[string]struct{} {
	return p.primary.GetAvailableGPUTypes()
}

func (p *multiRegionCloudProvider) GetNodeGpuConfig(node *apiv1.Node) *cloudprovider.GpuConfig {
	if provider := p.providerForNode(node); provider != nil {
		return provider.GetNodeGpuConfig(node)
	}
	return p.primary.GetNodeGpuConfig(node)
}

func (p *multiRegionCloudProvider) Cleanup() error {
	for _, provider := range p.providers {
		if err := provider.provider.Cleanup(); err != nil {
			return err
		}
	}
	return nil
}

func (p *multiRegionCloudProvider) Refresh() error {
	for _, provider := range p.providers {
		if err := provider.provider.Refresh(); err != nil {
			return err
		}
	}
	return nil
}

func wrapRegionalNodeGroup(region string, group cloudprovider.NodeGroup) cloudprovider.NodeGroup {
	if group == nil {
		return nil
	}

	if wrapped, ok := group.(*regionalNodeGroup); ok {
		return wrapped
	}

	return &regionalNodeGroup{
		region: region,
		group:  group,
	}
}

func (g *regionalNodeGroup) MaxSize() int {
	return g.group.MaxSize()
}

func (g *regionalNodeGroup) MinSize() int {
	return g.group.MinSize()
}

func (g *regionalNodeGroup) TargetSize() (int, error) {
	return g.group.TargetSize()
}

func (g *regionalNodeGroup) IncreaseSize(delta int) error {
	return g.group.IncreaseSize(delta)
}

func (g *regionalNodeGroup) AtomicIncreaseSize(delta int) error {
	return g.group.AtomicIncreaseSize(delta)
}

func (g *regionalNodeGroup) DeleteNodes(nodes []*apiv1.Node) error {
	return g.group.DeleteNodes(nodes)
}

func (g *regionalNodeGroup) ForceDeleteNodes(nodes []*apiv1.Node) error {
	return g.group.ForceDeleteNodes(nodes)
}

func (g *regionalNodeGroup) DecreaseTargetSize(delta int) error {
	return g.group.DecreaseTargetSize(delta)
}

func (g *regionalNodeGroup) Id() string {
	return fmt.Sprintf("%s/%s", g.region, g.group.Id())
}

func (g *regionalNodeGroup) Debug() string {
	return fmt.Sprintf("%s [%s]", g.group.Debug(), g.region)
}

func (g *regionalNodeGroup) Nodes() ([]cloudprovider.Instance, error) {
	return g.group.Nodes()
}

func (g *regionalNodeGroup) TemplateNodeInfo() (*framework.NodeInfo, error) {
	return g.group.TemplateNodeInfo()
}

func (g *regionalNodeGroup) Exist() bool {
	return g.group.Exist()
}

func (g *regionalNodeGroup) Create() (cloudprovider.NodeGroup, error) {
	group, err := g.group.Create()
	return wrapRegionalNodeGroup(g.region, group), err
}

func (g *regionalNodeGroup) Delete() error {
	return g.group.Delete()
}

func (g *regionalNodeGroup) Autoprovisioned() bool {
	return g.group.Autoprovisioned()
}

func (g *regionalNodeGroup) GetOptions(defaults config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	return g.group.GetOptions(defaults)
}
