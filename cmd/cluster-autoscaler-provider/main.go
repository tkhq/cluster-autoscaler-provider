package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	upstreamaws "k8s.io/autoscaler/cluster-autoscaler/cloudprovider/aws"
	upstreamwrapper "k8s.io/autoscaler/cluster-autoscaler/cloudprovider/externalgrpc/examples/external-grpc-cloud-provider-service/wrapper"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/externalgrpc/protos"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	coreoptions "k8s.io/autoscaler/cluster-autoscaler/core/options"
	kube_flag "k8s.io/component-base/cli/flag"
	klog "k8s.io/klog/v2"
)

var (
	mode        = flag.String("mode", stringEnvDefault("CLUSTER_AUTOSCALER_PROVIDER_MODE", "provider"), "Execution mode: 'router' or 'provider'.")
	configPath  = flag.String("config", stringEnvDefault("CLUSTER_AUTOSCALER_PROVIDER_CONFIG", ""), "The path to the shared runtime configuration file.")
	grpcAddress = flag.String("grpc-address", stringEnvDefault("CLUSTER_AUTOSCALER_PROVIDER_GRPC_ADDRESS", ":8086"), "The address to expose the gRPC service.")
	region      = flag.String("region", firstStringEnvDefault([]string{"CLUSTER_AUTOSCALER_PROVIDER_REGION", "AWS_REGION"}, ""), "The AWS region this instance is responsible for (only used in 'provider' mode).")

	routerHTTPAddress       = flag.String("http-address", stringEnvDefault("CLUSTER_AUTOSCALER_PROVIDER_HTTP_ADDRESS", ":8080"), "The address to expose the router HTTP health and metrics service.")
	routerCacheTTL          = flag.Duration("cache-ttl", durationEnvDefault("CLUSTER_AUTOSCALER_PROVIDER_CACHE_TTL", defaultCacheTTL), "How long router NodeGroups results stay cached.")
	routerBackendRPCTimeout = flag.Duration("backend-rpc-timeout", durationEnvDefault("CLUSTER_AUTOSCALER_PROVIDER_BACKEND_RPC_TIMEOUT", defaultBackendRPCTimeout), "Timeout for router RPCs to provider backends.")
	keyCert                 = flag.String("key-cert", firstStringEnvDefault([]string{"CLUSTER_AUTOSCALER_PROVIDER_KEY_CERT", "KEY_CERT"}, ""), "The path to the certificate key file. Empty string for insecure communication.")
	cert                    = flag.String("cert", firstStringEnvDefault([]string{"CLUSTER_AUTOSCALER_PROVIDER_CERT", "CERT"}, ""), "The path to the certificate file. Empty string for insecure communication.")
	cacert                  = flag.String("ca-cert", firstStringEnvDefault([]string{"CLUSTER_AUTOSCALER_PROVIDER_CA_CERT", "CA_CERT"}, ""), "The path to the ca certificate file. Empty string for insecure communication.")

	cloudConfig = flag.String("cloud-config", firstStringEnvDefault([]string{"CLUSTER_AUTOSCALER_PROVIDER_CLOUD_CONFIG", "CLOUD_CONFIG"}, ""), "The path to the cloud provider configuration file. Empty string for no configuration file.")

	nodeGroupsFlag = registerMultiStringFlag(
		"nodes",
		[]string{"CLUSTER_AUTOSCALER_PROVIDER_NODES"},
		"Sets min,max size and other configuration data for a node group in a format accepted by the cloud provider. Can be used multiple times.")

	awsUseStaticInstanceList = flag.Bool("aws-use-static-instance-list", boolEnvDefault("CLUSTER_AUTOSCALER_PROVIDER_AWS_USE_STATIC_INSTANCE_LIST", false), "Use the generated static EC2 instance type list instead of calling AWS APIs at startup.")
	dryRun                   = flag.Bool("dry-run", boolEnvDefault("CLUSTER_AUTOSCALER_PROVIDER_DRY_RUN", false), "Enable dry-run mode: initialize clients and serve gRPC, but log mutating actions instead of performing them.")
)

