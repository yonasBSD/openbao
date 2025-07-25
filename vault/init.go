// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sync/atomic"
	"time"

	uuid "github.com/hashicorp/go-uuid"
	wrapping "github.com/openbao/go-kms-wrapping/v2"
	"github.com/openbao/openbao/physical/raft"
	"github.com/openbao/openbao/vault/seal"

	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/helper/pgpkeys"
	"github.com/openbao/openbao/sdk/v2/helper/shamir"
)

// InitParams keeps the init function from being littered with too many
// params, that's it!
type InitParams struct {
	BarrierConfig   *SealConfig
	RecoveryConfig  *SealConfig
	RootTokenPGPKey string
}

// InitResult is used to provide the key parts back after
// they are generated as part of the initialization.
type InitResult struct {
	SecretShares   [][]byte
	RecoveryShares [][]byte
	RootToken      string
}

var initInProgress uint32

func (c *Core) InitializeRecovery(ctx context.Context) error {
	if !c.recoveryMode {
		return nil
	}

	raftStorage, ok := c.underlyingPhysical.(*raft.RaftBackend)
	if !ok {
		return nil
	}

	parsedClusterAddr, err := url.Parse(c.ClusterAddr())
	if err != nil {
		return err
	}

	c.postRecoveryUnsealFuncs = append(c.postRecoveryUnsealFuncs, func() error {
		return raftStorage.StartRecoveryCluster(context.Background(), raft.Peer{
			ID:      raftStorage.NodeID(),
			Address: parsedClusterAddr.Host,
		})
	})

	return nil
}

// Initialized checks if the Vault is already initialized.  This means one of
// two things: either the barrier has been created (with keyring and root key)
// and the seal config written to storage, or Raft is forming a cluster and a
// join/bootstrap is in progress.
func (c *Core) Initialized(ctx context.Context) (bool, error) {
	// Check the barrier first
	init, err := c.InitializedLocally(ctx)
	if err != nil || init {
		return init, err
	}

	if c.isRaftUnseal() {
		return true, nil
	}

	rb := c.getRaftBackend()
	if rb != nil && rb.Initialized() {
		return true, nil
	}

	return false, nil
}

// InitializedLocally checks if the Vault is already initialized from the
// local node's perspective.  This is the same thing as Initialized, unless
// using Raft, in which case Initialized may return true (because a peer
// we're joining to has been initialized) while InitializedLocally returns
// false (because we're not done bootstrapping raft on the local node).
func (c *Core) InitializedLocally(ctx context.Context) (bool, error) {
	// Check the barrier first
	init, err := c.barrier.Initialized(ctx)
	if err != nil {
		c.logger.Error("barrier init check failed", "error", err)
		return false, err
	}
	if !init {
		c.logger.Info("security barrier not initialized")
		return false, nil
	}

	// Verify the seal configuration
	sealConf, err := c.seal.BarrierConfig(ctx)
	if err != nil {
		return false, err
	}
	if sealConf == nil {
		return false, errors.New("core: barrier reports initialized but no seal configuration found")
	}

	return true, nil
}

func (c *Core) generateShares(sc *SealConfig) ([]byte, [][]byte, error) {
	// Generate a root key
	rootKey, err := c.barrier.GenerateKey(c.secureRandomReader)
	if err != nil {
		return nil, nil, fmt.Errorf("key generation failed: %w", err)
	}

	// Return the root key if only a single key part is used
	var unsealKeys [][]byte
	if sc.SecretShares == 1 {
		unsealKeys = append(unsealKeys, rootKey)
	} else {
		// Split the root key using the Shamir algorithm
		shares, err := shamir.Split(rootKey, sc.SecretShares, sc.SecretThreshold)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to generate barrier shares: %w", err)
		}
		unsealKeys = shares
	}

	// If we have PGP keys, perform the encryption
	if len(sc.PGPKeys) > 0 {
		hexEncodedShares := make([][]byte, len(unsealKeys))
		for i := range unsealKeys {
			hexEncodedShares[i] = []byte(hex.EncodeToString(unsealKeys[i]))
		}
		_, encryptedShares, err := pgpkeys.EncryptShares(hexEncodedShares, sc.PGPKeys)
		if err != nil {
			return nil, nil, err
		}
		unsealKeys = encryptedShares
	}

	return rootKey, unsealKeys, nil
}

