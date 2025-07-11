// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/armon/go-metrics"
	"github.com/openbao/openbao/api/v2"
	"github.com/openbao/openbao/helper/metricsutil"
	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/physical/raft"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/logical"
)

func (c *Core) metricsLoop(stopCh chan struct{}) {
	emitTimer := time.Tick(time.Second)

	stopOrHAState := func() (bool, consts.HAState) {
		l := newLockGrabber(c.stateLock.RLock, c.stateLock.RUnlock, stopCh)
		go l.grab()
		if stopped := l.lockOrStop(); stopped {
			return true, 0
		}
		defer c.stateLock.RUnlock()
		return false, c.HAState()
	}

	identityCountTimer := time.Tick(time.Minute * 10)
	// Only emit on active node of cluster.
	if stopped, haState := stopOrHAState(); stopped {
		return
	} else if haState == consts.Standby {
		identityCountTimer = nil
	}

	writeTimer := time.Tick(time.Second * 30)

	// This loop covers
	// vault.expire.num_leases
	// vault.core.unsealed
	// vault.identity.num_entities
	// and the non-telemetry request counters shown in the UI.
	for {
		select {
		case <-emitTimer:
			stopped, haState := stopOrHAState()
			if stopped {
				return
			}
			if haState == consts.Active {
				c.metricsMutex.Lock()
				// Emit on active node only
				if c.expiration != nil {
					c.expiration.emitMetrics()
				}
				c.metricsMutex.Unlock()
			}

			// Refresh the sealed gauge, on all nodes
			if c.Sealed() {
				c.metricSink.SetGaugeWithLabels([]string{"core", "unsealed"}, 0, nil)
			} else {
				c.metricSink.SetGaugeWithLabels([]string{"core", "unsealed"}, 1, nil)
			}

			// Refresh the standby gauge, on all nodes
			if haState != consts.Active {
				c.metricSink.SetGaugeWithLabels([]string{"core", "active"}, 0, nil)
			} else {
				c.metricSink.SetGaugeWithLabels([]string{"core", "active"}, 1, nil)
			}

			// If we're using a raft backend, emit raft metrics
			if rb, ok := c.underlyingPhysical.(*raft.RaftBackend); ok {
				rb.CollectMetrics(c.MetricSink())
			}

			// Capture the total number of in-flight requests
			c.inFlightReqGaugeMetric()

			// Refresh gauge metrics that are looped
			c.cachedGaugeMetricsEmitter()
		case <-writeTimer:
			l := newLockGrabber(c.stateLock.RLock, c.stateLock.RUnlock, stopCh)
			go l.grab()
			if stopped := l.lockOrStop(); stopped {
				return
			}
			c.stateLock.RUnlock()
		case <-identityCountTimer:
			// TODO: this can be replaced by the identity gauge counter; we need to
			// sum across all namespaces.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
				defer cancel()
				entities, err := c.countActiveEntities(ctx)
				if err != nil {
					c.logger.Error("error counting identity entities", "err", err)
				} else {
					metrics.SetGauge([]string{"identity", "num_entities"}, float32(entities.Entities.Total))
				}
			}()
		case <-stopCh:
			return
		}
	}
}

// These wrappers are responsible for redirecting to the current instance of
// TokenStore; there is one per method because an additional level of abstraction
// seems confusing.
func (c *Core) tokenGaugeCollector(ctx context.Context) ([]metricsutil.GaugeLabelValues, error) {
	// stateLock or authLock protects the tokenStore pointer
	c.stateLock.RLock()
	ts := c.tokenStore
	c.stateLock.RUnlock()
	if ts == nil {
		return []metricsutil.GaugeLabelValues{}, errors.New("nil token store")
	}
	return ts.gaugeCollector(ctx)
}

func (c *Core) tokenGaugePolicyCollector(ctx context.Context) ([]metricsutil.GaugeLabelValues, error) {
	c.stateLock.RLock()
	ts := c.tokenStore
	c.stateLock.RUnlock()
	if ts == nil {
		return []metricsutil.GaugeLabelValues{}, errors.New("nil token store")
	}
	return ts.gaugeCollectorByPolicy(ctx)
}

