package start

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strconv"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/util/flag"
	"k8s.io/kubernetes/pkg/master/ports"
	"k8s.io/kubernetes/pkg/registry/core/service/ipallocator"

	legacyconfigv1 "github.com/openshift/api/legacyconfig/v1"
	"github.com/openshift/origin/pkg/cmd/flagtypes"
	"github.com/openshift/origin/pkg/cmd/server/admin"
	configapi "github.com/openshift/origin/pkg/cmd/server/apis/config"
	"github.com/openshift/origin/pkg/cmd/server/bootstrappolicy"
	cmdutil "github.com/openshift/origin/pkg/cmd/util"
	"github.com/spf13/cobra"
)

// MasterArgs is a struct that the command stores flag values into.  It holds a partially complete set of parameters for starting the master
// This object should hold the common set values, but not attempt to handle all cases.  The expected path is to use this object to create
// a fully specified config later on.  If you need something not set here, then create a fully specified config file and pass that as argument
// to starting the master.
type MasterArgs struct {
	// MasterAddr is the master address for use by OpenShift components (host, host:port, or URL).
	// Scheme and port default to the --listen scheme and port. When unset, attempt to use the first
	// public IPv4 non-loopback address registered on this host.
	MasterAddr flagtypes.Addr

	// EtcdAddr is the address of the etcd server (host, host:port, or URL). If specified, no built-in
	// etcd will be started.
	EtcdAddr flagtypes.Addr

	// MasterPublicAddr is the master address for use by public clients, if different (host, host:port,
	// or URL). Defaults to same as --master.
	MasterPublicAddr flagtypes.Addr

	// StartAPI controls whether the API component of the master is started (to support the API role)
	// TODO: once we implement bastion role and kube/os controller role, revisit
	StartAPI bool
	// StartControllers controls whether the controller component of the master is started (to support
	// the controller role)
	StartControllers bool

	// DNSBindAddr exposed for integration tests to set
	DNSBindAddr flagtypes.Addr

	// EtcdDir is the etcd data directory.
	EtcdDir   string
	ConfigDir *flag.StringFlag

	// CORSAllowedOrigins is a list of allowed origins for CORS, comma separated.
	// An allowed origin can be a regular expression to support subdomain matching.
	// CORS is enabled for localhost, 127.0.0.1, and the asset server by default.
	CORSAllowedOrigins []string

	APIServerCAFiles []string

	ListenArg          *ListenArg
	ImageFormatArgs    *ImageFormatArgs
	KubeConnectionArgs *KubeConnectionArgs

	SchedulerConfigFile string

	NetworkArgs *NetworkArgs

	OverrideConfig func(config *configapi.MasterConfig) error
}

// BindMasterArgs binds the options to the flags with prefix + default flag names
func BindMasterArgs(args *MasterArgs, flags *pflag.FlagSet, prefix string) {
	flags.Var(&args.MasterAddr, prefix+"master", "The master address for use by OpenShift components (host, host:port, or URL). Scheme and port default to the --listen scheme and port. When unset, attempt to use the first public IPv4 non-loopback address registered on this host.")
	flags.Var(&args.MasterPublicAddr, prefix+"public-master", "The master address for use by public clients, if different (host, host:port, or URL). Defaults to same as --master.")
	flags.Var(&args.EtcdAddr, prefix+"etcd", "The address of the etcd server (host, host:port, or URL). If specified, no built-in etcd will be started.")
	flags.Var(&args.DNSBindAddr, prefix+"dns", "The address to listen for DNS requests on.")

	flags.StringVar(&args.EtcdDir, prefix+"etcd-dir", "openshift.local.etcd", "The etcd data directory.")

	flags.StringSliceVar(&args.APIServerCAFiles, prefix+"certificate-authority", args.APIServerCAFiles, "Optional files containing signing authorities to use (in addition to the generated signer) to verify the API server's serving certificate.")

	flags.StringSliceVar(&args.CORSAllowedOrigins, prefix+"cors-allowed-origins", []string{}, "List of allowed origins for CORS, comma separated.  An allowed origin can be a regular expression to support subdomain matching.  CORS is enabled for localhost, 127.0.0.1, and the asset server by default.")

	// autocompletion hints
	cobra.MarkFlagFilename(flags, prefix+"etcd-dir")
	cobra.MarkFlagFilename(flags, prefix+"certificate-authority")
}

