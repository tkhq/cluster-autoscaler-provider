//go:build integration

// Integration tests for the dry-run mode of region-cluster-autoscaler.
//
// These tests verify that the dry-run gRPC wrapper correctly intercepts
// mutating CloudProvider RPCs (NodeGroupIncreaseSize, NodeGroupDeleteNodes,
// NodeGroupDecreaseTargetSize), logs what would have been done, and returns
// success — without forwarding the call to the underlying provider.
//
// Each test starts a real cluster-autoscaler v1.35.0 binary as a subprocess
// connected to an in-process gRPC server. The gRPC server registers a
// dryRunWrapper around a stubCloudProvider that returns hardcoded but
// realistic responses for all read-only RPCs. An envtest instance provides a
// lightweight Kubernetes API server so the cluster-autoscaler can discover
// nodes, pods, and make scheduling decisions.
//
// TestDryRunScaleUp:
//   - Seeds the cluster with a fully-utilized node and a pending pod.
//   - Waits for the cluster-autoscaler to decide a scale-up is needed and
//     send a NodeGroupIncreaseSize RPC.
//   - Asserts the dry-run wrapper intercepted the call (stub was NOT called)
//     and that "DRY-RUN: would increase size of node group" appears in klog.
//
// TestDryRunScaleDown:
//   - Seeds the cluster with two nodes: one busy, one idle.
//   - Waits for the cluster-autoscaler to decide scale-down is needed and
//     send a NodeGroupDeleteNodes or NodeGroupDecreaseTargetSize RPC.
//   - Asserts the dry-run wrapper intercepted the call (stub was NOT called)
//     and that "DRY-RUN: would delete" or "DRY-RUN: would decrease target
//     size" appears in klog.
//
// Requirements:
//   - Docker (to extract the cluster-autoscaler binary from the upstream
//     container image, unless CA_EXTERNALGRPC_BINARY_PATH or
//     CA_EXTERNALGRPC_BINARY_URL is set).
//   - kubebuilder envtest binaries (etcd, kube-apiserver). Install with:
//       go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//       export KUBEBUILDER_ASSETS="$(setup-envtest use 1.35.0 -p path)"
//   - The cluster-autoscaler binary is cached at
//     ~/.cache/region-cluster-autoscaler/bin/ after first download.
//
// Environment variables:
//   RUN_CA_EXTERNALGRPC_INTEGRATION_TEST=1   Required to run these tests.
//   KUBEBUILDER_ASSETS=<path>                Required. Path to envtest binaries.
//   CA_EXTERNALGRPC_BINARY_PATH=<path>       Optional. Explicit path to a
//                                            pre-built cluster-autoscaler binary.
//   CA_EXTERNALGRPC_BINARY_URL=<url>         Optional. URL to download the
//                                            cluster-autoscaler binary from.
//
// Example:
//   export KUBEBUILDER_ASSETS="$(setup-envtest use 1.35.0 -p path)"
//   RUN_CA_EXTERNALGRPC_INTEGRATION_TEST=1 \
//     go test -tags integration -v -timeout 5m ./cmd/region-cluster-autoscaler/

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/externalgrpc/protos"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	klog "k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	caIntegrationEnvVar    = "RUN_CA_EXTERNALGRPC_INTEGRATION_TEST"
	caBinaryPathEnvVar     = "CA_EXTERNALGRPC_BINARY_PATH"
	caBinaryDownloadURLVar = "CA_EXTERNALGRPC_BINARY_URL"
	defaultCABinaryVersion = "1.35.0"
)

// stubCloudProvider implements protos.CloudProviderServer with hardcoded
// responses for read-only methods. The dry-run wrapper intercepts mutating
// calls before they reach this stub.
type stubCloudProvider struct {
	protos.UnimplementedCloudProviderServer
	nodeGroupID string
	providerID  string // e.g. "aws:///us-east-1a/i-123"
	targetSize  int32
	minSize     int32
	maxSize     int32
	templateNode []byte // proto-serialized v1.Node

	mu                  sync.Mutex
	increaseSizeCalls   int
	deleteNodesCalls    int
	decreaseTargetCalls int
}

