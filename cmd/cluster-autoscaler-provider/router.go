package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/externalgrpc/protos"
	k8smetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	klog "k8s.io/klog/v2"
)

const defaultProviderProbeInterval = 5 * time.Second

var (
	registerRouterMetricsOnce sync.Once

	routerConfiguredProviders = k8smetrics.NewGauge(
		&k8smetrics.GaugeOpts{
			Namespace: "cluster_autoscaler_provider",
			Subsystem: "router",
			Name:      "configured_providers",
			Help:      "Number of configured backend providers.",
		},
	)
	routerHealthyProviders = k8smetrics.NewGauge(
		&k8smetrics.GaugeOpts{
			Namespace: "cluster_autoscaler_provider",
			Subsystem: "router",
			Name:      "healthy_providers",
			Help:      "Number of backend providers currently marked healthy.",
		},
	)
	routerUnhealthyProviders = k8smetrics.NewGauge(
		&k8smetrics.GaugeOpts{
			Namespace: "cluster_autoscaler_provider",
			Subsystem: "router",
			Name:      "unhealthy_providers",
			Help:      "Number of backend providers currently marked unhealthy.",
		},
	)
)

type regionalClient struct {
	provider   string
	region     string
	rpcTimeout time.Duration
	client     protos.CloudProviderClient
	conn       *grpc.ClientConn
}

type providerStatus struct {
	healthy     bool
	lastChecked time.Time
	lastError   string
}

type CachingRouter struct {
	protos.UnimplementedCloudProviderServer

	clients     map[string]regionalClient
	clientOrder []string

	mu                sync.RWMutex
	cache             map[string]cacheEntry
	cacheTTL          time.Duration
	backendRPCTimeout time.Duration
	providerState     map[string]providerStatus
	closeOnce         sync.Once
	closeErr          error
}

var _ protos.CloudProviderServer = (*CachingRouter)(nil)
type cacheEntry struct {
	data       interface{}
	expiration time.Time
}

func registerRouterMetrics() {
	registerRouterMetricsOnce.Do(func() {
		legacyregistry.MustRegister(
			routerConfiguredProviders,
			routerHealthyProviders,
			routerUnhealthyProviders,
		)
	})
}

func newCachingRouter(opts RouterOptions) (*CachingRouter, error) {
	registerRouterMetrics()

	clients := make(map[string]regionalClient, len(opts.Backends))
	clientOrder := make([]string, 0, len(opts.Backends))
	providerState := make(map[string]providerStatus, len(opts.Backends))
	for _, backend := range opts.Backends {
		klog.Infof("configuring router backend region=%s address=%s provider=%s", backend.Region, backend.Address, backend.Provider)

		conn, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("failed to initialize backend client for %s: %w", backend.Address, err)
		}
		clients[backend.Region] = regionalClient{
			provider:   backend.Provider,
			region:     backend.Region,
			rpcTimeout: backend.RPCTimeout,
			client:     protos.NewCloudProviderClient(conn),
			conn:       conn,
		}
		clientOrder = append(clientOrder, backend.Region)
		providerState[backend.Region] = providerStatus{}
	}

	r := &CachingRouter{
		clients:           clients,
		clientOrder:       clientOrder,
		cache:             make(map[string]cacheEntry),
		cacheTTL:          opts.CacheTTL,
		backendRPCTimeout: opts.BackendRPCTimeout,
		providerState:     providerState,
	}
	r.updateProviderMetricsLocked()
	return r, nil
}

func (r *CachingRouter) Start(ctx context.Context) {
	go func() {
		r.probeProviders(ctx)

		ticker := time.NewTicker(defaultProviderProbeInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if r.HealthyProviderCount() > 0 {
					continue
				}
				r.probeProviders(ctx)
			}
		}
	}()
}