// NewDefaultMasterArgs creates MasterArgs with sub-objects created and default values set.
func NewDefaultMasterArgs() *MasterArgs {
	config := &MasterArgs{
		MasterAddr:       flagtypes.Addr{Value: "localhost:8443", DefaultScheme: "https", DefaultPort: 8443, AllowPrefix: true}.Default(),
		EtcdAddr:         flagtypes.Addr{Value: "0.0.0.0:4001", DefaultScheme: "https", DefaultPort: 4001}.Default(),
		MasterPublicAddr: flagtypes.Addr{Value: "localhost:8443", DefaultScheme: "https", DefaultPort: 8443, AllowPrefix: true}.Default(),
		DNSBindAddr:      flagtypes.Addr{Value: "0.0.0.0:8053", DefaultScheme: "tcp", DefaultPort: 8053, AllowPrefix: true}.Default(),

		ConfigDir: &flag.StringFlag{},

		ListenArg:          NewDefaultListenArg(),
		ImageFormatArgs:    NewDefaultImageFormatArgs(),
		KubeConnectionArgs: NewDefaultKubeConnectionArgs(),
		NetworkArgs:        NewDefaultMasterNetworkArgs(),
	}

	return config
}

// GetConfigFileToWrite returns the configuration filepath for master
func (args MasterArgs) GetConfigFileToWrite() string {
	return path.Join(args.ConfigDir.Value(), "master-config.yaml")
}

// makeHostMatchRegex returns a regex that matches this host exactly.
// If host contains a port, the returned regex matches the port exactly.
// If host does not contain a port, the returned regex matches any port or no port.
func makeHostMatchRegex(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		// we have a port, match the end exactly
		return "//" + regexp.QuoteMeta(host) + "$"
	} else {
		// we don't have a port, match a port separator or the end
		return "//" + regexp.QuoteMeta(host) + "(:|$)"
	}
}