func (s *stubCloudProvider) NodeGroups(_ context.Context, _ *protos.NodeGroupsRequest) (*protos.NodeGroupsResponse, error) {
	return &protos.NodeGroupsResponse{
		NodeGroups: []*protos.NodeGroup{
			{
				Id:      s.nodeGroupID,
				MinSize: s.minSize,
				MaxSize: s.maxSize,
				Debug:   s.nodeGroupID,
			},
		},
	}, nil
}

func (s *stubCloudProvider) NodeGroupForNode(_ context.Context, req *protos.NodeGroupForNodeRequest) (*protos.NodeGroupForNodeResponse, error) {
	if req.GetNode().GetProviderID() == s.providerID {
		return &protos.NodeGroupForNodeResponse{
			NodeGroup: &protos.NodeGroup{
				Id:      s.nodeGroupID,
				MinSize: s.minSize,
				MaxSize: s.maxSize,
			},
		}, nil
	}
	return &protos.NodeGroupForNodeResponse{}, nil
}

func (s *stubCloudProvider) NodeGroupTargetSize(_ context.Context, _ *protos.NodeGroupTargetSizeRequest) (*protos.NodeGroupTargetSizeResponse, error) {
	return &protos.NodeGroupTargetSizeResponse{TargetSize: s.targetSize}, nil
}

func (s *stubCloudProvider) NodeGroupNodes(_ context.Context, _ *protos.NodeGroupNodesRequest) (*protos.NodeGroupNodesResponse, error) {
	return &protos.NodeGroupNodesResponse{
		Instances: []*protos.Instance{
			{
				Id: s.providerID,
				Status: &protos.InstanceStatus{
					InstanceState: protos.InstanceStatus_instanceRunning,
				},
			},
		},
	}, nil
}

func (s *stubCloudProvider) NodeGroupTemplateNodeInfo(_ context.Context, _ *protos.NodeGroupTemplateNodeInfoRequest) (*protos.NodeGroupTemplateNodeInfoResponse, error) {
	return &protos.NodeGroupTemplateNodeInfoResponse{
		NodeBytes: s.templateNode,
	}, nil
}

func (s *stubCloudProvider) Refresh(_ context.Context, _ *protos.RefreshRequest) (*protos.RefreshResponse, error) {
	return &protos.RefreshResponse{}, nil
}

func (s *stubCloudProvider) Cleanup(_ context.Context, _ *protos.CleanupRequest) (*protos.CleanupResponse, error) {
	return &protos.CleanupResponse{}, nil
}

func (s *stubCloudProvider) GPULabel(_ context.Context, _ *protos.GPULabelRequest) (*protos.GPULabelResponse, error) {
	return &protos.GPULabelResponse{}, nil
}

func (s *stubCloudProvider) GetAvailableGPUTypes(_ context.Context, _ *protos.GetAvailableGPUTypesRequest) (*protos.GetAvailableGPUTypesResponse, error) {
	return &protos.GetAvailableGPUTypesResponse{}, nil
}

func (s *stubCloudProvider) PricingNodePrice(_ context.Context, _ *protos.PricingNodePriceRequest) (*protos.PricingNodePriceResponse, error) {
	return &protos.PricingNodePriceResponse{}, nil
}

func (s *stubCloudProvider) PricingPodPrice(_ context.Context, _ *protos.PricingPodPriceRequest) (*protos.PricingPodPriceResponse, error) {
	return &protos.PricingPodPriceResponse{}, nil
}

func (s *stubCloudProvider) NodeGroupGetOptions(_ context.Context, _ *protos.NodeGroupAutoscalingOptionsRequest) (*protos.NodeGroupAutoscalingOptionsResponse, error) {
	return &protos.NodeGroupAutoscalingOptionsResponse{}, nil
}