// Initialize is used to initialize the Vault with the given
// configurations.
func (c *Core) Initialize(ctx context.Context, initParams *InitParams) (*InitResult, error) {
	results, err := c.initializeInternal(ctx, initParams)
	if err != nil {
		return results, err
	}

	if c.seal.RecoveryKeySupported() {
		recoveryConfig, err := c.seal.RecoveryConfig(ctx)
		if err != nil {
			// Discard the error; we know we initialized successfully.
			c.logger.Error("failed to read recovery configuration", "err", err)
			return results, nil
		}

		if recoveryConfig.SecretShares == 0 {
			// When we have no recovery keys generated on an auto-unseal
			// mechanism, the caller has no option but to restart the service.
			// This ensures we can start using the node immediately.
			if err := c.UnsealWithStoredKeys(ctx); err != nil {
				// Discard this error; caller will need the root key to debug.
				c.logger.Error("failed to automatically unseal after initialization with no recovery keys", "err", err)
				return results, nil
			}
		}
	}

	return results, nil
}

func (c *Core) initializeInternal(ctx context.Context, initParams *InitParams) (*InitResult, error) {
	atomic.StoreUint32(&initInProgress, 1)
	defer atomic.StoreUint32(&initInProgress, 0)
	barrierConfig := initParams.BarrierConfig
	recoveryConfig := initParams.RecoveryConfig

	// N.B. Although the core is capable of handling situations where some keys
	// are stored and some aren't, in practice, replication + HSMs makes this
	// extremely hard to reason about, to the point that it will probably never
	// be supported. The reason is that each HSM needs to encode the root key
	// separately, which means the shares must be generated independently,
	// which means both that the shares will be different *AND* there would
	// need to be a way to actually allow fetching of the generated keys by
	// operators.
	if c.SealAccess().StoredKeysSupported() == seal.StoredKeysSupportedGeneric {
		if len(barrierConfig.PGPKeys) > 0 {
			return nil, errors.New("PGP keys not supported when storing shares")
		}
		barrierConfig.SecretShares = 1
		barrierConfig.SecretThreshold = 1
		if barrierConfig.StoredShares != 1 {
			c.Logger().Warn("stored keys supported on init, forcing shares/threshold to 1")
		}
	}

	barrierConfig.StoredShares = 1

	// Check if the seal configuration is valid
	if err := barrierConfig.Validate(); err != nil {
		c.logger.Error("invalid seal configuration", "error", err)
		return nil, fmt.Errorf("invalid seal configuration: %w", err)
	}

	if c.seal.RecoveryKeySupported() {
		if recoveryConfig == nil {
			return nil, errors.New("recovery configuration must be supplied")
		}

		// Check if the seal configuration is valid
		if err := recoveryConfig.ValidateRecovery(); err != nil {
			c.logger.Error("invalid recovery configuration", "error", err)
			return nil, fmt.Errorf("invalid recovery configuration: %w", err)
		}
	}

	// Avoid an initialization race on the local node.
	c.stateLock.Lock()
	defer c.stateLock.Unlock()

	// Check if we are initialized
	init, err := c.Initialized(ctx)
	if err != nil {
		return nil, err
	}
	if init {
		return nil, ErrAlreadyInit
	}

	// Bootstrap the raft backend if that's provided as the physical or
	// HA backend.
	raftBackend := c.getRaftBackend()
	if raftBackend != nil {
		err := c.RaftBootstrap(ctx, true)
		if err != nil {
			c.logger.Error("failed to bootstrap raft", "error", err)
			return nil, err
		}

		// Teardown cluster after bootstrap setup
		defer func() {
			if err := raftBackend.TeardownCluster(nil); err != nil {
				c.logger.Error("failed to stop raft", "error", err)
			}
		}()
	}

	// If we have a HA backend, acquire an initialization lock. This type of
	// race condition is possible on some storage backends which allow
	// multiple writers, such as PostgreSQL, with declarative initialization
	// if multiple nodes come up at the same exact time.
	if c.ha != nil {
		// Create a lock
		uuid, err := uuid.GenerateUUID()
		if err != nil {
			return nil, fmt.Errorf("failed to generate uuid: %w", err)
		}
		lock, err := c.ha.LockWith(CoreInitLockPath, uuid)
		if err != nil {
			return nil, fmt.Errorf("failed to create initialization lock: %w", err)
		}

		stopCh := make(chan struct{})
		go func() {
			// Add a timeout for lock acquisition: if multiple nodes race to
			// acquire this lock, we wish to stop the losing nodes rather than
			// hang indefinitely. One initialization is sufficient in this
			// case.
			time.Sleep(5 * time.Second)
			stopCh <- struct{}{}
		}()

		// This lock should not be held long enough to trigger a loss of
		// leadership so ignore the loss channel.
		_, err = lock.Lock(stopCh)
		if err != nil {
			c.logger.Error("got error acquiring lock", "error", err)
			return nil, ErrParallelInit
		}

		// Check if we are initialized again after acquiring this lock.
		init, err = c.InitializedLocally(ctx)
		if err != nil {
			return nil, err
		}
		if init {
			return nil, ErrAlreadyInit
		}

		c.logger.Info("acquired HA initialization lock")

		defer func() {
			if err := lock.Unlock(); err != nil {
				c.logger.Error("failed to unlock initialization lock", "error", err)
			}
		}()
	}

	err = c.seal.Init(ctx)
	if err != nil {
		c.logger.Error("failed to initialize seal", "error", err)
		return nil, fmt.Errorf("error initializing seal: %w", err)
	}

	barrierKey, barrierKeyShares, err := c.generateShares(barrierConfig)
	if err != nil {
		c.logger.Error("error generating shares", "error", err)
		return nil, err
	}

	var sealKey []byte
	var sealKeyShares [][]byte

	if barrierConfig.StoredShares == 1 && c.seal.BarrierType() == wrapping.WrapperTypeShamir {
		sealKey, sealKeyShares, err = c.generateShares(barrierConfig)
		if err != nil {
			c.logger.Error("error generating shares", "error", err)
			return nil, err
		}
	}

	// Initialize the barrier
	if err := c.barrier.Initialize(ctx, barrierKey, sealKey, c.secureRandomReader); err != nil {
		c.logger.Error("failed to initialize barrier", "error", err)
		return nil, fmt.Errorf("failed to initialize barrier: %w", err)
	}
	if c.logger.IsInfo() {
		c.logger.Info("security barrier initialized", "stored", barrierConfig.StoredShares, "shares", barrierConfig.SecretShares, "threshold", barrierConfig.SecretThreshold)
	}

	// Unseal the barrier
	if err := c.barrier.Unseal(ctx, barrierKey); err != nil {
		c.logger.Error("failed to unseal barrier", "error", err)
		return nil, fmt.Errorf("failed to unseal barrier: %w", err)
	}

	// Ensure the barrier is re-sealed
	defer func() {
		// Defers are LIFO so we need to run this here too to ensure the stop
		// happens before sealing. preSeal also stops, so we just make the
		// stopping safe against multiple calls.
		if err := c.barrier.Seal(); err != nil {
			c.logger.Error("failed to seal barrier", "error", err)
		}
	}()

	err = c.seal.SetBarrierConfig(ctx, barrierConfig)
	if err != nil {
		c.logger.Error("failed to save barrier configuration", "error", err)
		return nil, fmt.Errorf("barrier configuration saving failed: %w", err)
	}

	results := &InitResult{
		SecretShares: [][]byte{},
	}

	// If we are storing shares, pop them out of the returned results and push
	// them through the seal
	switch c.seal.StoredKeysSupported() {
	case seal.StoredKeysSupportedShamirRoot:
		keysToStore := [][]byte{barrierKey}
		shamirWrapper, err := c.seal.GetShamirWrapper()
		if err != nil {
			return nil, err
		}
		if err := shamirWrapper.SetAesGcmKeyBytes(sealKey); err != nil {
			c.logger.Error("failed to set seal key", "error", err)
			return nil, fmt.Errorf("failed to set seal key: %w", err)
		}
		if err := c.seal.SetStoredKeys(ctx, keysToStore); err != nil {
			c.logger.Error("failed to store keys", "error", err)
			return nil, fmt.Errorf("failed to store keys: %w", err)
		}
		results.SecretShares = sealKeyShares
	case seal.StoredKeysSupportedGeneric:
		keysToStore := [][]byte{barrierKey}
		if err := c.seal.SetStoredKeys(ctx, keysToStore); err != nil {
			c.logger.Error("failed to store keys", "error", err)
			return nil, fmt.Errorf("failed to store keys: %w", err)
		}
	default:
		// We don't support initializing an old-style Shamir seal anymore, so
		// this case is only reachable by tests.
		results.SecretShares = barrierKeyShares
	}

	// Perform initial setup
	if err := c.setupCluster(ctx); err != nil {
		c.logger.Error("cluster setup failed during init", "error", err)
		return nil, err
	}

	activeCtx, ctxCancel := context.WithCancel(namespace.RootContext(nil))
	if err := c.postUnseal(activeCtx, ctxCancel, standardUnsealStrategy{}); err != nil {
		c.logger.Error("post-unseal setup failed during init", "error", err)
		return nil, err
	}

	// Save the configuration regardless, but only generate a key if it's not
	// disabled. When using recovery keys they are stored in the barrier, so
	// this must happen post-unseal.
	if c.seal.RecoveryKeySupported() {
		err = c.seal.SetRecoveryConfig(ctx, recoveryConfig)
		if err != nil {
			c.logger.Error("failed to save recovery configuration", "error", err)
			return nil, fmt.Errorf("recovery configuration saving failed: %w", err)
		}

		if recoveryConfig.SecretShares > 0 {
			recoveryKey, recoveryUnsealKeys, err := c.generateShares(recoveryConfig)
			if err != nil {
				c.logger.Error("failed to generate recovery shares", "error", err)
				return nil, err
			}

			err = c.seal.SetRecoveryKey(ctx, recoveryKey)
			if err != nil {
				return nil, err
			}

			results.RecoveryShares = recoveryUnsealKeys
		}
	}

	// Generate a new root token
	rootToken, err := c.tokenStore.rootToken(ctx)
	if err != nil {
		c.logger.Error("root token generation failed", "error", err)
		return nil, err
	}
	results.RootToken = rootToken.ExternalID
	c.logger.Info("root token generated")

	if initParams.RootTokenPGPKey != "" {
		_, encryptedVals, err := pgpkeys.EncryptShares([][]byte{[]byte(results.RootToken)}, []string{initParams.RootTokenPGPKey})
		if err != nil {
			c.logger.Error("root token encryption failed", "error", err)
			return nil, err
		}
		results.RootToken = base64.StdEncoding.EncodeToString(encryptedVals[0])
	}

	if raftBackend != nil {
		if _, err := c.raftCreateTLSKeyring(ctx); err != nil {
			c.logger.Error("failed to create raft TLS keyring", "error", err)
			return nil, err
		}
	}

	// Prepare to re-seal
	if err := c.preSeal(); err != nil {
		c.logger.Error("pre-seal teardown failed", "error", err)
		return nil, err
	}

	if c.serviceRegistration != nil {
		if err := c.serviceRegistration.NotifyInitializedStateChange(true); err != nil {
			if c.logger.IsWarn() {
				c.logger.Warn("notification of initialization failed", "error", err)
			}
		}
	}

	return results, nil
}

