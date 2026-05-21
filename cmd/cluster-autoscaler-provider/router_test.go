package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/externalgrpc/protos"
)

type routerTestBackend struct {
	protos.UnimplementedCloudProviderServer

	mu sync.Mutex

	nodeGroups            []*protos.NodeGroup
	nodeGroupsErr         error
	nodeGroupsDelay       time.Duration
	nodeGroupsCalls       int
	nodeGroupForNodeCalls int
	refreshErr            error
	refreshCalls          int
	cleanupErr            error
	cleanupCalls          int
	lastIncreaseID        string
	lastIncreaseDelta     int32
	increaseCalls         int
	pricingNodeResp       *protos.PricingNodePriceResponse
	pricingNodeErr        error
	pricingNodeCalls      int
	gpuLabelResp          *protos.GPULabelResponse
	gpuLabelErr           error
	gpuLabelCalls         int
}

var _ protos.CloudProviderServer = (*routerTestBackend)(nil)
func (b *routerTestBackend) NodeGroups(ctx context.Context, _ *protos.NodeGroupsRequest) (*protos.NodeGroupsResponse, error) {
	b.mu.Lock()
	b.nodeGroupsCalls++
	delay := b.nodeGroupsDelay
	err := b.nodeGroupsErr
	sourceGroups := make([]*protos.NodeGroup, 0, len(b.nodeGroups))
	for _, group := range b.nodeGroups {
		sourceGroups = append(sourceGroups, proto.Clone(group).(*protos.NodeGroup))
	}
	b.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err != nil {
		return nil, err
	}

	return &protos.NodeGroupsResponse{NodeGroups: sourceGroups}, nil
}

func (b *routerTestBackend) NodeGroupForNode(_ context.Context, _ *protos.NodeGroupForNodeRequest) (*protos.NodeGroupForNodeResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nodeGroupForNodeCalls++
	return &protos.NodeGroupForNodeResponse{}, nil
}

func (b *routerTestBackend) Refresh(_ context.Context, _ *protos.RefreshRequest) (*protos.RefreshResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshCalls++
	if b.refreshErr != nil {
		return nil, b.refreshErr
	}
	return &protos.RefreshResponse{}, nil
}

func (b *routerTestBackend) Cleanup(_ context.Context, _ *protos.CleanupRequest) (*protos.CleanupResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cleanupCalls++
	if b.cleanupErr != nil {
		return nil, b.cleanupErr
	}
	return &protos.CleanupResponse{}, nil
}

func (b *routerTestBackend) PricingNodePrice(_ context.Context, _ *protos.PricingNodePriceRequest) (*protos.PricingNodePriceResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pricingNodeCalls++
	if b.pricingNodeErr != nil {
		return nil, b.pricingNodeErr
	}
	if b.pricingNodeResp != nil {
		return b.pricingNodeResp, nil
	}
	return &protos.PricingNodePriceResponse{}, nil
}

func (b *routerTestBackend) GPULabel(_ context.Context, _ *protos.GPULabelRequest) (*protos.GPULabelResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gpuLabelCalls++
	if b.gpuLabelErr != nil {
		return nil, b.gpuLabelErr
	}
	if b.gpuLabelResp != nil {
		return b.gpuLabelResp, nil
	}
	return &protos.GPULabelResponse{}, nil
}

func (b *routerTestBackend) NodeGroupIncreaseSize(_ context.Context, req *protos.NodeGroupIncreaseSizeRequest) (*protos.NodeGroupIncreaseSizeResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.increaseCalls++
	b.lastIncreaseID = req.GetId()
	b.lastIncreaseDelta = req.GetDelta()
	return &protos.NodeGroupIncreaseSizeResponse{}, nil
}

func (b *routerTestBackend) NodeGroupDeleteNodes(_ context.Context, _ *protos.NodeGroupDeleteNodesRequest) (*protos.NodeGroupDeleteNodesResponse, error) {
	return &protos.NodeGroupDeleteNodesResponse{}, nil
}

func (b *routerTestBackend) NodeGroupDecreaseTargetSize(_ context.Context, _ *protos.NodeGroupDecreaseTargetSizeRequest) (*protos.NodeGroupDecreaseTargetSizeResponse, error) {
	return &protos.NodeGroupDecreaseTargetSizeResponse{}, nil
}

func startRouterTestBackend(t *testing.T, backend protos.CloudProviderServer) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := grpc.NewServer()
	protos.RegisterCloudProviderServer(server, backend)
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Logf("backend server exited: %v", err)
		}
	}()

	t.Cleanup(func() {
		server.GracefulStop()
	})

	return listener.Addr().(*net.TCPAddr).Port
}