// Mutating methods: track calls. These should never be reached when the
// dry-run wrapper is in place.

func (s *stubCloudProvider) NodeGroupIncreaseSize(_ context.Context, _ *protos.NodeGroupIncreaseSizeRequest) (*protos.NodeGroupIncreaseSizeResponse, error) {
	s.mu.Lock()
	s.increaseSizeCalls++
	s.mu.Unlock()
	return &protos.NodeGroupIncreaseSizeResponse{}, nil
}

func (s *stubCloudProvider) NodeGroupDeleteNodes(_ context.Context, _ *protos.NodeGroupDeleteNodesRequest) (*protos.NodeGroupDeleteNodesResponse, error) {
	s.mu.Lock()
	s.deleteNodesCalls++
	s.mu.Unlock()
	return &protos.NodeGroupDeleteNodesResponse{}, nil
}

func (s *stubCloudProvider) NodeGroupDecreaseTargetSize(_ context.Context, _ *protos.NodeGroupDecreaseTargetSizeRequest) (*protos.NodeGroupDecreaseTargetSizeResponse, error) {
	s.mu.Lock()
	s.decreaseTargetCalls++
	s.mu.Unlock()
	return &protos.NodeGroupDecreaseTargetSizeResponse{}, nil
}

func (s *stubCloudProvider) getMutatingCallCounts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.increaseSizeCalls, s.deleteNodesCalls, s.decreaseTargetCalls
}

// methodCallTracker counts gRPC calls via a server interceptor.
type methodCallTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func newMethodCallTracker() *methodCallTracker {
	return &methodCallTracker{counts: map[string]int{}}
}

func (m *methodCallTracker) Interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		m.mu.Lock()
		m.counts[info.FullMethod]++
		m.mu.Unlock()
		return resp, err
	}
}

func (m *methodCallTracker) Count(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[method]
}

// threadSafeWriter wraps a bytes.Buffer with a mutex for safe concurrent writes.
type threadSafeWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *threadSafeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *threadSafeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// buildTemplateNode creates a proto-serialized v1.Node for the stub's
// NodeGroupTemplateNodeInfo response.
func buildTemplateNode() ([]byte, error) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "template-node",
			Labels: map[string]string{
				"kubernetes.io/os":   "linux",
				"kubernetes.io/arch": "amd64",
			},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
	return node.Marshal()
}