func (r *CachingRouter) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		var errs []error
		if err := r.cleanupProviders(ctx); err != nil {
			errs = append(errs, err)
		}
		for _, client := range r.clients {
			if err := client.conn.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close backend connection for region %s: %w", client.region, err))
			}
		}
		if len(errs) > 0 {
			r.closeErr = fmt.Errorf("failed to close backend connections: %w", errors.Join(errs...))
		}
	})
	return r.closeErr
}

func (r *CachingRouter) HealthyProviderCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, status := range r.providerState {
		if status.healthy {
			count++
		}
	}
	return count
}

func (r *CachingRouter) ConfiguredProviderCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providerState)
}

func (r *CachingRouter) UnhealthyProviderCount() int {
	return r.ConfiguredProviderCount() - r.HealthyProviderCount()
}

func (r *CachingRouter) probeProviders(ctx context.Context) {
	type probeResult struct {
		region string
		err    error
	}

	results := make(chan probeResult, len(r.clients))
	var wg sync.WaitGroup

	for _, client := range r.clients {
		wg.Add(1)
		go func(rc regionalClient) {
			defer wg.Done()

			probeCtx, cancel := r.backendContext(ctx, rc)
			defer cancel()

			_, err := rc.client.NodeGroups(probeCtx, &protos.NodeGroupsRequest{})
			results <- probeResult{region: rc.region, err: err}
		}(client)
	}

	wg.Wait()
	close(results)

	for result := range results {
		if result.err != nil {
			r.markProviderUnhealthy(result.region, result.err)
			continue
		}
		r.markProviderHealthy(result.region)
	}
}

func (r *CachingRouter) getClientForNode(providerID string) (regionalClient, error) {
	region := regionFromProviderID(providerID)
	if region == "" {
		return regionalClient{}, fmt.Errorf("could not determine region from providerID %q", providerID)
	}
	return r.getClientForRegion(region)
}

func (r *CachingRouter) getClientForRegion(region string) (regionalClient, error) {
	client, ok := r.clients[region]
	if !ok {
		return regionalClient{}, fmt.Errorf("no backend configured for region %q", region)
	}
	return client, nil
}

func (r *CachingRouter) getClientForGroup(groupID string) (regionalClient, string, error) {
	parts := strings.SplitN(groupID, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return regionalClient{}, "", fmt.Errorf("invalid group ID %q: expected region/id format", groupID)
	}

	client, err := r.getClientForRegion(parts[0])
	if err != nil {
		return regionalClient{}, "", err
	}
	return client, parts[1], nil
}

func (r *CachingRouter) getHealthyClients() []regionalClient {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clients := make([]regionalClient, 0, len(r.clientOrder))
	for _, region := range r.clientOrder {
		if !r.providerState[region].healthy {
			continue
		}
		clients = append(clients, r.clients[region])
	}
	return clients
}

func (r *CachingRouter) backendContext(parent context.Context, client regionalClient) (context.Context, context.CancelFunc) {
	timeout := r.backendRPCTimeout
	if client.rpcTimeout > 0 {
		timeout = client.rpcTimeout
	}
	return context.WithTimeout(parent, timeout)
}

func (r *CachingRouter) clearCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearCacheLocked()
}

func (r *CachingRouter) clearCacheLocked() {
	r.cache = make(map[string]cacheEntry)
}

func (r *CachingRouter) updateProviderMetricsLocked() {
	configured := len(r.providerState)
	healthy := 0
	for _, status := range r.providerState {
		if status.healthy {
			healthy++
		}
	}

	routerConfiguredProviders.Set(float64(configured))
	routerHealthyProviders.Set(float64(healthy))
	routerUnhealthyProviders.Set(float64(configured - healthy))
}

func (r *CachingRouter) markProviderHealthy(region string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	status := r.providerState[region]
	wasHealthy := status.healthy
	status.healthy = true
	status.lastChecked = time.Now()
	status.lastError = ""
	r.providerState[region] = status
	if !wasHealthy {
		klog.Infof("provider connected for region %s", region)
	}
	r.updateProviderMetricsLocked()
}