func startRouterTestBackendOnPort(t *testing.T, port int, backend protos.CloudProviderServer) {
	t.Helper()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen on fixed port %d: %v", port, err)
	}

	server := grpc.NewServer()
	protos.RegisterCloudProviderServer(server, backend)
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Logf("backend server exited: %v", err)
		}
	}()

	t.Cleanup(func() {
		server.GracefulStop()
	})
}

func reserveUnusedPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("close reserved listener: %v", err)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func newTestRouter(t *testing.T, backends map[string]int) *CachingRouter {
	t.Helper()

	opts := RouterOptions{
		CacheTTL:          30 * time.Second,
		BackendRPCTimeout: 5 * time.Second,
		Backends:          make([]Backend, 0, len(backends)),
	}
	regions := make([]string, 0, len(backends))
	for region := range backends {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	for _, region := range regions {
		port := backends[region]
		opts.Backends = append(opts.Backends, Backend{
			Provider: "aws",
			Region:   region,
			Address:  fmt.Sprintf("127.0.0.1:%d", port),
		})
	}

	router, err := newCachingRouter(opts)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	t.Cleanup(func() {
		if err := router.Close(context.Background()); err != nil {
			t.Fatalf("close router: %v", err)
		}
	})
	return router
}

func doRequest(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRouterReadinessAndMetricsReflectProviderHealth(t *testing.T) {
	healthyBackend := &routerTestBackend{
		nodeGroups: []*protos.NodeGroup{{Id: "asg-east", MinSize: 1, MaxSize: 3}},
	}
	healthyPort := startRouterTestBackend(t, healthyBackend)
	unhealthyPort := reserveUnusedPort(t)

	router := newTestRouter(t, map[string]int{
		"us-east-1": healthyPort,
		"us-west-2": unhealthyPort,
	})

	server := newRouterHTTPServer("127.0.0.1:0", router)

	notReady := doRequest(t, server.Handler, "/ready")
	if notReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected not ready before probing, got %d", notReady.Code)
	}

	router.probeProviders(context.Background())

	ready := doRequest(t, server.Handler, "/ready")
	if ready.Code != http.StatusOK {
		t.Fatalf("expected ready after probing healthy backend, got %d", ready.Code)
	}

	metrics := doRequest(t, server.Handler, "/metrics")
	body := metrics.Body.String()
	for _, want := range []string{
		"cluster_autoscaler_provider_router_configured_providers 2",
		"cluster_autoscaler_provider_router_healthy_providers 1",
		"cluster_autoscaler_provider_router_unhealthy_providers 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\n%s", want, body)
		}
	}
}

func TestRouterStartReprobesUntilProviderBecomesHealthy(t *testing.T) {
	t.Parallel()

	port := reserveUnusedPort(t)
	router := newTestRouter(t, map[string]int{
		"us-east-1": port,
	})
	defer func() {
		if err := router.Close(context.Background()); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	}()

	server := newRouterHTTPServer("127.0.0.1:0", router)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router.Start(ctx)

	notReady := doRequest(t, server.Handler, "/ready")
	if notReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected not ready before backend starts, got %d", notReady.Code)
	}

	backend := &routerTestBackend{
		nodeGroups: []*protos.NodeGroup{{Id: "asg-east", MinSize: 1, MaxSize: 3}},
	}
	startRouterTestBackendOnPort(t, port, backend)

	deadline := time.Now().Add(defaultProviderProbeInterval + 2*time.Second)
	for time.Now().Before(deadline) {
		ready := doRequest(t, server.Handler, "/ready")
		if ready.Code == http.StatusOK {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("expected router to become ready after backend started")
}

func TestRouterNodeGroupsTimesOutSlowBackend(t *testing.T) {
	east := &routerTestBackend{
		nodeGroups: []*protos.NodeGroup{{Id: "asg-east", MinSize: 1, MaxSize: 3}},
	}
	west := &routerTestBackend{
		nodeGroups:      []*protos.NodeGroup{{Id: "asg-west", MinSize: 2, MaxSize: 4}},
		nodeGroupsDelay: 200 * time.Millisecond,
	}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
		"us-west-2": startRouterTestBackend(t, west),
	})
	router.backendRPCTimeout = 50 * time.Millisecond

	start := time.Now()
	resp, err := router.NodeGroups(context.Background(), &protos.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("NodeGroups failed: %v", err)
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("NodeGroups exceeded expected timeout window")
	}
	if len(resp.GetNodeGroups()) != 1 {
		t.Fatalf("expected 1 node group after slow backend timeout, got %d", len(resp.GetNodeGroups()))
	}
	if resp.GetNodeGroups()[0].GetId() != "us-east-1/asg-east" {
		t.Fatalf("expected east result after west timeout, got %q", resp.GetNodeGroups()[0].GetId())
	}
}