func TestDryRunScaleUp(t *testing.T) {
	if os.Getenv(caIntegrationEnvVar) != "1" {
		t.Skipf("set %s=1 to run integration test", caIntegrationEnvVar)
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS must be set for envtest")
	}

	// Capture klog output to verify dry-run log lines.
	// klog.LogToStderr(false) is required because klog's default toStderr=true
	// causes output to go directly to os.Stderr, bypassing the writer set by
	// SetOutput.
	logBuf := &threadSafeWriter{}
	klog.LogToStderr(false)
	klog.SetOutput(logBuf)
	t.Cleanup(func() { klog.LogToStderr(true) })

	// Start envtest (lightweight k8s API server).
	testEnv := &envtest.Environment{}
	restCfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if stopErr := testEnv.Stop(); stopErr != nil {
			t.Logf("stop envtest: %v", stopErr)
		}
	})

	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("create kube client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	if err := seedClusterStateForScaleUp(ctx, kubeClient); err != nil {
		t.Fatalf("seed cluster state: %v", err)
	}

	templateNodeBytes, err := buildTemplateNode()
	if err != nil {
		t.Fatalf("build template node: %v", err)
	}

	stub := &stubCloudProvider{
		nodeGroupID:  "asg-a",
		providerID:   "aws:///us-east-1a/i-123",
		targetSize:   1,
		minSize:      1,
		maxSize:      5,
		templateNode: templateNodeBytes,
	}

	methods := newMethodCallTracker()
	wrapper := newDryRunWrapper(stub)

	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(methods.Interceptor()))
	protos.RegisterCloudProviderServer(grpcServer, wrapper)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.GracefulStop)

	grpcAddr := lis.Addr().String()

	kubeconfigPath, err := writeTestKubeconfigFile(t.TempDir(), restCfg)
	if err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	cloudConfigPath := filepath.Join(t.TempDir(), "ca-cloud-config.yaml")
	cloudConfig := fmt.Sprintf("address: %q\ngrpc_timeout: 3s\n", grpcAddr)
	if err := os.WriteFile(cloudConfigPath, []byte(cloudConfig), 0o600); err != nil {
		t.Fatalf("write cloud config: %v", err)
	}

	caBinaryPath := resolveClusterAutoscalerBinary(t)

	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	t.Cleanup(cmdCancel)

	cmd := exec.CommandContext(cmdCtx, caBinaryPath,
		"--cloud-provider=externalgrpc",
		"--cloud-config="+cloudConfigPath,
		"--kubeconfig="+kubeconfigPath,
		"--scan-interval=1s",
		"--scale-down-enabled=false",
		"--skip-nodes-with-system-pods=false",
		"--v=5",
	)
	var caLogs bytes.Buffer
	cmd.Stdout = &caLogs
	cmd.Stderr = &caLogs

	if err := cmd.Start(); err != nil {
		t.Fatalf("start cluster-autoscaler: %v", err)
	}
	t.Cleanup(func() {
		cmdCancel()
		_ = cmd.Wait()
	})

	// Wait for cluster-autoscaler to send a NodeGroupIncreaseSize RPC.
	err = waitForConditionWithProgress(90*time.Second, 500*time.Millisecond, func() (bool, string, error) {
		nodeGroupsCount := methods.Count(protos.CloudProvider_NodeGroups_FullMethodName)
		refreshCount := methods.Count(protos.CloudProvider_Refresh_FullMethodName)
		increaseSizeCount := methods.Count(protos.CloudProvider_NodeGroupIncreaseSize_FullMethodName)

		progress := fmt.Sprintf(
			"NodeGroups=%d Refresh=%d IncreaseSize=%d",
			nodeGroupsCount, refreshCount, increaseSizeCount,
		)

		done := nodeGroupsCount > 0 && refreshCount > 0 && increaseSizeCount > 0
		return done, progress, nil
	}, func(progress string) {
		t.Logf("scale-up progress: %s", progress)
	})
	if err != nil {
		t.Fatalf("waiting for scale-up RPC: %v\ncluster-autoscaler logs:\n%s", err, caLogs.String())
	}

	// Verify the dry-run wrapper intercepted the call (stub was NOT called).
	incr, del, decr := stub.getMutatingCallCounts()
	if incr != 0 || del != 0 || decr != 0 {
		t.Fatalf("dry-run wrapper did not intercept mutating calls: increase=%d delete=%d decrease=%d", incr, del, decr)
	}

	// Verify dry-run log line was emitted.
	klog.Flush()
	logs := logBuf.String()
	if !strings.Contains(logs, "DRY-RUN: would increase size of node group") {
		t.Fatalf("expected DRY-RUN increase size log line\nklog output:\n%s\ncluster-autoscaler logs:\n%s", logs, caLogs.String())
	}
}