// BuildSerializeableMasterConfig takes the MasterArgs (partially complete config) and uses them along with defaulting behavior to create the fully specified
// config object for starting the master
func (args MasterArgs) BuildSerializeableMasterConfig() (*configapi.MasterConfig, error) {
	masterPublicAddr, err := args.GetMasterPublicAddress()
	if err != nil {
		return nil, err
	}
	assetPublicAddr, err := args.GetAssetPublicAddress()
	if err != nil {
		return nil, err
	}
	dnsBindAddr, err := args.GetDNSBindAddress()
	if err != nil {
		return nil, err
	}

	listenServingInfo := servingInfoForAddr(&args.ListenArg.ListenAddr)

	// always include the all-in-one server's web console as an allowed CORS origin
	// always include localhost as an allowed CORS origin
	// always include master public address as an allowed CORS origin
	corsAllowedOrigins := sets.NewString(args.CORSAllowedOrigins...)
	corsAllowedOrigins.Insert(
		makeHostMatchRegex(assetPublicAddr.Host),
		makeHostMatchRegex(masterPublicAddr.Host),
		makeHostMatchRegex("localhost"),
		makeHostMatchRegex("127.0.0.1"),
	)

	etcdAddress, err := args.GetEtcdAddress()
	if err != nil {
		return nil, err
	}

	builtInEtcd := !args.EtcdAddr.Provided
	var etcdConfig *configapi.EtcdConfig
	if builtInEtcd {
		etcdConfig, err = args.BuildSerializeableEtcdConfig()
		if err != nil {
			return nil, err
		}
	}

	kubernetesMasterConfig, err := args.BuildSerializeableKubeMasterConfig()
	if err != nil {
		return nil, err
	}

	oauthConfig, err := args.BuildSerializeableOAuthConfig()
	if err != nil {
		return nil, err
	}

	kubeletClientInfo := admin.DefaultMasterKubeletClientCertInfo(args.ConfigDir.Value())

	etcdClientInfo := admin.DefaultMasterEtcdClientCertInfo(args.ConfigDir.Value())

	serviceServingCertSigner := admin.DefaultServiceSignerCAInfo(args.ConfigDir.Value())

	dnsServingInfo := servingInfoForAddr(&dnsBindAddr)

	config := &configapi.MasterConfig{
		ServingInfo: configapi.HTTPServingInfo{
			ServingInfo: listenServingInfo,
		},
		CORSAllowedOrigins: corsAllowedOrigins.List(),
		MasterPublicURL:    masterPublicAddr.String(),

		KubernetesMasterConfig: *kubernetesMasterConfig,
		EtcdConfig:             etcdConfig,

		AuthConfig: configapi.MasterAuthConfig{
			RequestHeader: &configapi.RequestHeaderAuthenticationOptions{
				ClientCA:            admin.DefaultCertFilename(args.ConfigDir.Value(), admin.FrontProxyCAFilePrefix),
				ClientCommonNames:   []string{bootstrappolicy.AggregatorUsername},
				UsernameHeaders:     []string{"X-Remote-User"},
				GroupHeaders:        []string{"X-Remote-Group"},
				ExtraHeaderPrefixes: []string{"X-Remote-Extra-"}},
		},

		AggregatorConfig: configapi.AggregatorConfig{
			ProxyClientInfo: admin.DefaultAggregatorClientCertInfo(args.ConfigDir.Value()).CertLocation,
		},

		OAuthConfig: oauthConfig,

		DNSConfig: &configapi.DNSConfig{
			BindAddress: dnsServingInfo.BindAddress,
			BindNetwork: dnsServingInfo.BindNetwork,

			AllowRecursiveQueries: true,
		},

		MasterClients: configapi.MasterClients{
			OpenShiftLoopbackKubeConfig: admin.DefaultKubeConfigFilename(args.ConfigDir.Value(), bootstrappolicy.MasterUnqualifiedUsername),
		},

		EtcdClientInfo: configapi.EtcdConnectionInfo{
			URLs: []string{etcdAddress.String()},
		},

		KubeletClientInfo: configapi.KubeletConnectionInfo{
			Port: ports.KubeletPort,
		},

		ImageConfig: configapi.ImageConfig{
			Format: args.ImageFormatArgs.ImageTemplate.Format,
			Latest: args.ImageFormatArgs.ImageTemplate.Latest,
		},

		ImagePolicyConfig: configapi.ImagePolicyConfig{},

		ProjectConfig: configapi.ProjectConfig{
			DefaultNodeSelector:    "",
			ProjectRequestMessage:  "",
			ProjectRequestTemplate: "",

			// Allocator defaults on
			SecurityAllocator: &configapi.SecurityAllocator{},
		},

		NetworkConfig: configapi.MasterNetworkConfig{
			NetworkPluginName: args.NetworkArgs.NetworkPluginName,
			ClusterNetworks: []configapi.ClusterNetworkEntry{
				{
					CIDR:             args.NetworkArgs.ClusterNetworkCIDR,
					HostSubnetLength: args.NetworkArgs.HostSubnetLength,
				},
			},
			ServiceNetworkCIDR: args.NetworkArgs.ServiceNetworkCIDR,
		},

		VolumeConfig: configapi.MasterVolumeConfig{
			DynamicProvisioningEnabled: true,
		},

		ControllerConfig: configapi.ControllerConfig{
			ServiceServingCert: configapi.ServiceServingCert{
				Signer: &serviceServingCertSigner,
			},
		},
	}

	config.ServingInfo.ServerCert = admin.DefaultMasterServingCertInfo(args.ConfigDir.Value())
	config.ServingInfo.ClientCA = admin.DefaultAPIClientCAFile(args.ConfigDir.Value())

	if oauthConfig != nil {
		s := admin.DefaultCABundleFile(args.ConfigDir.Value())
		oauthConfig.MasterCA = &s
	}

	config.KubeletClientInfo.CA = admin.DefaultRootCAFile(args.ConfigDir.Value())
	config.KubeletClientInfo.ClientCert = kubeletClientInfo.CertLocation
	config.ServiceAccountConfig.MasterCA = admin.DefaultCABundleFile(args.ConfigDir.Value())

	// Only set up ca/cert info for etcd connections if we're self-hosting etcd
	if builtInEtcd {
		config.EtcdClientInfo.CA = admin.DefaultRootCAFile(args.ConfigDir.Value())
		config.EtcdClientInfo.ClientCert = etcdClientInfo.CertLocation
	}

	// We're responsible for generating all the managed service accounts
	config.ServiceAccountConfig.ManagedNames = []string{
		bootstrappolicy.DefaultServiceAccountName,
		bootstrappolicy.BuilderServiceAccountName,
		bootstrappolicy.DeployerServiceAccountName,
	}
	// We also need the private key file to give to the token generator
	config.ServiceAccountConfig.PrivateKeyFile = admin.DefaultServiceAccountPrivateKeyFile(args.ConfigDir.Value())
	// We also need the public key file to give to the authenticator
	config.ServiceAccountConfig.PublicKeyFiles = []string{
		admin.DefaultServiceAccountPublicKeyFile(args.ConfigDir.Value()),
	}

	internal, err := applyDefaults(config, legacyconfigv1.LegacySchemeGroupVersion)
	if err != nil {
		return nil, err
	}
	config = internal.(*configapi.MasterConfig)

	// When creating a new config, use Protobuf
	configapi.SetProtobufClientDefaults(config.MasterClients.OpenShiftLoopbackClientConnectionOverrides)

	return config, nil
}