// UnsealWithStoredKeys performs auto-unseal using stored keys. An error
// return value of "nil" implies the Vault instance is unsealed.
//
// Callers should attempt to retry any NonFatalErrors. Callers should
// not re-attempt fatal errors.
func (c *Core) UnsealWithStoredKeys(ctx context.Context) error {
	c.unsealWithStoredKeysLock.Lock()
	defer c.unsealWithStoredKeysLock.Unlock()

	if c.seal.BarrierType() == wrapping.WrapperTypeShamir {
		return nil
	}

	// Disallow auto-unsealing when migrating
	if c.IsInSealMigrationMode(true) && !c.IsSealMigrated(true) {
		return NewNonFatalError(errors.New("cannot auto-unseal during seal migration"))
	}

	c.stateLock.Lock()
	defer c.stateLock.Unlock()

	sealed := c.Sealed()
	if !sealed {
		c.Logger().Warn("attempted unseal with stored keys, but vault is already unsealed")
		return nil
	}

	c.Logger().Info("stored unseal keys supported, attempting fetch")
	keys, err := c.seal.GetStoredKeys(ctx)
	if err != nil {
		return NewNonFatalError(fmt.Errorf("fetching stored unseal keys failed: %w", err))
	}

	// This usually happens when auto-unseal is configured, but the servers have
	// not been initialized yet.
	if len(keys) == 0 {
		return NewNonFatalError(errors.New("stored unseal keys are supported, but none were found: is the server initialized?"))
	}
	if len(keys) != 1 {
		return NewNonFatalError(errors.New("expected exactly one stored key"))
	}

	err = c.unsealInternal(ctx, keys[0])
	if err != nil {
		return NewNonFatalError(fmt.Errorf("unseal with stored key failed: %w", err))
	}

	if c.Sealed() {
		// This most likely means that the user configured Vault to only store a
		// subset of the required threshold of keys. We still consider this a
		// "success", since trying again would yield the same result.
		c.Logger().Warn("vault still sealed after using stored unseal key")
	} else {
		c.Logger().Info("unsealed with stored key")
	}

	return nil
}