func (r *CachingRouter) markProviderUnhealthy(region string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	status := r.providerState[region]
	wasHealthy := status.healthy
	status.healthy = false
	status.lastChecked = time.Now()
	if err != nil {
		status.lastError = err.Error()
	}
	r.providerState[region] = status
	if wasHealthy && err != nil {
		klog.Errorf("provider connection lost for region %s: %v", region, err)
	}

	healthy := 0
	for _, provider := range r.providerState {
		if provider.healthy {
			healthy++
		}
	}
	if healthy == 0 {
		r.clearCacheLocked()
	}
	r.updateProviderMetricsLocked()
}

func (r *CachingRouter) NodeGroups(ctx context.Context, req *protos.NodeGroupsRequest) (*protos.NodeGroupsResponse, error) {
	r.mu.RLock()
	if entry, ok := r.cache["NodeGroups"]; ok && time.Now().Before(entry.expiration) {
		r.mu.RUnlock()
		return proto.Clone(entry.data.(*protos.NodeGroupsResponse)).(*protos.NodeGroupsResponse), nil
	}
	r.mu.RUnlock()

	type result struct {
		region string
		resp   *protos.NodeGroupsResponse
		err    error
	}

	results := make(chan result, len(r.clients))
	var wg sync.WaitGroup
	for _, client := range r.clients {
		wg.Add(1)
		go func(rc regionalClient) {
			defer wg.Done()

			callCtx, cancel := r.backendContext(ctx, rc)
			defer cancel()

			resp, err := rc.client.NodeGroups(callCtx, req)
			results <- result{region: rc.region, resp: resp, err: err}
		}(client)
	}

	wg.Wait()
	close(results)

	allGroups := make([]*protos.NodeGroup, 0)
	successes := 0
	for result := range results {
		if result.err != nil {
			klog.Errorf("failed to fetch node groups from %s: %v", result.region, result.err)
			r.markProviderUnhealthy(result.region, result.err)
			continue
		}

		r.markProviderHealthy(result.region)
		successes++
		for _, group := range result.resp.GetNodeGroups() {
			group.Id = fmt.Sprintf("%s/%s", result.region, group.GetId())
			allGroups = append(allGroups, group)
		}
	}

	if successes == 0 {
		return nil, fmt.Errorf("failed to fetch node groups from all configured providers")
	}

	resp := &protos.NodeGroupsResponse{NodeGroups: allGroups}
	r.mu.Lock()
	r.cache["NodeGroups"] = cacheEntry{
		data:       proto.Clone(resp).(*protos.NodeGroupsResponse),
		expiration: time.Now().Add(r.cacheTTL),
	}
	r.mu.Unlock()
	return proto.Clone(resp).(*protos.NodeGroupsResponse), nil
}

func (r *CachingRouter) NodeGroupForNode(ctx context.Context, req *protos.NodeGroupForNodeRequest) (*protos.NodeGroupForNodeResponse, error) {
	if req.GetNode() == nil {
		return nil, fmt.Errorf("node is required")
	}

	client, err := r.getClientForNode(req.GetNode().GetProviderID())
	if err != nil {
		klog.V(4).Infof("node group lookup skipped for unroutable node=%s providerID=%s: %v", req.GetNode().GetName(), req.GetNode().GetProviderID(), err)
		return &protos.NodeGroupForNodeResponse{}, nil
	}

	callCtx, cancel := r.backendContext(ctx, client)
	defer cancel()

	klog.V(6).Infof("node group lookup sent to provider=%s region=%s node=%s providerID=%s", client.provider, client.region, req.GetNode().GetName(), req.GetNode().GetProviderID())
	resp, err := client.client.NodeGroupForNode(callCtx, req)
	if err != nil {
		klog.Errorf("node group lookup failed for provider=%s region=%s node=%s providerID=%s: %v", client.provider, client.region, req.GetNode().GetName(), req.GetNode().GetProviderID(), err)
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}

	r.markProviderHealthy(client.region)
	if resp.GetNodeGroup() != nil && resp.GetNodeGroup().GetId() != "" {
		resp.GetNodeGroup().Id = fmt.Sprintf("%s/%s", client.region, resp.GetNodeGroup().GetId())
	}
	return resp, nil
}

