/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/httplib"
	"github.com/gravitational/gravity/lib/install"
	"github.com/gravitational/gravity/lib/localenv"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/pack/webpack"
	"github.com/gravitational/gravity/lib/processconfig"
	rpcserver "github.com/gravitational/gravity/lib/rpc/server"
	"github.com/gravitational/gravity/lib/state"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/systeminfo"
	"github.com/gravitational/gravity/lib/utils"
	"github.com/gravitational/gravity/tool/common"
	"github.com/gravitational/roundtrip"

	"github.com/gravitational/trace"
)

// NewLocalEnv returns an instance of a local environment.
func (g *Application) NewLocalEnv() (*localenv.LocalEnvironment, error) {
	stateDir := *g.StateDir
	// most commands (with the exception of update or join/expand)
	// use the state directory set by original install/join command,
	// unless it was specified explicitly
	if stateDir == "" {
		dir, err := state.GetStateDir()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		stateDir = filepath.Join(dir, defaults.LocalDir)
	}
	return g.getEnv(stateDir)
}

// NewUpdateEnv returns an instance of the local environment that is used
// only for updates
func (g *Application) NewUpdateEnv() (*localenv.LocalEnvironment, error) {
	dir, err := state.GetStateDir()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return g.getEnv(state.GravityUpdateDir(dir))
}

// NewInstallEnv returns an instance of local environment where install-specific data is stored
func (g *Application) NewInstallEnv() (*localenv.LocalEnvironment, error) {
	err := os.MkdirAll(defaults.GravityInstallDir(), defaults.SharedDirMask)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	return g.getEnv(defaults.GravityInstallDir())
}

// NewJoinEnv returns an instance of local environment where join-specific data is stored
func (g *Application) NewJoinEnv() (*localenv.LocalEnvironment, error) {
	err := os.MkdirAll(defaults.GravityJoinDir(), defaults.SharedDirMask)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	return g.getEnv(defaults.GravityJoinDir())
}

func (g *Application) getEnv(stateDir string) (*localenv.LocalEnvironment, error) {
	args := localenv.LocalEnvironmentArgs{
		StateDir:         stateDir,
		Insecure:         *g.Insecure,
		Silent:           localenv.Silent(*g.Silent),
		Debug:            *g.Debug,
		EtcdRetryTimeout: *g.EtcdRetryTimeout,
		Reporter:         common.ProgressReporter(*g.Silent),
	}
	if *g.StateDir != defaults.LocalGravityDir {
		args.LocalKeyStoreDir = *g.StateDir
	}
	// set insecure in devmode so we won't need to use
	// --insecure flag all the time
	cfg, _, err := processconfig.ReadConfig("")
	if err == nil && cfg.Devmode {
		args.Insecure = true
	}
	return localenv.NewLocalEnvironment(args)
}

// SetStateDirFromCommand sets a new state directory if it has been overridden on command line.
// It only does this for a select subset of commands - those that install a cluster and thus need
// to set up the state directory.
// cmd specifies the invoked command
func (g *Application) SetStateDirFromCommand(cmd string) error {
	if cmd != g.InstallCmd.FullCommand() && cmd != g.JoinExecuteCmd.FullCommand() {
		return nil
	}
	// if a custom state directory was provided during install/join, it means
	// that user wants all gravity data to be stored under this directory
	return trace.Wrap(state.SetStateDir(*g.StateDir))
}

// isUpdateCommand returns true if the specified command is
// an upgrade related command
func (g *Application) isUpdateCommand(cmd string) bool {
	switch cmd {
	case g.PlanCmd.FullCommand(),
		g.PlanDisplayCmd.FullCommand(),
		g.PlanExecuteCmd.FullCommand(),
		g.PlanRollbackCmd.FullCommand(),
		g.PlanResumeCmd.FullCommand(),
		g.PlanCompleteCmd.FullCommand(),
		g.UpdatePlanInitCmd.FullCommand(),
		g.UpdateTriggerCmd.FullCommand(),
		g.UpgradeCmd.FullCommand():
		return true
	case g.RPCAgentRunCmd.FullCommand():
		return len(*g.RPCAgentRunCmd.Args) > 0
	case g.RPCAgentDeployCmd.FullCommand():
		return len(*g.RPCAgentDeployCmd.LeaderArgs) > 0 ||
			len(*g.RPCAgentDeployCmd.NodeArgs) > 0
	}
	return false
}

// isExpandCommand returns true if the specified command is
// expand-related command
func (g *Application) isExpandCommand(cmd string) bool {
	switch cmd {
	case g.AutoJoinCmd.FullCommand(),
		g.PlanCmd.FullCommand(),
		g.PlanDisplayCmd.FullCommand(),
		g.PlanExecuteCmd.FullCommand(),
		g.PlanRollbackCmd.FullCommand(),
		g.PlanCompleteCmd.FullCommand(),
		g.PlanResumeCmd.FullCommand():
		return true
	}
	return false
}

// findServer searches the provided cluster's state for a server that matches one of the provided
// tokens, where a token can be the server's advertise IP, hostname or AWS internal DNS name
func findServer(site ops.Site, tokens []string) (*storage.Server, error) {
	for _, server := range site.ClusterState.Servers {
		for _, token := range tokens {
			if token == "" {
				continue
			}
			switch token {
			case server.AdvertiseIP, server.Hostname, server.Nodename:
				return &server, nil
			}
		}
	}
	return nil, trace.NotFound("could not find server matching %v among registered cluster nodes",
		tokens)
}

// findLocalServer searches the provided cluster's state for the server that matches the one
// the current command is being executed from
func findLocalServer(site ops.Site) (*storage.Server, error) {
	// collect the machines's IP addresses and search by them
	ifaces, err := systeminfo.NetworkInterfaces()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if len(ifaces) == 0 {
		return nil, trace.NotFound("no network interfaces found")
	}

	var ips []string
	for _, iface := range ifaces {
		ips = append(ips, iface.IPv4)
	}

	server, err := findServer(site, ips)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return server, nil
}

func isCancelledError(err error) bool {
	if err == nil {
		return false
	}
	return trace.IsCompareFailed(err) && strings.Contains(err.Error(), "cancelled")
}

func watchReconnects(ctx context.Context, cancel context.CancelFunc, watchCh <-chan rpcserver.WatchEvent) {
	go func() {
		for event := range watchCh {
			if event.Error == nil {
				continue
			}
			log.Warnf("Failed to reconnect to %v: %v.", event.Peer, event.Error)
			cancel()
			return
		}
	}()
}

func loadRPCCredentials(ctx context.Context, addr, token string) (*rpcserver.Credentials, error) {
	// Assume addr to be a complete address if it's prefixed with `http`
	if !strings.Contains(addr, "http") {
		host, port := utils.SplitHostPort(addr, strconv.Itoa(defaults.GravitySiteNodePort))
		addr = fmt.Sprintf("https://%v:%v", host, port)
	}
	httpClient := roundtrip.HTTPClient(httplib.GetClient(true))
	packages, err := webpack.NewBearerClient(addr, token, httpClient)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	creds, err := install.LoadRPCCredentials(ctx, packages, log)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return creds, nil
}
