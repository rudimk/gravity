package install

import (
	"context"

	"github.com/gravitational/gravity/lib/app"
	"github.com/gravitational/gravity/lib/checks"
	cloudgce "github.com/gravitational/gravity/lib/cloudprovider/gce"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/loc"
	validationpb "github.com/gravitational/gravity/lib/network/validation/proto"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/ops/opsclient"
	"github.com/gravitational/gravity/lib/pack"
	"github.com/gravitational/gravity/lib/process"
	"github.com/gravitational/gravity/lib/rpc"
	pb "github.com/gravitational/gravity/lib/rpc/proto"
	rpcserver "github.com/gravitational/gravity/lib/rpc/server"
	"github.com/gravitational/gravity/lib/schema"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/systeminfo"
	"github.com/gravitational/gravity/lib/utils"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
)

// RunLocalChecks executes host-local preflight checks for this configuration
func (c *Config) RunLocalChecks(ctx context.Context) error {
	app, err := c.GetApp()
	if err != nil {
		return trace.Wrap(err)
	}
	return trace.Wrap(checks.RunLocalChecks(ctx, checks.LocalChecksRequest{
		Manifest: app.Manifest,
		Role:     c.Role,
		Docker:   c.Docker,
		Options: &validationpb.ValidateOptions{
			VxlanPort: int32(c.VxlanPort),
			DnsAddrs:  c.DNSConfig.Addrs,
			DnsPort:   int32(c.DNSConfig.Port),
		},
		AutoFix: true,
	}))
}