func TestDryRunScaleDown(t *testing.T) {
	if os.Getenv(caIntegrationEnvVar) != "1" {
		t.Skipf("set %s=1 to run integration test", caIntegrationEnvVar)
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS must be set for envtest")
	}

	// Capture klog output to verify dry-run log lines.
	// klog.LogToStderr(false) is required because klog's default toStderr=true
	// causes output to go directly to os.Stderr, bypassing the writer set by
	// SetOutput.
	logBuf := &threadSafeWriter{}
	klog.LogToStderr(false)
	klog.SetOutput(logBuf)
	t.Cleanup(func() { klog.LogToStderr(true) })

	// Start envtest.
	testEnv := &envtest.Environment{}
	restCfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if stopErr := testEnv.Stop(); stopErr != nil {
			t.Logf("stop envtest: %v", stopErr)
		}
	})

	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("create kube client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	if err := seedClusterStateForScaleDown(ctx, kubeClient); err != nil {
		t.Fatalf("seed cluster state: %v", err)
	}

	templateNodeBytes, err := buildTemplateNode()
	if err != nil {
		t.Fatalf("build template node: %v", err)
	}

	// Two-node stub: one busy (east), one idle (west) — both in the same
	// node group. The idle node should be selected for scale-down.
	stub := &stubCloudProvider{
		nodeGroupID:  "asg-a",
		providerID:   "aws:///us-east-1a/i-east-1",
		targetSize:   2,
		minSize:      0,
		maxSize:      5,
		templateNode: templateNodeBytes,
	}

	// Override NodeGroupNodes to return both instances.
	twoNodeStub := &twoNodeStubCloudProvider{
		stubCloudProvider: stub,
		instances: []*protos.Instance{
			{
				Id:     "aws:///us-east-1a/i-east-1",
				Status: &protos.InstanceStatus{InstanceState: protos.InstanceStatus_instanceRunning},
			},
			{
				Id:     "aws:///us-east-1a/i-west-1",
				Status: &protos.InstanceStatus{InstanceState: protos.InstanceStatus_instanceRunning},
			},
		},
	}

	methods := newMethodCallTracker()
	wrapper := newDryRunWrapper(twoNodeStub)

	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(methods.Interceptor()))
	protos.RegisterCloudProviderServer(grpcServer, wrapper)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.GracefulStop)

	grpcAddr := lis.Addr().String()

	kubeconfigPath, err := writeTestKubeconfigFile(t.TempDir(), restCfg)
	if err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	cloudConfigPath := filepath.Join(t.TempDir(), "ca-cloud-config.yaml")
	cloudConfig := fmt.Sprintf("address: %q\ngrpc_timeout: 3s\n", grpcAddr)
	if err := os.WriteFile(cloudConfigPath, []byte(cloudConfig), 0o600); err != nil {
		t.Fatalf("write cloud config: %v", err)
	}

	caBinaryPath := resolveClusterAutoscalerBinary(t)

	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	t.Cleanup(cmdCancel)

	cmd := exec.CommandContext(cmdCtx, caBinaryPath,
		"--cloud-provider=externalgrpc",
		"--cloud-config="+cloudConfigPath,
		"--kubeconfig="+kubeconfigPath,
		"--scan-interval=1s",
		"--scale-down-enabled=true",
		"--scale-down-unneeded-time=10s",
		"--scale-down-unready-time=10s",
		"--scale-down-delay-after-add=0s",
		"--scale-down-delay-after-delete=0s",
		"--scale-down-delay-after-failure=0s",
		"--skip-nodes-with-system-pods=false",
		"--skip-nodes-with-local-storage=false",
		"--v=5",
	)
	var caLogs bytes.Buffer
	cmd.Stdout = &caLogs
	cmd.Stderr = &caLogs

	if err := cmd.Start(); err != nil {
		t.Fatalf("start cluster-autoscaler: %v", err)
	}
	t.Cleanup(func() {
		cmdCancel()
		_ = cmd.Wait()
	})

	// Wait for cluster-autoscaler to send DeleteNodes or DecreaseTargetSize.
	err = waitForConditionWithProgress(120*time.Second, 500*time.Millisecond, func() (bool, string, error) {
		deleteNodesCount := methods.Count(protos.CloudProvider_NodeGroupDeleteNodes_FullMethodName)
		decreaseTargetCount := methods.Count(protos.CloudProvider_NodeGroupDecreaseTargetSize_FullMethodName)

		progress := fmt.Sprintf(
			"DeleteNodes=%d DecreaseTargetSize=%d",
			deleteNodesCount, decreaseTargetCount,
		)

		done := deleteNodesCount > 0 || decreaseTargetCount > 0
		return done, progress, nil
	}, func(progress string) {
		t.Logf("scale-down progress: %s", progress)
	})
	if err != nil {
		t.Fatalf("waiting for scale-down RPC: %v\ncluster-autoscaler logs:\n%s", err, caLogs.String())
	}

	// Verify the dry-run wrapper intercepted the calls.
	incr, del, decr := stub.getMutatingCallCounts()
	if incr != 0 || del != 0 || decr != 0 {
		t.Fatalf("dry-run wrapper did not intercept mutating calls: increase=%d delete=%d decrease=%d", incr, del, decr)
	}

	// Verify at least one dry-run log line for scale-down.
	klog.Flush()
	logs := logBuf.String()
	hasDeleteLog := strings.Contains(logs, "DRY-RUN: would delete")
	hasDecreaseLog := strings.Contains(logs, "DRY-RUN: would decrease target size")
	if !hasDeleteLog && !hasDecreaseLog {
		t.Fatalf("expected DRY-RUN scale-down log line\nklog output:\n%s\ncluster-autoscaler logs:\n%s", logs, caLogs.String())
	}
}