func TestRouterNodeGroupsCachesAndRefreshInvalidates(t *testing.T) {
	east := &routerTestBackend{
		nodeGroups: []*protos.NodeGroup{{Id: "asg-east", MinSize: 1, MaxSize: 3}},
	}
	west := &routerTestBackend{
		nodeGroups: []*protos.NodeGroup{{Id: "asg-west", MinSize: 2, MaxSize: 4}},
	}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
		"us-west-2": startRouterTestBackend(t, west),
	})

	first, err := router.NodeGroups(context.Background(), &protos.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("first NodeGroups failed: %v", err)
	}
	if len(first.GetNodeGroups()) != 2 {
		t.Fatalf("expected 2 node groups, got %d", len(first.GetNodeGroups()))
	}

	east.mu.Lock()
	east.nodeGroups = []*protos.NodeGroup{{Id: "asg-east-updated", MinSize: 1, MaxSize: 5}}
	east.mu.Unlock()

	cached, err := router.NodeGroups(context.Background(), &protos.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("cached NodeGroups failed: %v", err)
	}
	if got := cached.GetNodeGroups()[0].GetId(); got != "us-east-1/asg-east" && got != "us-west-2/asg-west" {
		t.Fatalf("unexpected cached group id %q", got)
	}

	east.mu.Lock()
	east.refreshErr = nil
	east.mu.Unlock()
	west.mu.Lock()
	west.refreshErr = fmt.Errorf("west refresh unavailable")
	west.nodeGroupsErr = fmt.Errorf("west nodegroups unavailable")
	west.mu.Unlock()

	if _, err := router.Refresh(context.Background(), &protos.RefreshRequest{}); err != nil {
		t.Fatalf("refresh should succeed with one healthy provider: %v", err)
	}

	refreshed, err := router.NodeGroups(context.Background(), &protos.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("refreshed NodeGroups failed: %v", err)
	}
	if len(refreshed.GetNodeGroups()) != 1 {
		t.Fatalf("expected 1 node group after partial provider failure, got %d", len(refreshed.GetNodeGroups()))
	}
	if got := refreshed.GetNodeGroups()[0].GetId(); got != "us-east-1/asg-east-updated" {
		t.Fatalf("expected refreshed east node group, got %q", got)
	}

	east.mu.Lock()
	defer east.mu.Unlock()
	if east.nodeGroupsCalls != 2 {
		t.Fatalf("expected east NodeGroups to be called twice, got %d", east.nodeGroupsCalls)
	}
	west.mu.Lock()
	defer west.mu.Unlock()
	if west.nodeGroupsCalls != 2 {
		t.Fatalf("expected west NodeGroups to be called twice, got %d", west.nodeGroupsCalls)
	}
}

func TestRouterMutationStripsRegionPrefixAndInvalidatesCache(t *testing.T) {
	east := &routerTestBackend{
		nodeGroups: []*protos.NodeGroup{{Id: "asg-east", MinSize: 1, MaxSize: 3}},
	}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
	})

	if _, err := router.NodeGroups(context.Background(), &protos.NodeGroupsRequest{}); err != nil {
		t.Fatalf("prime NodeGroups cache: %v", err)
	}

	east.mu.Lock()
	east.nodeGroups = []*protos.NodeGroup{{Id: "asg-east-resized", MinSize: 1, MaxSize: 5}}
	east.mu.Unlock()

	if _, err := router.NodeGroupIncreaseSize(context.Background(), &protos.NodeGroupIncreaseSizeRequest{
		Id:    "us-east-1/asg-east",
		Delta: 2,
	}); err != nil {
		t.Fatalf("NodeGroupIncreaseSize failed: %v", err)
	}

	east.mu.Lock()
	if east.lastIncreaseID != "asg-east" {
		t.Fatalf("expected backend ID without region prefix, got %q", east.lastIncreaseID)
	}
	if east.lastIncreaseDelta != 2 {
		t.Fatalf("expected delta 2, got %d", east.lastIncreaseDelta)
	}
	east.mu.Unlock()

	resp, err := router.NodeGroups(context.Background(), &protos.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("NodeGroups after mutation failed: %v", err)
	}
	if len(resp.GetNodeGroups()) != 1 || resp.GetNodeGroups()[0].GetId() != "us-east-1/asg-east-resized" {
		t.Fatalf("expected invalidated cache to return resized group, got %#v", resp.GetNodeGroups())
	}
}

func TestRouterNodeGroupForNodeRejectsInvalidProviderIDWithoutFallback(t *testing.T) {
	east := &routerTestBackend{
		nodeGroups: []*protos.NodeGroup{{Id: "asg-east", MinSize: 1, MaxSize: 3}},
	}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
	})

	_, err := router.NodeGroupForNode(context.Background(), &protos.NodeGroupForNodeRequest{
		Node: &protos.ExternalGrpcNode{
			Name:       "node-1",
			ProviderID: "not-an-aws-provider-id",
		},
	})
	if err == nil {
		t.Fatal("expected invalid provider ID to fail")
	}

	east.mu.Lock()
	defer east.mu.Unlock()
	if east.nodeGroupForNodeCalls != 0 {
		t.Fatalf("expected no backend fallback call, got %d", east.nodeGroupForNodeCalls)
	}
}