func (args MasterArgs) BuildSerializeableOAuthConfig() (*configapi.OAuthConfig, error) {
	masterAddr, err := args.GetMasterAddress()
	if err != nil {
		return nil, err
	}
	masterPublicAddr, err := args.GetMasterPublicAddress()
	if err != nil {
		return nil, err
	}
	assetPublicAddr, err := args.GetAssetPublicAddress()
	if err != nil {
		return nil, err
	}

	config := &configapi.OAuthConfig{
		MasterURL:       masterAddr.String(),
		MasterPublicURL: masterPublicAddr.String(),
		AssetPublicURL:  assetPublicAddr.String(),

		IdentityProviders: []configapi.IdentityProvider{},
		GrantConfig: configapi.GrantConfig{
			Method: configapi.GrantHandlerAuto,
		},

		SessionConfig: &configapi.SessionConfig{
			SessionSecretsFile:   "",
			SessionMaxAgeSeconds: 5 * 60, // 5 minutes
			SessionName:          "ssn",
		},

		TokenConfig: configapi.TokenConfig{
			AuthorizeTokenMaxAgeSeconds:         5 * 60,       // 5 minutes
			AccessTokenMaxAgeSeconds:            24 * 60 * 60, // 1 day
			AccessTokenInactivityTimeoutSeconds: nil,          // no timeouts by default
		},
	}

	config.IdentityProviders = append(config.IdentityProviders,
		configapi.IdentityProvider{
			Name:            "anypassword",
			UseAsChallenger: true,
			UseAsLogin:      true,
			Provider:        &configapi.AllowAllPasswordIdentityProvider{},
		},
	)

	return config, nil
}

// BuildSerializeableEtcdConfig creates a fully specified etcd startup configuration based on MasterArgs
func (args MasterArgs) BuildSerializeableEtcdConfig() (*configapi.EtcdConfig, error) {
	etcdAddr, err := args.GetEtcdAddress()
	if err != nil {
		return nil, err
	}

	etcdPeerAddr, err := args.GetEtcdPeerAddress()
	if err != nil {
		return nil, err
	}

	config := &configapi.EtcdConfig{
		ServingInfo: configapi.ServingInfo{
			BindAddress: args.GetEtcdBindAddress(),
		},
		PeerServingInfo: configapi.ServingInfo{
			BindAddress: args.GetEtcdPeerBindAddress(),
		},
		Address:     etcdAddr.Host,
		PeerAddress: etcdPeerAddr.Host,
		StorageDir:  args.EtcdDir,
	}

	if args.ListenArg.UseTLS() {
		config.ServingInfo.ServerCert = admin.DefaultEtcdServingCertInfo(args.ConfigDir.Value())
		config.ServingInfo.ClientCA = admin.DefaultEtcdClientCAFile(args.ConfigDir.Value())

		config.PeerServingInfo.ServerCert = admin.DefaultEtcdServingCertInfo(args.ConfigDir.Value())
		config.PeerServingInfo.ClientCA = admin.DefaultEtcdClientCAFile(args.ConfigDir.Value())
	}

	return config, nil

}