// twoNodeStubCloudProvider extends stubCloudProvider to return two instances
// and resolve both nodes to the same node group.
type twoNodeStubCloudProvider struct {
	*stubCloudProvider
	instances []*protos.Instance
}

func (s *twoNodeStubCloudProvider) NodeGroupForNode(_ context.Context, req *protos.NodeGroupForNodeRequest) (*protos.NodeGroupForNodeResponse, error) {
	for _, inst := range s.instances {
		if req.GetNode().GetProviderID() == inst.GetId() {
			return &protos.NodeGroupForNodeResponse{
				NodeGroup: &protos.NodeGroup{
					Id:      s.nodeGroupID,
					MinSize: s.minSize,
					MaxSize: s.maxSize,
				},
			}, nil
		}
	}
	return &protos.NodeGroupForNodeResponse{}, nil
}

func (s *twoNodeStubCloudProvider) NodeGroupNodes(_ context.Context, _ *protos.NodeGroupNodesRequest) (*protos.NodeGroupNodesResponse, error) {
	return &protos.NodeGroupNodesResponse{Instances: s.instances}, nil
}

// --- Cluster state seeding helpers ---

func seedClusterStateForScaleUp(ctx context.Context, kubeClient kubernetes.Interface) error {
	if err := createTestReadyNode(ctx, kubeClient, "node-a", "aws:///us-east-1a/i-123", map[string]string{
		"kubernetes.io/os":   "linux",
		"kubernetes.io/arch": "amd64",
	}); err != nil {
		return err
	}

	// Running pod consuming all CPU on node-a.
	workloadPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workload-on-node-a",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{
				{
					Name:  "busy",
					Image: "busybox",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
					},
				},
			},
		},
	}
	createdWorkload, err := kubeClient.CoreV1().Pods("default").Create(ctx, workloadPod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create workload pod: %w", err)
	}
	createdWorkload.Status.Phase = corev1.PodRunning
	if _, err := kubeClient.CoreV1().Pods("default").UpdateStatus(ctx, createdWorkload, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update workload pod status: %w", err)
	}

	// Pending pod that cannot be scheduled (needs 80m CPU but node is full).
	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "pending",
					Image: "busybox",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("80m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
					},
				},
			},
		},
	}
	createdPending, err := kubeClient.CoreV1().Pods("default").Create(ctx, pendingPod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create pending pod: %w", err)
	}
	createdPending.Status.Phase = corev1.PodPending
	createdPending.Status.Conditions = []corev1.PodCondition{
		{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			Message:            "0/1 nodes are available: 1 Insufficient cpu.",
			LastTransitionTime: metav1.Now(),
		},
	}
	if _, err := kubeClient.CoreV1().Pods("default").UpdateStatus(ctx, createdPending, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update pending pod status: %w", err)
	}

	return nil
}