func TestRouterRefreshFailsWhenNoProvidersRefreshSuccessfully(t *testing.T) {
	east := &routerTestBackend{refreshErr: fmt.Errorf("east refresh failed")}
	west := &routerTestBackend{refreshErr: fmt.Errorf("west refresh failed")}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
		"us-west-2": startRouterTestBackend(t, west),
	})

	if _, err := router.Refresh(context.Background(), &protos.RefreshRequest{}); err == nil {
		t.Fatal("expected refresh to fail when all providers fail")
	}
}

func TestRouterCleanupDoesNotForwardToProviders(t *testing.T) {
	east := &routerTestBackend{}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
	})

	if _, err := router.Cleanup(context.Background(), &protos.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}

	east.mu.Lock()
	defer east.mu.Unlock()
	if east.cleanupCalls != 0 {
		t.Fatalf("expected provider cleanup calls = 0, got %d", east.cleanupCalls)
	}
}

func TestRouterCloseCleansUpProviders(t *testing.T) {
	east := &routerTestBackend{}
	west := &routerTestBackend{}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
		"us-west-2": startRouterTestBackend(t, west),
	})

	if err := router.Close(context.Background()); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	east.mu.Lock()
	defer east.mu.Unlock()
	if east.cleanupCalls != 1 {
		t.Fatalf("expected east provider cleanup calls = 1, got %d", east.cleanupCalls)
	}

	west.mu.Lock()
	defer west.mu.Unlock()
	if west.cleanupCalls != 1 {
		t.Fatalf("expected west provider cleanup calls = 1, got %d", west.cleanupCalls)
	}
}

func TestRouterPricingNodePriceUsesHealthyProviderDeterministically(t *testing.T) {
	east := &routerTestBackend{
		pricingNodeResp: &protos.PricingNodePriceResponse{Price: 1.25},
	}
	west := &routerTestBackend{
		pricingNodeResp: &protos.PricingNodePriceResponse{Price: 9.99},
	}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
		"us-west-2": startRouterTestBackend(t, west),
	})

	router.markProviderHealthy("us-east-1")
	router.markProviderUnhealthy("us-west-2", fmt.Errorf("unhealthy"))

	resp, err := router.PricingNodePrice(context.Background(), &protos.PricingNodePriceRequest{})
	if err != nil {
		t.Fatalf("PricingNodePrice failed: %v", err)
	}
	if resp.GetPrice() != 1.25 {
		t.Fatalf("expected price from healthy east provider, got %v", resp.GetPrice())
	}

	east.mu.Lock()
	defer east.mu.Unlock()
	if east.pricingNodeCalls != 1 {
		t.Fatalf("expected east pricing calls = 1, got %d", east.pricingNodeCalls)
	}
	west.mu.Lock()
	defer west.mu.Unlock()
	if west.pricingNodeCalls != 0 {
		t.Fatalf("expected west pricing calls = 0, got %d", west.pricingNodeCalls)
	}
}

func TestRouterGPULabelFailsOverToNextHealthyProvider(t *testing.T) {
	east := &routerTestBackend{
		gpuLabelErr: fmt.Errorf("east GPU failure"),
	}
	west := &routerTestBackend{
		gpuLabelResp: &protos.GPULabelResponse{Label: "nvidia.com/gpu"},
	}

	router := newTestRouter(t, map[string]int{
		"us-east-1": startRouterTestBackend(t, east),
		"us-west-2": startRouterTestBackend(t, west),
	})

	router.markProviderHealthy("us-east-1")
	router.markProviderHealthy("us-west-2")

	resp, err := router.GPULabel(context.Background(), &protos.GPULabelRequest{})
	if err != nil {
		t.Fatalf("GPULabel failed: %v", err)
	}
	if resp.GetLabel() != "nvidia.com/gpu" {
		t.Fatalf("expected west GPU label response, got %q", resp.GetLabel())
	}
	if got := router.HealthyProviderCount(); got != 1 {
		t.Fatalf("expected one healthy provider after failover, got %d", got)
	}

	east.mu.Lock()
	defer east.mu.Unlock()
	if east.gpuLabelCalls != 1 {
		t.Fatalf("expected east GPU label calls = 1, got %d", east.gpuLabelCalls)
	}
	west.mu.Lock()
	defer west.mu.Unlock()
	if west.gpuLabelCalls != 1 {
		t.Fatalf("expected west GPU label calls = 1, got %d", west.gpuLabelCalls)
	}
}