func (r *CachingRouter) NodeGroupIncreaseSize(ctx context.Context, req *protos.NodeGroupIncreaseSizeRequest) (*protos.NodeGroupIncreaseSizeResponse, error) {
	client, backendID, err := r.getClientForGroup(req.GetId())
	if err != nil {
		return nil, err
	}

	callCtx, cancel := r.backendContext(ctx, client)
	defer cancel()

	klog.Infof("scale up sent to provider=%s region=%s group=%s delta=%d", client.provider, client.region, backendID, req.GetDelta())
	resp, err := client.client.NodeGroupIncreaseSize(callCtx, &protos.NodeGroupIncreaseSizeRequest{
		Id:    backendID,
		Delta: req.GetDelta(),
	})
	if err != nil {
		klog.Errorf("scale up failed for provider=%s region=%s group=%s delta=%d: %v", client.provider, client.region, backendID, req.GetDelta(), err)
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}

	r.markProviderHealthy(client.region)
	r.clearCache()
	return resp, nil
}

func (r *CachingRouter) NodeGroupDeleteNodes(ctx context.Context, req *protos.NodeGroupDeleteNodesRequest) (*protos.NodeGroupDeleteNodesResponse, error) {
	client, backendID, err := r.getClientForGroup(req.GetId())
	if err != nil {
		return nil, err
	}

	callCtx, cancel := r.backendContext(ctx, client)
	defer cancel()

	klog.Infof("delete nodes sent to provider=%s region=%s group=%s nodes=%s", client.provider, client.region, backendID, formatProviderIDs(req.GetNodes()))
	resp, err := client.client.NodeGroupDeleteNodes(callCtx, &protos.NodeGroupDeleteNodesRequest{
		Id:    backendID,
		Nodes: req.GetNodes(),
	})
	if err != nil {
		klog.Errorf("delete nodes failed for provider=%s region=%s group=%s nodes=%s: %v", client.provider, client.region, backendID, formatProviderIDs(req.GetNodes()), err)
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}

	r.markProviderHealthy(client.region)
	r.clearCache()
	return resp, nil
}

func (r *CachingRouter) NodeGroupDecreaseTargetSize(ctx context.Context, req *protos.NodeGroupDecreaseTargetSizeRequest) (*protos.NodeGroupDecreaseTargetSizeResponse, error) {
	client, backendID, err := r.getClientForGroup(req.GetId())
	if err != nil {
		return nil, err
	}

	callCtx, cancel := r.backendContext(ctx, client)
	defer cancel()

	klog.Infof("scale down sent to provider=%s region=%s group=%s delta=%d", client.provider, client.region, backendID, req.GetDelta())
	resp, err := client.client.NodeGroupDecreaseTargetSize(callCtx, &protos.NodeGroupDecreaseTargetSizeRequest{
		Id:    backendID,
		Delta: req.GetDelta(),
	})
	if err != nil {
		klog.Errorf("scale down failed for provider=%s region=%s group=%s delta=%d: %v", client.provider, client.region, backendID, req.GetDelta(), err)
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}

	r.markProviderHealthy(client.region)
	r.clearCache()
	return resp, nil
}