// GetApp returns the application for this configuration
func (c *Config) GetApp() (*app.Application, error) {
	app, err := c.Apps.GetApp(*c.AppPackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return app, nil
}

// Config is installer configuration
type Config struct {
	// FieldLogger is used for logging
	log.FieldLogger
	// Printer specifies the output sink for progress messages
	utils.Printer
	// AdvertiseAddr is advertise address of this server
	AdvertiseAddr string
	// Token specifies the agent validation token used during the operation
	Token storage.InstallToken
	// CloudProvider is optional cloud provider
	CloudProvider string
	// StateDir is directory with local installer state
	StateDir string
	// WriteStateDir is installer write layer
	WriteStateDir string
	// UserLogFile is the log file where user-facing operation logs go
	UserLogFile string
	// SystemLogFile is the log file for system logs
	SystemLogFile string
	// SiteDomain is the name of the cluster
	SiteDomain string
	// Flavor is installation flavor
	Flavor *schema.Flavor
	// Role is server role
	Role string
	// AppPackage is the application being installed
	AppPackage *loc.Locator
	// RuntimeResources specifies optional Kubernetes resources to create
	RuntimeResources []runtime.Object
	// ClusterResources specifies optional cluster resources to create
	// TODO(dmitri): externalize the ClusterConfiguration resource and create
	// default provider-specific cloud-config on Gravity side
	ClusterResources []storage.UnknownResource
	// SystemDevice is a device for gravity data
	SystemDevice string
	// DockerDevice is a device for docker
	DockerDevice string
	// Mounts is a list of mount points (name -> source pairs)
	Mounts map[string]string
	// DNSOverrides contains installer node DNS overrides
	DNSOverrides storage.DNSOverrides
	// PodCIDR is a pod network CIDR
	PodCIDR string
	// ServiceCIDR is a service network CIDR
	ServiceCIDR string
	// VxlanPort is the overlay network port
	VxlanPort int
	// DNSConfig overrides the local cluster DNS configuration
	DNSConfig storage.DNSConfig
	// Docker specifies docker configuration
	Docker storage.DockerConfig
	// Insecure allows to turn off cert validation
	Insecure bool
	// Process is the gravity process running inside the installer
	Process process.GravityProcess
	// LocalPackages is the machine-local package service
	LocalPackages pack.PackageService
	// LocalApps is the machine-local application service
	LocalApps app.Applications
	// LocalBackend is the machine-local backend
	LocalBackend storage.Backend
	// ServiceUser specifies the user to use as a service user in planet
	// and for unprivileged kubernetes services
	ServiceUser systeminfo.User
	// GCENodeTags specifies additional VM instance tags on GCE
	GCENodeTags []string
	// LocalClusterClient is a factory for creating client to the installed cluster
	LocalClusterClient func() (*opsclient.Client, error)
	// Operator specifies the wizard's operator service
	Operator ops.Operator
	// Apps specifies the wizard's application service
	Apps app.Applications
	// Packages specifies the wizard's package service
	Packages pack.PackageService
	// AbortHandler specifies the handler for aborting the installation
	AbortHandler func(context.Context) error
	// UninstallHandler specifies the handler for cleanup during shutdown
	UninstallHandler func(context.Context) error
	// LocalAgent specifies whether the installer will also run an agent
	LocalAgent bool
}

// checkAndSetDefaults checks the parameters and autodetects some defaults
func (c *Config) checkAndSetDefaults(ctx context.Context) (err error) {
	if c.AdvertiseAddr == "" {
		return trace.BadParameter("missing AdvertiseAddr")
	}
	if c.LocalClusterClient == nil {
		return trace.BadParameter("missing LocalClusterClient")
	}
	if c.Apps == nil {
		return trace.BadParameter("missing Apps")
	}
	if c.Packages == nil {
		return trace.BadParameter("missing Packages")
	}
	if c.Operator == nil {
		return trace.BadParameter("missing Operator")
	}
	if err := CheckAddr(c.AdvertiseAddr); err != nil {
		return trace.Wrap(err)
	}
	if err := c.Docker.Check(); err != nil {
		return trace.Wrap(err)
	}
	if c.Process == nil {
		return trace.BadParameter("missing Process")
	}
	if c.LocalPackages == nil {
		return trace.BadParameter("missing LocalPackages")
	}
	if c.LocalApps == nil {
		return trace.BadParameter("missing LocalApps")
	}
	if c.LocalBackend == nil {
		return trace.BadParameter("missing LocalBackend")
	}
	if c.AppPackage == nil {
		return trace.BadParameter("missing AppPackage")
	}
	if c.AbortHandler == nil {
		return trace.BadParameter("missing AbortHandler")
	}
	if c.UninstallHandler == nil {
		return trace.BadParameter("missing UninstallHandler")
	}
	if c.VxlanPort < 1 || c.VxlanPort > 65535 {
		return trace.BadParameter("invalid vxlan port: must be in range 1-65535")
	}
	if err := c.validateCloudConfig(); err != nil {
		return trace.Wrap(err)
	}
	if c.DNSConfig.IsEmpty() {
		c.DNSConfig = storage.DefaultDNSConfig
	}
	return nil
}

func (c *Config) validateCloudConfig() (err error) {
	c.CloudProvider, err = ValidateCloudProvider(c.CloudProvider)
	if err != nil {
		return trace.Wrap(err)
	}
	if c.CloudProvider != schema.ProviderGCE {
		return nil
	}
	// TODO(dmitri): skip validations if user provided custom cloud configuration
	if err := cloudgce.ValidateTag(c.SiteDomain); err != nil {
		log.WithError(err).Warnf("Failed to validate cluster name %v as node tag on GCE.", c.SiteDomain)
		if len(c.GCENodeTags) == 0 {
			return trace.BadParameter("specified cluster name %q does "+
				"not conform to GCE tag value specification "+
				"and no node tags have been specified.\n"+
				"Either provide a conforming cluster name or use --gce-node-tag "+
				"to specify the node tag explicitly.\n"+
				"See https://cloud.google.com/vpc/docs/add-remove-network-tags for details.", c.SiteDomain)
		}
	}
	var errors []error
	for _, tag := range c.GCENodeTags {
		if err := cloudgce.ValidateTag(tag); err != nil {
			errors = append(errors, trace.Wrap(err, "failed to validate tag %q", tag))
		}
	}
	if len(errors) != 0 {
		return trace.NewAggregate(errors...)
	}
	// Use cluster name as node tag
	if len(c.GCENodeTags) == 0 {
		c.GCENodeTags = append(c.GCENodeTags, c.SiteDomain)
	}
	return nil
}

// newAgent creates a new installer agent
func (c *Config) newAgent(ctx context.Context) (*rpcserver.PeerServer, error) {
	err := ExportRPCCredentials(ctx, c.Packages, c.FieldLogger)
	if err != nil {
		return nil, trace.Wrap(err, "failed to export RPC credentials")
	}
	serverCreds, clientCreds, err := rpc.Credentials(defaults.RPCAgentSecretsDir)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var mounts []*pb.Mount
	for name, source := range c.Mounts {
		mounts = append(mounts, &pb.Mount{Name: name, Source: source})
	}
	runtimeConfig := pb.RuntimeConfig{
		SystemDevice: c.SystemDevice,
		DockerDevice: c.DockerDevice,
		Role:         c.Role,
		Mounts:       mounts,
		Token:        c.Token.Token,
	}
	return NewAgent(ctx, AgentConfig{
		FieldLogger:   c.FieldLogger,
		AdvertiseAddr: c.AdvertiseAddr,
		ServerAddr:    c.Process.AgentService().ServerAddr(),
		Credentials: rpcserver.Credentials{
			Server: serverCreds,
			Client: clientCreds,
		},
		RuntimeConfig:         runtimeConfig,
		AbortHandler:          c.AbortHandler,
		UninstallHandler:      c.UninstallHandler,
		SkipConnectValidation: true,
		ReconnectStrategy: &rpcserver.ReconnectStrategy{
			ShouldReconnect: func(err error) error {
				// Reconnect forever
				return err
			},
		},
	})
}
