// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package allocrunner

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	cni "github.com/containerd/go-cni"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

const (
	// defaultCNIPath is the CNI path to use when it is not set by the client
	// and is not set by environment variable
	defaultCNIPath = "/opt/cni/bin"

	// defaultCNIInterfacePrefix is the network interface to use if not set in
	// client config
	defaultCNIInterfacePrefix = "eth"
)

type cniNetworkConfigurator struct {
	cni                     cni.CNI
	confParser              *cniConfParser
	ignorePortMappingHostIP bool
	nodeAttrs               map[string]string
	nodeMeta                map[string]string
	rand                    *rand.Rand
	logger                  log.Logger
	nsOpts                  *nsOpts
}

// FixMe: Instead of this, just handle cniNetworkConfigurator differently
func newCNINetworkConfiguratorWithConf(logger log.Logger, cniPath, cniInterfacePrefix string, ignorePortMappingHostIP bool, parser *cniConfParser, node *structs.Node) (*cniNetworkConfigurator, error) {
	conf := &cniNetworkConfigurator{
		confParser:              parser,
		rand:                    rand.New(rand.NewSource(time.Now().Unix())),
		logger:                  logger,
		ignorePortMappingHostIP: ignorePortMappingHostIP,
		nodeAttrs:               node.Attributes,
		nodeMeta:                node.Meta,
		nsOpts:                  &nsOpts{},
	}
	if cniPath == "" {
		if cniPath = os.Getenv(envCNIPath); cniPath == "" {
			cniPath = defaultCNIPath
		}
	}

	if cniInterfacePrefix == "" {
		cniInterfacePrefix = defaultCNIInterfacePrefix
	}

	c, err := cni.New(cni.WithPluginDir(filepath.SplitList(cniPath)),
		cni.WithInterfacePrefix(cniInterfacePrefix))
	if err != nil {
		return nil, err
	}
	conf.cni = c

	return conf, nil
}

// FixMe: We should not need this in Windows
const (
	ConsulIPTablesConfigEnvVar = "CONSUL_IPTABLES_CONFIG"
)

// Teardown calls the CNI plugins with the delete action
func (c *cniNetworkConfigurator) Teardown(ctx context.Context, alloc *structs.Allocation, spec *drivers.NetworkIsolationSpec) error {
	if err := c.ensureCNIInitialized(); err != nil {
		return err
	}

	portMap := getPortMapping(alloc, c.ignorePortMappingHostIP)

	if err := c.cni.Remove(ctx, alloc.ID, spec.Path, cni.WithCapabilityPortMap(portMap.ports)); err != nil {
		c.logger.Warn("error from cni.Remove; attempting manual iptables cleanup", "err", err)
	}

	return nil
}