func seedClusterStateForScaleDown(ctx context.Context, kubeClient kubernetes.Interface) error {
	// Busy node: running a workload pod.
	if err := createTestReadyNode(ctx, kubeClient, "node-east", "aws:///us-east-1a/i-east-1", map[string]string{
		"kubernetes.io/os":   "linux",
		"kubernetes.io/arch": "amd64",
	}); err != nil {
		return err
	}

	// Idle node: no workload.
	if err := createTestReadyNode(ctx, kubeClient, "node-west", "aws:///us-east-1a/i-west-1", map[string]string{
		"kubernetes.io/os":   "linux",
		"kubernetes.io/arch": "amd64",
	}); err != nil {
		return err
	}

	busyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "busy-east-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "busy"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-east",
			Containers: []corev1.Container{
				{
					Name:  "busy",
					Image: "busybox",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("900m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
	}
	createdBusy, err := kubeClient.CoreV1().Pods("default").Create(ctx, busyPod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create busy pod: %w", err)
	}
	createdBusy.Status.Phase = corev1.PodRunning
	if _, err := kubeClient.CoreV1().Pods("default").UpdateStatus(ctx, createdBusy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update busy pod status: %w", err)
	}

	return nil
}

func createTestReadyNode(ctx context.Context, kubeClient kubernetes.Interface, name, providerID string, labels map[string]string) error {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{
			ProviderID: providerID,
		},
	}

	createdNode, err := kubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create node %q: %w", name, err)
	}
	createdNode.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1000m"),
		corev1.ResourceMemory: resource.MustParse("1024Mi"),
		corev1.ResourcePods:   resource.MustParse("10"),
	}
	createdNode.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1000m"),
		corev1.ResourceMemory: resource.MustParse("1024Mi"),
		corev1.ResourcePods:   resource.MustParse("10"),
	}
	createdNode.Status.Conditions = []corev1.NodeCondition{
		{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		},
	}
	finalNode, err := kubeClient.CoreV1().Nodes().UpdateStatus(ctx, createdNode, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update node status %q: %w", name, err)
	}

	// Remove not-ready taint that envtest may add.
	nodeWithSpec, err := kubeClient.CoreV1().Nodes().Get(ctx, finalNode.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %q: %w", name, err)
	}
	filteredTaints := make([]corev1.Taint, 0, len(nodeWithSpec.Spec.Taints))
	for _, taint := range nodeWithSpec.Spec.Taints {
		if taint.Key == corev1.TaintNodeNotReady {
			continue
		}
		filteredTaints = append(filteredTaints, taint)
	}
	nodeWithSpec.Spec.Taints = filteredTaints
	if _, err := kubeClient.CoreV1().Nodes().Update(ctx, nodeWithSpec, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update node taints %q: %w", name, err)
	}

	return nil
}

// --- Binary resolution (same approach as reference test) ---