func (c *Core) leaseExpiryGaugeCollector(ctx context.Context) ([]metricsutil.GaugeLabelValues, error) {
	c.stateLock.RLock()
	e := c.expiration
	metricsConsts := c.MetricSink().TelemetryConsts
	c.stateLock.RUnlock()
	if e == nil {
		return []metricsutil.GaugeLabelValues{}, errors.New("nil expiration manager")
	}
	return e.leaseAggregationMetrics(ctx, metricsConsts)
}

func (c *Core) tokenGaugeMethodCollector(ctx context.Context) ([]metricsutil.GaugeLabelValues, error) {
	c.stateLock.RLock()
	ts := c.tokenStore
	c.stateLock.RUnlock()
	if ts == nil {
		return []metricsutil.GaugeLabelValues{}, errors.New("nil token store")
	}
	return ts.gaugeCollectorByMethod(ctx)
}

func (c *Core) tokenGaugeTtlCollector(ctx context.Context) ([]metricsutil.GaugeLabelValues, error) {
	c.stateLock.RLock()
	ts := c.tokenStore
	c.stateLock.RUnlock()
	if ts == nil {
		return []metricsutil.GaugeLabelValues{}, errors.New("nil token store")
	}
	return ts.gaugeCollectorByTtl(ctx)
}

// emitMetricsActiveNode is used to start all the periodic metrics; all of them should
// be shut down when stopCh is closed.  This code runs on the active node only.
func (c *Core) emitMetricsActiveNode(stopCh chan struct{}) {
	// The gauge collection processes are started and stopped here
	// because there's more than one TokenManager created during startup,
	// but we only want one set of gauges.
	metricsInit := []struct {
		MetricName    []string
		MetadataLabel []metrics.Label
		CollectorFunc metricsutil.GaugeCollector
		DisableEnvVar string
	}{
		{
			[]string{"token", "count"},
			[]metrics.Label{{Name: "gauge", Value: "token_by_namespace"}},
			c.tokenGaugeCollector,
			"",
		},
		{
			[]string{"token", "count", "by_policy"},
			[]metrics.Label{{Name: "gauge", Value: "token_by_policy"}},
			c.tokenGaugePolicyCollector,
			"",
		},
		{
			[]string{"expire", "leases", "by_expiration"},
			[]metrics.Label{{Name: "gauge", Value: "leases_by_expiration"}},
			c.leaseExpiryGaugeCollector,
			"",
		},
		{
			[]string{"token", "count", "by_auth"},
			[]metrics.Label{{Name: "gauge", Value: "token_by_auth"}},
			c.tokenGaugeMethodCollector,
			"",
		},
		{
			[]string{"token", "count", "by_ttl"},
			[]metrics.Label{{Name: "gauge", Value: "token_by_ttl"}},
			c.tokenGaugeTtlCollector,
			"",
		},
		{
			[]string{"secret", "kv", "count"},
			[]metrics.Label{{Name: "gauge", Value: "kv_secrets_by_mountpoint"}},
			c.kvSecretGaugeCollector,
			"BAO_DISABLE_KV_GAUGE",
		},
		{
			[]string{"identity", "entity", "count"},
			[]metrics.Label{{Name: "gauge", Value: "identity_by_namespace"}},
			c.entityGaugeCollector,
			"",
		},
		{
			[]string{"identity", "entity", "alias", "count"},
			[]metrics.Label{{Name: "gauge", Value: "identity_by_mountpoint"}},
			c.entityGaugeCollectorByMount,
			"",
		},
	}

	// Disable collection if configured.
	if c.MetricSink().GaugeInterval == time.Duration(0) {
		c.logger.Info("usage gauge collection is disabled")
	} else if standby, _ := c.Standby(); !standby {
		for _, init := range metricsInit {
			if init.DisableEnvVar != "" {
				if api.ReadBaoVariable(init.DisableEnvVar) != "" {
					c.logger.Info("usage gauge collection is disabled for",
						"metric", init.MetricName)
					continue
				}
			}

			proc, err := c.MetricSink().NewGaugeCollectionProcess(
				init.MetricName,
				init.MetadataLabel,
				init.CollectorFunc,
				c.logger,
			)
			if err != nil {
				c.logger.Error("failed to start collector", "metric", init.MetricName, "error", err)
			} else {
				go proc.Run()
				defer proc.Stop()
			}
		}
	}

	// When this returns, all the defers set up above will fire.
	c.metricsLoop(stopCh)
}

