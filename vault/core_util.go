// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"

	"github.com/openbao/openbao/sdk/v2/physical"
)

func coreInit(c *Core, conf *CoreConfig) error {
	phys := conf.Physical
	// Wrap the physical backend in a cache layer if enabled
	cacheLogger := c.baseLogger.Named("storage.cache")
	c.allLoggers = append(c.allLoggers, cacheLogger)
	c.physical = physical.NewCache(phys, conf.CacheSize, cacheLogger, c.MetricSink().Sink)
	c.physicalCache = c.physical.(physical.ToggleablePurgemonster)

	// Wrap in encoding checks
	if !conf.DisableKeyEncodingChecks {
		c.physical = physical.NewStorageEncoding(c.physical)
	}

	return nil
}

func preSealPhysical(c *Core) {
	// Purge the cache
	c.physicalCache.SetEnabled(false)
	c.physicalCache.Purge(context.Background())
}

func postUnsealPhysical(c *Core) error {
	return nil
}
