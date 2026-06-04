package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/externalgrpc/protos"
	k8smetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	klog "k8s.io/klog/v2"
)

const (
	defaultProviderProbeInterval = 5 * time.Second
)

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
	routerBackendRPCTotal = k8smetrics.NewCounterVec(
		&k8smetrics.CounterOpts{
			Namespace: "cluster_autoscaler_provider",
			Subsystem: "router",
			Name:      "backend_rpc_total",
			Help:      "Number of router RPCs sent to backend providers, labeled by result.",
		},
		[]string{"provider", "region", "method", "grpc_code", "outcome"},
	)
	routerBackendRPCDuration = k8smetrics.NewHistogramVec(
		&k8smetrics.HistogramOpts{
			Namespace: "cluster_autoscaler_provider",
			Subsystem: "router",
			Name:      "backend_rpc_duration_seconds",
			Help:      "Duration of router RPCs sent to backend providers, labeled by result.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30, 60},
		},
		[]string{"provider", "region", "method", "grpc_code", "outcome"},
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

type backendRPC struct {
	parent  context.Context
	context context.Context
	cancel  context.CancelFunc
	client  regionalClient
	method  string
	timeout time.Duration
	start   time.Time
}

type backendRPCStatus struct {
	provider   string
	region     string
	method     string
	duration   time.Duration
	timeout    time.Duration
	grpcCode   codes.Code
	outcome    string
	parentErr  error
	backendErr error
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
			routerBackendRPCTotal,
			routerBackendRPCDuration,
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

			call := r.startBackendRPC(ctx, rc, "ProbeNodeGroups")
			_, err := rc.client.NodeGroups(call.Context(), &protos.NodeGroupsRequest{})
			call.Finish(err)
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

func (r *CachingRouter) backendTimeout(client regionalClient) time.Duration {
	timeout := r.backendRPCTimeout
	if client.rpcTimeout > 0 {
		timeout = client.rpcTimeout
	}
	return timeout
}

func (r *CachingRouter) startBackendRPC(parent context.Context, client regionalClient, method string) *backendRPC {
	timeout := r.backendTimeout(client)
	ctx, cancel := context.WithTimeout(parent, timeout)
	return &backendRPC{
		parent:  parent,
		context: ctx,
		cancel:  cancel,
		client:  client,
		method:  method,
		timeout: timeout,
		start:   time.Now(),
	}
}

func (c *backendRPC) Context() context.Context {
	return c.context
}

func (c *backendRPC) Finish(err error) backendRPCStatus {
	duration := time.Since(c.start)
	grpcCode := status.Code(err)
	outcome := backendRPCOutcome(err, c.parent.Err(), c.context.Err())

	routerBackendRPCTotal.WithLabelValues(c.client.provider, c.client.region, c.method, grpcCode.String(), outcome).Inc()
	routerBackendRPCDuration.WithLabelValues(c.client.provider, c.client.region, c.method, grpcCode.String(), outcome).Observe(duration.Seconds())

	c.cancel()
	return backendRPCStatus{
		provider:   c.client.provider,
		region:     c.client.region,
		method:     c.method,
		duration:   duration,
		timeout:    c.timeout,
		grpcCode:   grpcCode,
		outcome:    outcome,
		parentErr:  c.parent.Err(),
		backendErr: c.context.Err(),
	}
}

func backendRPCOutcome(err error, parentErr error, backendErr error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(parentErr, context.DeadlineExceeded) {
		return "caller_deadline"
	}
	if errors.Is(parentErr, context.Canceled) {
		return "caller_canceled"
	}
	if errors.Is(backendErr, context.DeadlineExceeded) {
		return "backend_deadline"
	}
	if errors.Is(backendErr, context.Canceled) {
		return "backend_canceled"
	}
	return "error"
}

func (s backendRPCStatus) logValues() string {
	return fmt.Sprintf(
		"provider=%s region=%s method=%s duration=%s timeout=%s grpc_code=%s outcome=%s parent_err=%s backend_err=%s",
		s.provider,
		s.region,
		s.method,
		s.duration.Round(time.Millisecond),
		s.timeout,
		s.grpcCode.String(),
		s.outcome,
		contextErrString(s.parentErr),
		contextErrString(s.backendErr),
	)
}

func contextErrString(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
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
		status backendRPCStatus
	}

	results := make(chan result, len(r.clients))
	var wg sync.WaitGroup
	for _, client := range r.clients {
		wg.Add(1)
		go func(rc regionalClient) {
			defer wg.Done()

			call := r.startBackendRPC(ctx, rc, "NodeGroups")
			resp, err := rc.client.NodeGroups(call.Context(), req)
			results <- result{region: rc.region, resp: resp, err: err, status: call.Finish(err)}
		}(client)
	}

	wg.Wait()
	close(results)

	allGroups := make([]*protos.NodeGroup, 0)
	successes := 0
	for result := range results {
		if result.err != nil {
			klog.Errorf("failed to fetch node groups from backend %s: %v", result.status.logValues(), result.err)
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

	klog.V(6).Infof("node group lookup sent to provider=%s region=%s node=%s providerID=%s", client.provider, client.region, req.GetNode().GetName(), req.GetNode().GetProviderID())
	call := r.startBackendRPC(ctx, client, "NodeGroupForNode")
	resp, err := client.client.NodeGroupForNode(call.Context(), req)
	callStatus := call.Finish(err)
	if err != nil {
		klog.Errorf("node group lookup failed for node=%s providerID=%s %s: %v", req.GetNode().GetName(), req.GetNode().GetProviderID(), callStatus.logValues(), err)
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

	klog.Infof("scale up sent to provider=%s region=%s group=%s delta=%d", client.provider, client.region, backendID, req.GetDelta())
	call := r.startBackendRPC(ctx, client, "NodeGroupIncreaseSize")
	resp, err := client.client.NodeGroupIncreaseSize(call.Context(), &protos.NodeGroupIncreaseSizeRequest{
		Id:    backendID,
		Delta: req.GetDelta(),
	})
	callStatus := call.Finish(err)
	if err != nil {
		klog.Errorf("scale up failed for group=%s delta=%d %s: %v", backendID, req.GetDelta(), callStatus.logValues(), err)
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

	klog.Infof("delete nodes sent to provider=%s region=%s group=%s nodes=%s", client.provider, client.region, backendID, formatProviderIDs(req.GetNodes()))
	call := r.startBackendRPC(ctx, client, "NodeGroupDeleteNodes")
	resp, err := client.client.NodeGroupDeleteNodes(call.Context(), &protos.NodeGroupDeleteNodesRequest{
		Id:    backendID,
		Nodes: req.GetNodes(),
	})
	callStatus := call.Finish(err)
	if err != nil {
		klog.Errorf("delete nodes failed for group=%s nodes=%s %s: %v", backendID, formatProviderIDs(req.GetNodes()), callStatus.logValues(), err)
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

	klog.Infof("scale down sent to provider=%s region=%s group=%s delta=%d", client.provider, client.region, backendID, req.GetDelta())
	call := r.startBackendRPC(ctx, client, "NodeGroupDecreaseTargetSize")
	resp, err := client.client.NodeGroupDecreaseTargetSize(call.Context(), &protos.NodeGroupDecreaseTargetSizeRequest{
		Id:    backendID,
		Delta: req.GetDelta(),
	})
	callStatus := call.Finish(err)
	if err != nil {
		klog.Errorf("scale down failed for group=%s delta=%d %s: %v", backendID, req.GetDelta(), callStatus.logValues(), err)
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
		status backendRPCStatus
	}

	results := make(chan result, len(r.clients))
	var wg sync.WaitGroup
	for _, client := range r.clients {
		wg.Add(1)
		go func(rc regionalClient) {
			defer wg.Done()

			klog.V(2).Infof("refresh sent to provider=%s region=%s", rc.provider, rc.region)
			call := r.startBackendRPC(ctx, rc, "Refresh")
			_, err := rc.client.Refresh(call.Context(), req)
			results <- result{region: rc.region, err: err, status: call.Finish(err)}
		}(client)
	}

	wg.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result.err != nil {
			klog.Errorf("failed to refresh backend %s: %v", result.status.logValues(), result.err)
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
		status backendRPCStatus
	}

	results := make(chan result, len(r.clients))
	var wg sync.WaitGroup
	for _, client := range r.clients {
		wg.Add(1)
		go func(rc regionalClient) {
			defer wg.Done()

			klog.Infof("cleanup sent to provider=%s region=%s", rc.provider, rc.region)
			call := r.startBackendRPC(ctx, rc, "Cleanup")
			_, err := rc.client.Cleanup(call.Context(), &protos.CleanupRequest{})
			results <- result{region: rc.region, err: err, status: call.Finish(err)}
		}(client)
	}

	wg.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result.err != nil {
			klog.Errorf("failed to clean up backend %s: %v", result.status.logValues(), result.err)
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

	call := r.startBackendRPC(ctx, client, "NodeGroupTargetSize")
	resp, err := client.client.NodeGroupTargetSize(call.Context(), &protos.NodeGroupTargetSizeRequest{Id: backendID})
	call.Finish(err)
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

	call := r.startBackendRPC(ctx, client, "NodeGroupNodes")
	resp, err := client.client.NodeGroupNodes(call.Context(), &protos.NodeGroupNodesRequest{Id: backendID})
	call.Finish(err)
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

	call := r.startBackendRPC(ctx, client, "NodeGroupTemplateNodeInfo")
	resp, err := client.client.NodeGroupTemplateNodeInfo(call.Context(), &protos.NodeGroupTemplateNodeInfoRequest{Id: backendID})
	call.Finish(err)
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

	call := r.startBackendRPC(ctx, client, "NodeGroupGetOptions")
	resp, err := client.client.NodeGroupGetOptions(call.Context(), &protos.NodeGroupAutoscalingOptionsRequest{
		Id:       backendID,
		Defaults: req.GetDefaults(),
	})
	call.Finish(err)
	if err != nil {
		r.markProviderUnhealthy(client.region, err)
		return nil, err
	}
	r.markProviderHealthy(client.region)
	return resp, nil
}

func (r *CachingRouter) PricingNodePrice(ctx context.Context, req *protos.PricingNodePriceRequest) (*protos.PricingNodePriceResponse, error) {
	for _, client := range r.getHealthyClients() {
		call := r.startBackendRPC(ctx, client, "PricingNodePrice")
		resp, err := client.client.PricingNodePrice(call.Context(), req)
		callStatus := call.Finish(err)
		if err != nil {
			r.markProviderUnhealthy(client.region, err)
			klog.Errorf("pricing node price failed for %s: %v", callStatus.logValues(), err)
			continue
		}
		r.markProviderHealthy(client.region)
		return resp, nil
	}
	return nil, fmt.Errorf("no healthy backends available")
}

func (r *CachingRouter) PricingPodPrice(ctx context.Context, req *protos.PricingPodPriceRequest) (*protos.PricingPodPriceResponse, error) {
	for _, client := range r.getHealthyClients() {
		call := r.startBackendRPC(ctx, client, "PricingPodPrice")
		resp, err := client.client.PricingPodPrice(call.Context(), req)
		callStatus := call.Finish(err)
		if err != nil {
			r.markProviderUnhealthy(client.region, err)
			klog.Errorf("pricing pod price failed for %s: %v", callStatus.logValues(), err)
			continue
		}
		r.markProviderHealthy(client.region)
		return resp, nil
	}
	return nil, fmt.Errorf("no healthy backends available")
}

func (r *CachingRouter) GPULabel(ctx context.Context, req *protos.GPULabelRequest) (*protos.GPULabelResponse, error) {
	for _, client := range r.getHealthyClients() {
		call := r.startBackendRPC(ctx, client, "GPULabel")
		resp, err := client.client.GPULabel(call.Context(), req)
		callStatus := call.Finish(err)
		if err != nil {
			r.markProviderUnhealthy(client.region, err)
			klog.Errorf("gpu label lookup failed for %s: %v", callStatus.logValues(), err)
			continue
		}
		r.markProviderHealthy(client.region)
		return resp, nil
	}
	return nil, fmt.Errorf("no healthy backends available")
}

func (r *CachingRouter) GetAvailableGPUTypes(ctx context.Context, req *protos.GetAvailableGPUTypesRequest) (*protos.GetAvailableGPUTypesResponse, error) {
	for _, client := range r.getHealthyClients() {
		call := r.startBackendRPC(ctx, client, "GetAvailableGPUTypes")
		resp, err := client.client.GetAvailableGPUTypes(call.Context(), req)
		callStatus := call.Finish(err)
		if err != nil {
			r.markProviderUnhealthy(client.region, err)
			klog.Errorf("available gpu types lookup failed for %s: %v", callStatus.logValues(), err)
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