type kvMount struct {
	Namespace  *namespace.Namespace
	MountPoint string
	Version    string
	NumSecrets int
}

func (c *Core) findKvMounts() []*kvMount {
	mounts := make([]*kvMount, 0)

	c.mountsLock.RLock()
	defer c.mountsLock.RUnlock()

	// we don't grab the statelock, so this code might run during or after the seal process.
	// Therefore, we need to check if c.mounts is nil. If we do not, this will panic when
	// run after seal.
	if c.mounts == nil {
		return mounts
	}

	for _, entry := range c.mounts.Entries {
		if entry.Type == "kv" || entry.Type == "generic" {
			version, ok := entry.Options["version"]
			if !ok {
				version = "1"
			}
			mounts = append(mounts, &kvMount{
				Namespace:  entry.namespace,
				MountPoint: entry.Path,
				Version:    version,
				NumSecrets: 0,
			})
		}
	}
	return mounts
}

func (c *Core) kvCollectionErrorCount() {
	c.MetricSink().IncrCounterWithLabels(
		[]string{"metrics", "collection", "error"},
		1,
		[]metrics.Label{{Name: "gauge", Value: "kv_secrets_by_mountpoint"}},
	)
}

func (c *Core) walkKvMountSecrets(ctx context.Context, m *kvMount) {
	var subdirectories []string
	if m.Version == "1" {
		subdirectories = []string{m.Namespace.Path + m.MountPoint}
	} else {
		subdirectories = []string{m.Namespace.Path + m.MountPoint + "metadata/"}
	}

	for len(subdirectories) > 0 {
		// Check for cancellation
		select {
		case <-ctx.Done():
			return
		default:
		}

		currentDirectory := subdirectories[0]
		subdirectories = subdirectories[1:]

		listRequest := &logical.Request{
			Operation: logical.ListOperation,
			Path:      currentDirectory,
		}
		resp, err := c.router.Route(ctx, listRequest)
		if err != nil {
			c.kvCollectionErrorCount()
			// ErrUnsupportedPath probably means that the mount is not there any more,
			// don't log those cases.
			if !strings.Contains(err.Error(), logical.ErrUnsupportedPath.Error()) {
				c.logger.Error("failed to perform internal KV list", "mount_point", m.MountPoint, "error", err)
				break
			}
			// Quit handling this mount point (but it'll still appear in the list)
			return
		}
		if resp == nil {
			continue
		}
		rawKeys, ok := resp.Data["keys"]
		if !ok {
			continue
		}
		keys, ok := rawKeys.([]string)
		if !ok {
			c.kvCollectionErrorCount()
			c.logger.Error("KV list keys are not a []string", "mount_point", m.MountPoint, "rawKeys", rawKeys)
			// Quit handling this mount point (but it'll still appear in the list)
			return
		}
		for _, path := range keys {
			if len(path) > 0 && path[len(path)-1] == '/' {
				subdirectories = append(subdirectories, currentDirectory+path)
			} else {
				m.NumSecrets += 1
			}
		}
	}
}