// BuildSerializeableKubeMasterConfig creates a fully specified kubernetes master startup configuration based on MasterArgs
func (args MasterArgs) BuildSerializeableKubeMasterConfig() (*configapi.KubernetesMasterConfig, error) {
	masterAddr, err := args.GetMasterAddress()
	if err != nil {
		return nil, err
	}
	masterHost, _, err := net.SplitHostPort(masterAddr.Host)
	if err != nil {
		masterHost = masterAddr.Host
	}
	masterIP := ""
	if ip := net.ParseIP(masterHost); ip != nil {
		masterIP = ip.String()
	}

	config := &configapi.KubernetesMasterConfig{
		MasterIP:            masterIP,
		ServicesSubnet:      args.NetworkArgs.ServiceNetworkCIDR,
		SchedulerConfigFile: args.SchedulerConfigFile,
		ProxyClientInfo:     admin.DefaultProxyClientCertInfo(args.ConfigDir.Value()).CertLocation,
	}

	return config, nil
}

func (args MasterArgs) Validate() error {
	masterAddr, err := args.GetMasterAddress()
	if err != nil {
		return err
	}
	if len(masterAddr.Path) != 0 {
		return fmt.Errorf("master url may not include a path: '%v'", masterAddr.Path)
	}

	addr, err := args.GetMasterPublicAddress()
	if err != nil {
		return err
	}
	if len(addr.Path) != 0 {
		return fmt.Errorf("master public url may not include a path: '%v'", addr.Path)
	}

	if err := args.KubeConnectionArgs.Validate(); err != nil {
		return err
	}

	addr, err = args.KubeConnectionArgs.GetKubernetesAddress(masterAddr)
	if err != nil {
		return err
	}
	if len(addr.Path) != 0 {
		return fmt.Errorf("kubernetes url may not include a path: '%v'", addr.Path)
	}

	return nil
}

// GetServerCertHostnames returns the set of hostnames that any serving certificate for master needs to be valid for.
func (args MasterArgs) GetServerCertHostnames() (sets.String, error) {
	masterAddr, err := args.GetMasterAddress()
	if err != nil {
		return nil, err
	}
	masterPublicAddr, err := args.GetMasterPublicAddress()
	if err != nil {
		return nil, err
	}
	assetPublicAddr, err := args.GetAssetPublicAddress()
	if err != nil {
		return nil, err
	}

	allHostnames := sets.NewString(
		"localhost", "127.0.0.1",
		"openshift.default.svc.cluster.local",
		"openshift.default.svc",
		"openshift.default",
		"openshift",
		"kubernetes.default.svc.cluster.local",
		"kubernetes.default.svc",
		"kubernetes.default",
		"kubernetes",
		"etcd.kube-system.svc",
		masterAddr.Host, masterPublicAddr.Host, assetPublicAddr.Host)

	if _, ipnet, err := net.ParseCIDR(args.NetworkArgs.ServiceNetworkCIDR); err == nil {
		// CIDR is ignored if it is invalid, other code handles validation.
		if firstServiceIP, err := ipallocator.GetIndexedIP(ipnet, 1); err == nil {
			allHostnames.Insert(firstServiceIP.String())
		}
	}

	listenIP := net.ParseIP(args.ListenArg.ListenAddr.Host)
	// add the IPs that might be used based on the ListenAddr.
	if listenIP != nil && listenIP.IsUnspecified() {
		allAddresses, _ := cmdutil.AllLocalIP4()
		for _, ip := range allAddresses {
			allHostnames.Insert(ip.String())
		}
	} else {
		allHostnames.Insert(args.ListenArg.ListenAddr.Host)
	}

	certHostnames := sets.String{}
	for hostname := range allHostnames {
		if host, _, err := net.SplitHostPort(hostname); err == nil {
			// add the hostname without the port
			certHostnames.Insert(host)
		} else {
			// add the originally specified hostname
			certHostnames.Insert(hostname)
		}
	}

	return certHostnames, nil
}

