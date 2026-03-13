package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"net"
	"os"
	"strings"

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

type multiStringFlag []string

func (f *multiStringFlag) String() string {
	return "[" + strings.Join(*f, " ") + "]"
}

func (f *multiStringFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func registerMultiStringFlag(name string, usage string) *multiStringFlag {
	value := new(multiStringFlag)
	flag.Var(value, name, usage)
	return value
}

var (
	address = flag.String("address", ":8086", "The address to expose the grpc service.")
	keyCert = flag.String("key-cert", "", "The path to the certificate key file. Empty string for insecure communication.")
	cert    = flag.String("cert", "", "The path to the certificate file. Empty string for insecure communication.")
	cacert  = flag.String("ca-cert", "", "The path to the ca certificate file. Empty string for insecure communication.")

	cloudConfig = flag.String("cloud-config", "", "The path to the cloud provider configuration file. Empty string for no configuration file.")
	clusterName = flag.String("cluster-name", "", "Autoscaled cluster name, if available.")

	nodeGroupsFlag = registerMultiStringFlag(
		"nodes",
		"Sets min,max size and other configuration data for a node group in a format accepted by the cloud provider. Can be used multiple times.")
	nodeGroupAutoDiscoveryFlag = registerMultiStringFlag(
		"node-group-auto-discovery",
		"One or more definition(s) of node group auto-discovery. AWS matches by ASG tags, for example `asg:tag=tagKey,anotherTagKey`.")

	awsUseStaticInstanceList = flag.Bool("aws-use-static-instance-list", false, "Use the generated static EC2 instance type list instead of calling AWS APIs at startup.")
)

func main() {
	klog.InitFlags(nil)
	kube_flag.InitFlags()
	flag.Parse()

	server := newServer()
	provider := buildAWSCloudProvider()
	protos.RegisterCloudProviderServer(server, upstreamwrapper.NewCloudProviderGrpcWrapper(provider))

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		klog.Fatalf("failed to listen on %q: %v", *address, err)
	}

	klog.Infof("aws external-grpc cloudprovider service listening on %s", *address)
	if err := server.Serve(listener); err != nil {
		klog.Fatalf("failed to serve gRPC API: %v", err)
	}
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

func buildAWSCloudProvider() cloudprovider.CloudProvider {
	opts := &coreoptions.AutoscalerOptions{
		AutoscalingOptions: config.AutoscalingOptions{
			CloudProviderName:        cloudprovider.AwsProviderName,
			CloudConfig:              *cloudConfig,
			NodeGroupAutoDiscovery:   *nodeGroupAutoDiscoveryFlag,
			NodeGroups:               *nodeGroupsFlag,
			ClusterName:              *clusterName,
			AWSUseStaticInstanceList: *awsUseStaticInstanceList,
			UserAgent:                "aws-cluster-autoscaler-external-grpc",
		},
	}

	discovery := cloudprovider.NodeGroupDiscoveryOptions{
		NodeGroupSpecs:              opts.NodeGroups,
		NodeGroupAutoDiscoverySpecs: opts.NodeGroupAutoDiscovery,
	}

	resourceLimiter := cloudprovider.NewResourceLimiter(nil, nil)
	return upstreamaws.BuildAWS(opts, discovery, resourceLimiter)
}