func (r *CachingRouter) Refresh(ctx context.Context, req *protos.RefreshRequest) (*protos.RefreshResponse, error) {
	r.clearCache()

	type result struct {
		region string
		err    error
	}

	results := make(chan result, len(r.clients))
	var wg sync.WaitGroup
	for _, client := range r.clients {
		wg.Add(1)
		go func(rc regionalClient) {
			defer wg.Done()

			callCtx, cancel := r.backendContext(ctx, rc)
			defer cancel()

			klog.V(2).Infof("refresh sent to provider=%s region=%s", rc.provider, rc.region)
			_, err := rc.client.Refresh(callCtx, req)
			results <- result{region: rc.region, err: err}
		}(client)
	}

	wg.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result.err != nil {
			klog.Errorf("failed to refresh backend %s: %v", result.region, result.err)
			r.markProviderUnhealthy(result.region, result.err)
			continue
		}
		r.markProviderHealthy(result.region)
		successes++
	}

	if successes == 0 {
		return nil, fmt.Errorf("failed to refresh all configured providers")
	}
	return &protos.RefreshResponse{}, nil
}

func (r *CachingRouter) Cleanup(ctx context.Context, req *protos.CleanupRequest) (*protos.CleanupResponse, error) {
	klog.Infof("ignoring cleanup request from upstream client; provider cleanup is handled on router shutdown")
	return &protos.CleanupResponse{}, nil
}

func (r *CachingRouter) cleanupProviders(ctx context.Context) error {
	type result struct {
		region string
		err    error
	}

	results := make(chan result, len(r.clients))
	var wg sync.WaitGroup
	for _, client := range r.clients {
		wg.Add(1)
		go func(rc regionalClient) {
			defer wg.Done()

			callCtx, cancel := r.backendContext(ctx, rc)
			defer cancel()

			klog.Infof("cleanup sent to provider=%s region=%s", rc.provider, rc.region)
			_, err := rc.client.Cleanup(callCtx, &protos.CleanupRequest{})
			results <- result{region: rc.region, err: err}
		}(client)
	}

	wg.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result.err != nil {
			klog.Errorf("failed to clean up backend %s: %v", result.region, result.err)
			r.markProviderUnhealthy(result.region, result.err)
			continue
		}
		r.markProviderHealthy(result.region)
		successes++
	}

	if successes == 0 {
		return fmt.Errorf("failed to clean up all configured providers")
	}
	return nil
}

func (r *CachingRouter) NodeGroupTargetSize(ctx context.Context, req *protos.NodeGroupTargetSizeRequest) (*protos.NodeGroupTargetSizeResponse, error) {
	client, backendID, err := r.getClientForGroup(req.GetId())
	if err != nil {
		return nil, err
	}

	callCtx, cancel := r.backendContext(ctx, client)
	defer cancel()

	resp, err := client.client.NodeGroupTargetSize(callCtx, &protos.NodeGroupTargetSizeRequest{Id: backendID})
	if err != nil {
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}
	r.markProviderHealthy(client.region)
	return resp, nil
}

func (r *CachingRouter) NodeGroupNodes(ctx context.Context, req *protos.NodeGroupNodesRequest) (*protos.NodeGroupNodesResponse, error) {
	client, backendID, err := r.getClientForGroup(req.GetId())
	if err != nil {
		return nil, err
	}

	callCtx, cancel := r.backendContext(ctx, client)
	defer cancel()

	resp, err := client.client.NodeGroupNodes(callCtx, &protos.NodeGroupNodesRequest{Id: backendID})
	if err != nil {
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}
	r.markProviderHealthy(client.region)
	return resp, nil
}

func (r *CachingRouter) NodeGroupTemplateNodeInfo(ctx context.Context, req *protos.NodeGroupTemplateNodeInfoRequest) (*protos.NodeGroupTemplateNodeInfoResponse, error) {
	client, backendID, err := r.getClientForGroup(req.GetId())
	if err != nil {
		return nil, err
	}

	callCtx, cancel := r.backendContext(ctx, client)
	defer cancel()

	resp, err := client.client.NodeGroupTemplateNodeInfo(callCtx, &protos.NodeGroupTemplateNodeInfoRequest{Id: backendID})
	if err != nil {
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}
	r.markProviderHealthy(client.region)
	return resp, nil
}