func resolveClusterAutoscalerBinary(t *testing.T) string {
	t.Helper()

	if explicitPath := os.Getenv(caBinaryPathEnvVar); explicitPath != "" {
		info, err := os.Stat(explicitPath)
		if err != nil {
			t.Fatalf("%s is set but path is invalid: %v", caBinaryPathEnvVar, err)
		}
		if info.IsDir() {
			t.Fatalf("%s must point to a binary, got directory: %s", caBinaryPathEnvVar, explicitPath)
		}
		return explicitPath
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("determine user cache dir: %v", err)
	}
	binDir := filepath.Join(cacheDir, "region-cluster-autoscaler", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create binary cache dir: %v", err)
	}
	binFilename := fmt.Sprintf("cluster-autoscaler-%s-%s-%s", defaultCABinaryVersion, runtime.GOOS, runtime.GOARCH)
	binPath := filepath.Join(binDir, binFilename)

	info, err := os.Stat(binPath)
	if err == nil && !info.IsDir() && info.Size() > 0 {
		return binPath
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat cached binary: %v", err)
	}

	downloadURL := os.Getenv(caBinaryDownloadURLVar)
	if downloadURL != "" {
		downloadCaBinary(t, downloadURL, binPath)
		return binPath
	}

	imageName := fmt.Sprintf("registry.k8s.io/autoscaling/cluster-autoscaler-%s:v%s", runtime.GOARCH, defaultCABinaryVersion)
	t.Logf("Extracting cluster-autoscaler binary from Docker image %s", imageName)

	pullCmd := exec.Command("docker", "pull", imageName)
	if out, err := pullCmd.CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v\n%s", imageName, err, string(out))
	}

	createCmd := exec.Command("docker", "create", imageName)
	out, err := createCmd.Output()
	if err != nil {
		t.Fatalf("docker create %s: %v", imageName, err)
	}
	containerID := strings.TrimSpace(string(out))
	defer func() {
		_ = exec.Command("docker", "rm", containerID).Run()
	}()

	tmpPath := binPath + ".tmp"
	cpCmd := exec.Command("docker", "cp", containerID+":/cluster-autoscaler", tmpPath)
	if out, err := cpCmd.CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, string(out))
	}

	if err := os.Chmod(tmpPath, 0755); err != nil {
		t.Fatalf("chmod binary: %v", err)
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		t.Fatalf("install binary: %v", err)
	}

	return binPath
}

func downloadCaBinary(t *testing.T, downloadURL, binPath string) {
	t.Helper()

	resp, err := http.Get(downloadURL)
	if err != nil {
		t.Fatalf("download cluster-autoscaler binary: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("download cluster-autoscaler binary: status=%d body=%q", resp.StatusCode, string(body))
	}

	tmpPath := binPath + ".tmp"
	outFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("create temp binary file: %v", err)
	}
	if _, err := io.Copy(outFile, resp.Body); err != nil {
		_ = outFile.Close()
		t.Fatalf("write binary file: %v", err)
	}
	if err := outFile.Close(); err != nil {
		t.Fatalf("close binary file: %v", err)
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		t.Fatalf("install binary: %v", err)
	}
}

// --- Kubeconfig helper ---

func writeTestKubeconfigFile(dir string, restCfg *rest.Config) (string, error) {
	kubeCfg := clientcmdapi.NewConfig()
	kubeCfg.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   restCfg.Host,
		CertificateAuthorityData: restCfg.CAData,
	}
	kubeCfg.AuthInfos["envtest-admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: restCfg.CertData,
		ClientKeyData:         restCfg.KeyData,
		Token:                 restCfg.BearerToken,
	}
	kubeCfg.Contexts["envtest-context"] = &clientcmdapi.Context{
		Cluster:  "envtest",
		AuthInfo: "envtest-admin",
	}
	kubeCfg.CurrentContext = "envtest-context"

	outPath := filepath.Join(dir, "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeCfg, outPath); err != nil {
		return "", err
	}
	return outPath, nil
}

// --- Polling helper ---

func waitForConditionWithProgress(
	timeout time.Duration,
	pollInterval time.Duration,
	cond func() (bool, string, error),
	onProgress func(string),
) error {
	deadline := time.Now().Add(timeout)
	lastProgress := ""
	lastReportedAt := time.Time{}

	for time.Now().Before(deadline) {
		ok, progress, err := cond()
		if err != nil {
			return err
		}

		now := time.Now()
		if onProgress != nil && (progress != lastProgress || lastReportedAt.IsZero() || now.Sub(lastReportedAt) >= 5*time.Second) {
			onProgress(progress)
			lastReportedAt = now
		}
		lastProgress = progress

		if ok {
			return nil
		}
		time.Sleep(pollInterval)
	}

	if lastProgress == "" {
		return fmt.Errorf("timeout after %s", timeout)
	}
	return fmt.Errorf("timeout after %s (last progress: %s)", timeout, lastProgress)
}