func (c *Core) kvSecretGaugeCollector(ctx context.Context) ([]metricsutil.GaugeLabelValues, error) {
	// Find all KV mounts
	mounts := c.findKvMounts()
	results := make([]metricsutil.GaugeLabelValues, len(mounts))

	// Use a root namespace, so include namespace path
	// in any queries.
	ctx = namespace.RootContext(ctx)

	// Route list requests to all the identified mounts.
	// (All of these will show up as activity in the vault.route metric.)
	// Then we have to explore each subdirectory.
	for i, m := range mounts {
		// Check for cancellation, return empty array
		select {
		case <-ctx.Done():
			return []metricsutil.GaugeLabelValues{}, nil
		default:
		}

		results[i].Labels = []metrics.Label{
			metricsutil.NamespaceLabel(m.Namespace),
			{Name: "mount_point", Value: m.MountPoint},
		}

		c.walkKvMountSecrets(ctx, m)
		results[i].Value = float32(m.NumSecrets)
	}

	return results, nil
}

func (c *Core) entityGaugeCollector(ctx context.Context) ([]metricsutil.GaugeLabelValues, error) {
	// Protect against concurrent changes during seal
	c.stateLock.RLock()
	identityStore := c.identityStore
	c.stateLock.RUnlock()
	if identityStore == nil {
		return []metricsutil.GaugeLabelValues{}, errors.New("nil identity store")
	}

	byNamespace, err := identityStore.countEntitiesByNamespace(ctx)
	if err != nil {
		return []metricsutil.GaugeLabelValues{}, err
	}

	// No check for expiration here; the bulk of the work should be in
	// counting the entities.
	allNamespaces, err := c.namespaceStore.ListAllNamespaces(ctx, true)
	if err != nil {
		return []metricsutil.GaugeLabelValues{}, err
	}

	values := make([]metricsutil.GaugeLabelValues, len(allNamespaces))
	for i := range values {
		values[i].Labels = []metrics.Label{
			metricsutil.NamespaceLabel(allNamespaces[i]),
		}
		values[i].Value = float32(byNamespace[allNamespaces[i].ID])
	}

	return values, nil
}

func (c *Core) entityGaugeCollectorByMount(ctx context.Context) ([]metricsutil.GaugeLabelValues, error) {
	c.stateLock.RLock()
	identityStore := c.identityStore
	c.stateLock.RUnlock()
	if identityStore == nil {
		return []metricsutil.GaugeLabelValues{}, errors.New("nil identity store")
	}

	byAccessor, err := identityStore.countEntitiesByMountAccessor(ctx)
	if err != nil {
		return []metricsutil.GaugeLabelValues{}, err
	}

	values := make([]metricsutil.GaugeLabelValues, 0)
	for accessor, count := range byAccessor {
		// Terminate if taking too long to do the translation
		select {
		case <-ctx.Done():
			return values, errors.New("context cancelled")
		default:
		}

		c.stateLock.RLock()
		mountEntry := c.router.MatchingMountByAccessor(accessor)
		c.stateLock.RUnlock()
		if mountEntry == nil {
			continue
		}
		values = append(values, metricsutil.GaugeLabelValues{
			Labels: []metrics.Label{
				metricsutil.NamespaceLabel(mountEntry.namespace),
				{Name: "auth_method", Value: mountEntry.Type},
				{Name: "mount_point", Value: "auth/" + mountEntry.Path},
			},
			Value: float32(count),
		})
	}

	return values, nil
}

func (c *Core) cachedGaugeMetricsEmitter() {
	if c.metricsHelper == nil {
		return
	}

	loopMetrics := &c.metricsHelper.LoopMetrics.Metrics

	emit := func(key interface{}, value interface{}) bool {
		metricValue := value.(metricsutil.GaugeMetric)
		c.metricSink.SetGaugeWithLabels(metricValue.Key, metricValue.Value, metricValue.Labels)
		return true
	}

	loopMetrics.Range(emit)
}

func (c *Core) inFlightReqGaugeMetric() {
	totalInFlightReq := c.inFlightReqData.InFlightReqCount.Load()
	// Adding a gauge metric to capture total number of inflight requests
	c.metricSink.SetGaugeWithLabels([]string{"core", "in_flight_requests"}, float32(totalInFlightReq), nil)
}