func (r *CachingRouter) NodeGroupGetOptions(ctx context.Context, req *protos.NodeGroupAutoscalingOptionsRequest) (*protos.NodeGroupAutoscalingOptionsResponse, error) {
	client, backendID, err := r.getClientForGroup(req.GetId())
	if err != nil {
		return nil, err
	}

	callCtx, cancel := r.backendContext(ctx, client)
	defer cancel()

	resp, err := client.client.NodeGroupGetOptions(callCtx, &protos.NodeGroupAutoscalingOptionsRequest{
		Id:       backendID,
		Defaults: req.GetDefaults(),
	})
	if err != nil {
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}
	r.markProviderHealthy(client.region)
	return resp, nil
}

func (r *CachingRouter) PricingNodePrice(ctx context.Context, req *protos.PricingNodePriceRequest) (*protos.PricingNodePriceResponse, error) {
	for _, client := range r.getHealthyClients() {
		callCtx, cancel := r.backendContext(ctx, client)
		resp, err := client.client.PricingNodePrice(callCtx, req)
		cancel()
		if err != nil {
			r.markProviderUnhealthy(client.region, err)
			klog.Errorf("pricing node price failed for region %s: %v", client.region, err)
			continue
		}
		r.markProviderHealthy(client.region)
		return resp, nil
	}
	return nil, fmt.Errorf("no healthy backends available")
}

func (r *CachingRouter) PricingPodPrice(ctx context.Context, req *protos.PricingPodPriceRequest) (*protos.PricingPodPriceResponse, error) {
	for _, client := range r.getHealthyClients() {
		callCtx, cancel := r.backendContext(ctx, client)
		resp, err := client.client.PricingPodPrice(callCtx, req)
		cancel()
		if err != nil {
			r.markProviderUnhealthy(client.region, err)
			klog.Errorf("pricing pod price failed for region %s: %v", client.region, err)
			continue
		}
		r.markProviderHealthy(client.region)
		return resp, nil
	}
	return nil, fmt.Errorf("no healthy backends available")
}

func (r *CachingRouter) GPULabel(ctx context.Context, req *protos.GPULabelRequest) (*protos.GPULabelResponse, error) {
	for _, client := range r.getHealthyClients() {
		callCtx, cancel := r.backendContext(ctx, client)
		resp, err := client.client.GPULabel(callCtx, req)
		cancel()
		if err != nil {
			r.markProviderUnhealthy(client.region, err)
			klog.Errorf("gpu label lookup failed for region %s: %v", client.region, err)
			continue
		}
		r.markProviderHealthy(client.region)
		return resp, nil
	}
	return nil, fmt.Errorf("no healthy backends available")
}

func (r *CachingRouter) GetAvailableGPUTypes(ctx context.Context, req *protos.GetAvailableGPUTypesRequest) (*protos.GetAvailableGPUTypesResponse, error) {
	for _, client := range r.getHealthyClients() {
		callCtx, cancel := r.backendContext(ctx, client)
		resp, err := client.client.GetAvailableGPUTypes(callCtx, req)
		cancel()
		if err != nil {
			r.markProviderUnhealthy(client.region, err)
			klog.Errorf("available gpu types lookup failed for region %s: %v", client.region, err)
			continue
		}
		r.markProviderHealthy(client.region)
		return resp, nil
	}
	return nil, fmt.Errorf("no healthy backends available")
}

func formatProviderIDs(nodes []*protos.ExternalGrpcNode) string {
	if len(nodes) == 0 {
		return "[]"
	}

	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			ids = append(ids, "<nil>")
			continue
		}
		if node.GetProviderID() != "" {
			ids = append(ids, node.GetProviderID())
			continue
		}
		if node.GetName() != "" {
			ids = append(ids, node.GetName())
			continue
		}
		ids = append(ids, "<unknown>")
	}

	return "[" + strings.Join(ids, ",") + "]"
}