// GetMasterAddress checks for an unset master address and then attempts to use the first
// public IPv4 non-loopback address registered on this host.
// TODO: make me IPv6 safe
func (args MasterArgs) GetMasterAddress() (*url.URL, error) {
	if args.MasterAddr.Provided {
		return args.MasterAddr.URL, nil
	}

	// Use the bind port by default
	port := args.ListenArg.ListenAddr.Port

	// Use the bind scheme by default
	scheme := args.ListenArg.ListenAddr.URL.Scheme

	addr := ""
	if ip, err := cmdutil.DefaultLocalIP4(); err == nil {
		addr = ip.String()
	} else if err == cmdutil.ErrorNoDefaultIP {
		addr = "127.0.0.1"
	} else if err != nil {
		return nil, fmt.Errorf("Unable to find a public IP address: %v", err)
	}

	masterAddr := scheme + "://" + net.JoinHostPort(addr, strconv.Itoa(port))
	return url.Parse(masterAddr)
}

func (args MasterArgs) GetDNSBindAddress() (flagtypes.Addr, error) {
	if args.DNSBindAddr.Provided {
		return args.DNSBindAddr, nil
	}
	dnsAddr := flagtypes.Addr{Value: args.ListenArg.ListenAddr.Host, DefaultPort: args.DNSBindAddr.DefaultPort}.Default()
	return dnsAddr, nil
}

func (args MasterArgs) GetMasterPublicAddress() (*url.URL, error) {
	if args.MasterPublicAddr.Provided {
		return args.MasterPublicAddr.URL, nil
	}

	return args.GetMasterAddress()
}

// GetEtcdBindAddress derives the etcd bind address by using the bind address and
// the default etcd port
func (args MasterArgs) GetEtcdBindAddress() string {
	return net.JoinHostPort(args.ListenArg.ListenAddr.Host, strconv.Itoa(args.EtcdAddr.DefaultPort))
}

// GetEtcdPeerBindAddress derives the etcd peer address by using the bind address
// and the default etcd peering port
func (args MasterArgs) GetEtcdPeerBindAddress() string {
	return net.JoinHostPort(args.ListenArg.ListenAddr.Host, "7001")
}

// GetEtcdAddress returns the address for etcd
func (args MasterArgs) GetEtcdAddress() (*url.URL, error) {
	if args.EtcdAddr.Provided {
		return args.EtcdAddr.URL, nil
	}

	// Etcd should be reachable on the same address that the master is (for simplicity)
	masterAddr, err := args.GetMasterAddress()
	if err != nil {
		return nil, err
	}

	return &url.URL{
		// Use the bind scheme by default
		Scheme: args.ListenArg.ListenAddr.URL.Scheme,

		Host: net.JoinHostPort(getHost(*masterAddr), strconv.Itoa(args.EtcdAddr.DefaultPort)),
	}, nil
}

func (args MasterArgs) GetEtcdPeerAddress() (*url.URL, error) {
	// Derive from the etcd address, on port 7001
	etcdAddress, err := args.GetEtcdAddress()
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(etcdAddress.Host)
	if err != nil {
		return nil, err
	}

	etcdAddress.Host = net.JoinHostPort(host, "7001")

	return etcdAddress, nil
}

func (args MasterArgs) GetAssetPublicAddress() (*url.URL, error) {
	t, err := args.GetMasterPublicAddress()
	if err != nil {
		return nil, err
	}
	assetPublicAddr := *t
	assetPublicAddr.Path = "/console/" // TODO: make a constant

	return &assetPublicAddr, nil
}

func getHost(theURL url.URL) string {
	host, _, err := net.SplitHostPort(theURL.Host)
	if err != nil {
		return theURL.Host
	}

	return host
}

// applyDefaults roundtrips the config to v1 and back to ensure proper defaults are set.
func applyDefaults(config runtime.Object, version schema.GroupVersion) (runtime.Object, error) {
	ext, err := configapi.Scheme.ConvertToVersion(config, version)
	if err != nil {
		return nil, err
	}
	configapi.Scheme.Default(ext)
	return configapi.Scheme.ConvertToVersion(ext, configapi.SchemeGroupVersion)
}

func servingInfoForAddr(addr *flagtypes.Addr) configapi.ServingInfo {
	info := configapi.ServingInfo{
		BindAddress: addr.URL.Host,
	}
	if addr.IPv6Host {
		info.BindNetwork = "tcp6"
	}
	return info
}