func main() {
	klog.InitFlags(nil)
	kube_flag.InitFlags()
	flag.Parse()

	if *configPath == "" {
		klog.Fatal("--config or CLUSTER_AUTOSCALER_PROVIDER_CONFIG is required")
	}
	runtimeConfig, err := loadRuntimeConfig(*configPath)
	if err != nil {
		klog.Fatalf("failed to load runtime config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *mode {
	case "router":
		runRouter(ctx, runtimeConfig)
	case "provider":
		runProvider(ctx, runtimeConfig)
	default:
		klog.Fatalf("invalid mode %q", *mode)
	}
}

func runProvider(ctx context.Context, runtimeConfig *RuntimeConfig) {
	if *region == "" {
		klog.Fatal("provider mode requires a region selector")
	}

	settings := ProviderRuntimeSettings{Region: *region}
	var err error
	settings, err = runtimeConfig.ResolveProviderSettings(settings)
	if err != nil {
		klog.Fatalf("failed to resolve provider runtime settings: %v", err)
	}

	if settings.GRPCAddress == "" {
		klog.Fatalf("runtime config does not define a gRPC listen address for region %q", settings.Region)
	}

	klog.Infof("starting provider mode for region=%s on %s", settings.Region, settings.GRPCAddress)

	server := newServer()
	provider := buildAWSCloudProvider(settings)
	var grpcService protos.CloudProviderServer
	grpcService = upstreamwrapper.NewCloudProviderGrpcWrapper(provider)
	if *dryRun {
		klog.Infof("dry-run mode enabled: mutating operations will be logged but not executed")
		grpcService = newDryRunWrapper(grpcService)
	}
	protos.RegisterCloudProviderServer(server, grpcService)

	listener, err := net.Listen("tcp", settings.GRPCAddress)
	if err != nil {
		klog.Fatalf("failed to listen on %q: %v", settings.GRPCAddress, err)
	}

	klog.Infof("aws regional cloudprovider service (%s) listening on %s", settings.Region, settings.GRPCAddress)

	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()

	if err := server.Serve(listener); err != nil {
		klog.Fatalf("failed to serve gRPC API: %v", err)
	}
}

func runRouter(ctx context.Context, runtimeConfig *RuntimeConfig) {
	settings := RouterRuntimeSettings{
		GRPCAddress:       *grpcAddress,
		HTTPAddress:       *routerHTTPAddress,
		CacheTTL:          *routerCacheTTL,
		BackendRPCTimeout: *routerBackendRPCTimeout,
	}

	settings = runtimeConfig.ResolveRouterSettings(settings)
	if err := validateRouterSettings(settings); err != nil {
		klog.Fatalf("invalid router settings: %v", err)
	}
	if len(settings.Backends) == 0 {
		klog.Fatal("at least one configured backend is required in router mode")
	}

	opts := RouterOptions{
		CacheTTL:          settings.CacheTTL,
		BackendRPCTimeout: settings.BackendRPCTimeout,
		Backends:          settings.Backends,
	}

	klog.Infof("starting router mode on %s; configured backends: %s", settings.GRPCAddress, formatBackendSummary(opts.Backends))

	router, err := newCachingRouter(opts)
	if err != nil {
		klog.Fatalf("failed to create caching router: %v", err)
	}
	defer func() {
		if err := router.Close(ctx); err != nil {
			klog.Errorf("failed to close router backend connections: %v", err)
		}
	}()
	router.Start(ctx)

	server := newServer()
	protos.RegisterCloudProviderServer(server, router)

	httpServer := newRouterHTTPServer(settings.HTTPAddress, router)
	serveHTTPServer(ctx, httpServer, "router HTTP server")

	listener, err := net.Listen("tcp", settings.GRPCAddress)
	if err != nil {
		klog.Fatalf("failed to listen on %q: %v", settings.GRPCAddress, err)
	}

	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()

	klog.Infof("caching router listening on %s", settings.GRPCAddress)
	if err := server.Serve(listener); err != nil {
		klog.Fatalf("failed to serve gRPC API: %v", err)
	}
}

func validateRouterSettings(settings RouterRuntimeSettings) error {
	if settings.CacheTTL <= 0 {
		return fmt.Errorf("cache TTL must be greater than zero")
	}
	if settings.BackendRPCTimeout <= 0 {
		return fmt.Errorf("backend RPC timeout must be greater than zero")
	}
	return nil
}

func newServer() *grpc.Server {
	if *keyCert == "" || *cert == "" || *cacert == "" {
		klog.V(1).Info("TLS assets not provided, starting insecure gRPC server")
		return grpc.NewServer()
	}

	certificate, err := tls.LoadX509KeyPair(*cert, *keyCert)
	if err != nil {
		klog.Fatalf("failed to read TLS certificate pair: %v", err)
	}

	certPool := x509.NewCertPool()
	caBundle, err := os.ReadFile(*cacert)
	if err != nil {
		klog.Fatalf("failed to read client CA certificate: %v", err)
	}
	if ok := certPool.AppendCertsFromPEM(caBundle); !ok {
		klog.Fatal("failed to append client CA certificate")
	}

	transportCreds := credentials.NewTLS(&tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    certPool,
	})
	return grpc.NewServer(grpc.Creds(transportCreds))
}

func buildAWSCloudProvider(settings ProviderRuntimeSettings) cloudprovider.CloudProvider {
	opts := &coreoptions.AutoscalerOptions{
		AutoscalingOptions: config.AutoscalingOptions{
			CloudProviderName:         cloudprovider.AwsProviderName,
			CloudConfig:               *cloudConfig,
			NodeGroupAutoDiscovery:    settings.NodeGroupAutoDiscovery,
			NodeGroups:                nodeGroupsFlag.Values(),
			ClusterName:               settings.ClusterName,
			AWSUseStaticInstanceList:  *awsUseStaticInstanceList,
			UserAgent:                 "aws-cluster-autoscaler-external-grpc",
			SkipNodesWithLocalStorage: settings.SkipNodesWithLocalStorage,
			SkipNodesWithSystemPods:   settings.SkipNodesWithSystemPods,
		},
	}

	discovery := cloudprovider.NodeGroupDiscoveryOptions{
		NodeGroupSpecs:              opts.NodeGroups,
		NodeGroupAutoDiscoverySpecs: opts.NodeGroupAutoDiscovery,
	}

	resourceLimiter := cloudprovider.NewResourceLimiter(nil, nil)

	return newCleanupOnceProvider(buildProviderForRegion(settings.Region, func() cloudprovider.CloudProvider {
		return upstreamaws.BuildAWS(opts, discovery, resourceLimiter)
	}))
}
