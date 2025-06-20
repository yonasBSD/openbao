// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armon/go-metrics"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/go-raftchunking"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/hashicorp/raft"
	"github.com/openbao/openbao/sdk/v2/helper/jsonutil"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/sdk/v2/plugin/pb"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

const (
	deleteOp uint32 = 1 << iota
	putOp
	restoreCallbackOp
	getOp
	verifyReadOp
	verifyListOp
	beginTxOp
	commitTxOp

	chunkingPrefix   = "raftchunking/"
	databaseFilename = "vault.db"
)

var (
	// dataBucketName is the value we use for the bucket
	dataBucketName     = []byte("data")
	configBucketName   = []byte("config")
	latestIndexKey     = []byte("latest_indexes")
	latestConfigKey    = []byte("latest_config")
	localNodeConfigKey = []byte("local_node_config")
)

// Verify FSM satisfies the correct interfaces
var (
	_ physical.Backend = (*FSM)(nil)
	_ raft.FSM         = (*FSM)(nil)
	_ raft.BatchingFSM = (*FSM)(nil)
)

type restoreCallback func(context.Context) error

// fsmEntryTxErrorKey is the value for a FSMEntry to signal that it is
// not the result of a regular Get operation (which cannot occur in a
// transaction) but is instead contains the response value from failing to
// apply this transaction.
const fsmEntryTxErrorKey = "[\ttransaction-commit-failure\t]"

type FSMEntry struct {
	Key   string
	Value []byte
}

func (f *FSMEntry) String() string {
	return fmt.Sprintf("Key: %s. Value: %s", f.Key, hex.EncodeToString(f.Value))
}

func (f *FSMEntry) IsTxError() bool {
	return f.Key == fsmEntryTxErrorKey
}

func (f *FSMEntry) AsTxError() error {
	str := string(f.Value)
	commitErr := physical.ErrTransactionCommitFailure.Error()

	split := strings.SplitN(str, commitErr, 2)
	if len(split) != 2 {
		return errors.New(str)
	}

	return fmt.Errorf("%v%w%v", split[0], physical.ErrTransactionCommitFailure, split[1])
}

// FSMApplyResponse is returned from an FSM apply. It indicates if the apply was
// successful or not. EntryMap contains the keys/values from the Get operations.
type FSMApplyResponse struct {
	Success    bool
	EntrySlice []*FSMEntry
}

// FSM is Vault's primary state storage. It writes updates to a bolt db file
// that lives on local disk. FSM implements raft.FSM and physical.Backend
// interfaces.
type FSM struct {
	// latestIndex and latestTerm must stay at the top of this struct to be
	// properly 64-bit aligned.

	// latestIndex and latestTerm are the term and index of the last log we
	// received
	latestIndex *uint64
	latestTerm  *uint64
	// latestConfig is the latest server configuration we've seen
	latestConfig atomic.Value

	l           sync.RWMutex
	path        string
	logger      log.Logger
	noopRestore bool

	// applyCallback is used to control the pace of applies in tests
	applyCallback func()

	db *bolt.DB

	// retoreCb is called after we've restored a snapshot
	restoreCb restoreCallback

	chunker *raftchunking.ChunkingBatchingFSM

	localID         string
	desiredSuffrage string
	unknownOpTypes  sync.Map

	// tracker for fast application of transactions
	fastTxnTracker *fsmTxnCommitIndexTracker
}

// NewFSM constructs a FSM using the given directory
func NewFSM(path string, localID string, logger log.Logger) (*FSM, error) {
	// Initialize the latest term, index, and config values
	latestTerm := new(uint64)
	latestIndex := new(uint64)
	latestConfig := atomic.Value{}
	atomic.StoreUint64(latestTerm, 0)
	atomic.StoreUint64(latestIndex, 0)
	latestConfig.Store((*ConfigurationValue)(nil))

	f := &FSM{
		path:   path,
		logger: logger,

		latestTerm:   latestTerm,
		latestIndex:  latestIndex,
		latestConfig: latestConfig,
		// Assume that the default intent is to join as as voter. This will be updated
		// when this node joins a cluster with a different suffrage, or during cluster
		// setup if this is already part of a cluster with a desired suffrage.
		desiredSuffrage: "voter",
		localID:         localID,
		fastTxnTracker:  FsmTxnCommitIndexTracker(),
	}

	f.chunker = raftchunking.NewChunkingBatchingFSM(f, &FSMChunkStorage{
		f:   f,
		ctx: context.Background(),
	})

	dbPath := filepath.Join(path, databaseFilename)
	f.l.Lock()
	defer f.l.Unlock()
	if err := f.openDBFile(dbPath); err != nil {
		return nil, fmt.Errorf("failed to open bolt file: %w", err)
	}

	return f, nil
}

func (f *FSM) getDB() *bolt.DB {
	f.l.RLock()
	defer f.l.RUnlock()

	return f.db
}

// SetFSMDelay adds a delay to the FSM apply. This is used in tests to simulate
// a slow apply.
func (r *RaftBackend) SetFSMDelay(delay time.Duration) {
	r.SetFSMApplyCallback(func() { time.Sleep(delay) })
}

func (r *RaftBackend) SetFSMApplyCallback(f func()) {
	r.fsm.l.Lock()
	r.fsm.applyCallback = f
	r.fsm.l.Unlock()
}

func (f *FSM) openDBFile(dbPath string) error {
	if len(dbPath) == 0 {
		return errors.New("can not open empty filename")
	}

	st, err := os.Stat(dbPath)
	switch {
	case err != nil && os.IsNotExist(err):
	case err != nil:
		return fmt.Errorf("error checking raft FSM db file %q: %v", dbPath, err)
	default:
		perms := st.Mode() & os.ModePerm
		if perms&0o077 != 0 {
			f.logger.Warn("raft FSM db file has wider permissions than needed",
				"needed", os.FileMode(0o600), "existing", perms)
		}
	}

	opts := boltOptions(dbPath)
	start := time.Now()
	boltDB, err := bolt.Open(dbPath, 0o600, opts)
	if err != nil {
		return err
	}
	elapsed := time.Now().Sub(start)
	f.logger.Debug("time to open database", "elapsed", elapsed, "path", dbPath)
	metrics.MeasureSince([]string{"raft_storage", "fsm", "open_db_file"}, start)

	err = boltDB.Update(func(tx *bolt.Tx) error {
		// make sure we have the necessary buckets created
		_, err := tx.CreateBucketIfNotExists(dataBucketName)
		if err != nil {
			return fmt.Errorf("failed to create bucket: %v", err)
		}
		b, err := tx.CreateBucketIfNotExists(configBucketName)
		if err != nil {
			return fmt.Errorf("failed to create bucket: %v", err)
		}

		// Read in our latest index and term and populate it inmemory
		val := b.Get(latestIndexKey)
		if val != nil {
			var latest IndexValue
			err := proto.Unmarshal(val, &latest)
			if err != nil {
				return err
			}

			atomic.StoreUint64(f.latestTerm, latest.Term)
			atomic.StoreUint64(f.latestIndex, latest.Index)
		}

		// Read in our latest config and populate it inmemory
		val = b.Get(latestConfigKey)
		if val != nil {
			var latest ConfigurationValue
			err := proto.Unmarshal(val, &latest)
			if err != nil {
				return err
			}

			f.latestConfig.Store(&latest)
		}
		return nil
	})
	if err != nil {
		return err
	}

	f.db = boltDB
	return nil
}

func (f *FSM) Stats() bolt.Stats {
	f.l.RLock()
	defer f.l.RUnlock()

	return f.db.Stats()
}

func (f *FSM) Close() error {
	f.l.RLock()
	defer f.l.RUnlock()

	return f.db.Close()
}

func writeSnapshotMetaToDB(metadata *raft.SnapshotMeta, db *bolt.DB) error {
	latestIndex := &IndexValue{
		Term:  metadata.Term,
		Index: metadata.Index,
	}
	indexBytes, err := proto.Marshal(latestIndex)
	if err != nil {
		return err
	}

	protoConfig := raftConfigurationToProtoConfiguration(metadata.ConfigurationIndex, metadata.Configuration)
	configBytes, err := proto.Marshal(protoConfig)
	if err != nil {
		return err
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(configBucketName)
		if err != nil {
			return err
		}

		err = b.Put(latestConfigKey, configBytes)
		if err != nil {
			return err
		}

		err = b.Put(latestIndexKey, indexBytes)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (f *FSM) localNodeConfig() (*LocalNodeConfigValue, error) {
	var configBytes []byte
	if err := f.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(configBucketName).Get(localNodeConfigKey)
		if value != nil {
			configBytes = make([]byte, len(value))
			copy(configBytes, value)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if configBytes == nil {
		return nil, nil
	}

	var lnConfig LocalNodeConfigValue
	if configBytes != nil {
		err := proto.Unmarshal(configBytes, &lnConfig)
		if err != nil {
			return nil, err
		}
		f.desiredSuffrage = lnConfig.DesiredSuffrage
		return &lnConfig, nil
	}

	return nil, nil
}

func (f *FSM) DesiredSuffrage() string {
	f.l.RLock()
	defer f.l.RUnlock()

	return f.desiredSuffrage
}

func (f *FSM) upgradeLocalNodeConfig() error {
	f.l.Lock()
	defer f.l.Unlock()

	// Read the local node config
	lnConfig, err := f.localNodeConfig()
	if err != nil {
		return err
	}

	// Entry is already present. Get the suffrage value.
	if lnConfig != nil {
		f.desiredSuffrage = lnConfig.DesiredSuffrage
		return nil
	}

	//
	// This is the upgrade case where there is no entry
	//

	lnConfig = &LocalNodeConfigValue{}

	// Refer to the persisted latest raft config
	config := f.latestConfig.Load().(*ConfigurationValue)

	// If there is no config, then this is a fresh node coming up. This could end up
	// being a voter or non-voter. But by default assume that this is a voter. It
	// will be changed if this node joins the cluster as a non-voter.
	if config == nil {
		f.desiredSuffrage = "voter"
		lnConfig.DesiredSuffrage = f.desiredSuffrage
		return f.persistDesiredSuffrage(lnConfig)
	}

	// Get the last known suffrage of the node and assume that it is the desired
	// suffrage. There is no better alternative here.
	for _, srv := range config.Servers {
		if srv.Id == f.localID {
			switch srv.Suffrage {
			case int32(raft.Nonvoter):
				lnConfig.DesiredSuffrage = "non-voter"
			default:
				lnConfig.DesiredSuffrage = "voter"
			}
			// Bring the intent to the fsm instance.
			f.desiredSuffrage = lnConfig.DesiredSuffrage
			break
		}
	}

	return f.persistDesiredSuffrage(lnConfig)
}

// recordSuffrage is called when a node successfully joins the cluster. This
// intent should land in the stored configuration. If the config isn't available
// yet, we still go ahead and store the intent in the fsm. During the next
// update to the configuration, this intent will be persisted.
func (f *FSM) recordSuffrage(desiredSuffrage string) error {
	f.l.Lock()
	defer f.l.Unlock()

	if err := f.persistDesiredSuffrage(&LocalNodeConfigValue{
		DesiredSuffrage: desiredSuffrage,
	}); err != nil {
		return err
	}

	f.desiredSuffrage = desiredSuffrage
	return nil
}

func (f *FSM) persistDesiredSuffrage(lnconfig *LocalNodeConfigValue) error {
	dsBytes, err := proto.Marshal(lnconfig)
	if err != nil {
		return err
	}

	return f.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(configBucketName).Put(localNodeConfigKey, dsBytes)
	})
}

func (f *FSM) witnessSnapshot(metadata *raft.SnapshotMeta) error {
	f.l.RLock()
	defer f.l.RUnlock()

	err := writeSnapshotMetaToDB(metadata, f.db)
	if err != nil {
		return err
	}

	atomic.StoreUint64(f.latestIndex, metadata.Index)
	atomic.StoreUint64(f.latestTerm, metadata.Term)
	f.latestConfig.Store(raftConfigurationToProtoConfiguration(metadata.ConfigurationIndex, metadata.Configuration))

	return nil
}

// LatestState returns the latest index and configuration values we have seen on
// this FSM.
func (f *FSM) LatestState() (*IndexValue, *ConfigurationValue) {
	return &IndexValue{
		Term:  atomic.LoadUint64(f.latestTerm),
		Index: atomic.LoadUint64(f.latestIndex),
	}, f.latestConfig.Load().(*ConfigurationValue)
}

// Delete deletes the given key from the bolt file.
func (f *FSM) Delete(ctx context.Context, path string) error {
	defer metrics.MeasureSince([]string{"raft_storage", "fsm", "delete"}, time.Now())

	f.l.RLock()
	defer f.l.RUnlock()

	return f.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(dataBucketName).Delete([]byte(path))
	})
}

// Delete deletes the given key from the bolt file.
func (f *FSM) DeletePrefix(ctx context.Context, prefix string) error {
	defer metrics.MeasureSince([]string{"raft_storage", "fsm", "delete_prefix"}, time.Now())

	f.l.RLock()
	defer f.l.RUnlock()

	err := f.db.Update(func(tx *bolt.Tx) error {
		// Assume bucket exists and has keys
		c := tx.Bucket(dataBucketName).Cursor()

		prefixBytes := []byte(prefix)
		for k, _ := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = c.Next() {
			if err := c.Delete(); err != nil {
				return err
			}
		}

		return nil
	})

	return err
}

// Get retrieves the value at the given path from the bolt file.
func (f *FSM) Get(ctx context.Context, path string) (*physical.Entry, error) {
	// TODO: Remove this outdated metric name in an older release
	defer metrics.MeasureSince([]string{"raft", "get"}, time.Now())
	defer metrics.MeasureSince([]string{"raft_storage", "fsm", "get"}, time.Now())

	f.l.RLock()
	defer f.l.RUnlock()

	var valCopy []byte
	var found bool

	err := f.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(dataBucketName).Get([]byte(path))
		if value != nil {
			found = true
			valCopy = make([]byte, len(value))
			copy(valCopy, value)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	return &physical.Entry{
		Key:   path,
		Value: valCopy,
	}, nil
}

// Put writes the given entry to the bolt file.
func (f *FSM) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"raft_storage", "fsm", "put"}, time.Now())

	f.l.RLock()
	defer f.l.RUnlock()

	// Start a write transaction.
	return f.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(dataBucketName).Put([]byte(entry.Key), entry.Value)
	})
}

// List retrieves the set of keys with the given prefix from the bolt file.
func (f *FSM) List(ctx context.Context, prefix string) ([]string, error) {
	return f.ListPage(ctx, prefix, "", -1)
}

// ListPage retrieves the set of keys with the given prefix from the bolt
// file, after the specified entry (if present), and up to the given
// limit of entries.
func (f *FSM) ListPage(ctx context.Context, prefix string, after string, limit int) ([]string, error) {
	// TODO: Remove this outdated metric name in a future release
	defer metrics.MeasureSince([]string{"raft", "list"}, time.Now())
	defer metrics.MeasureSince([]string{"raft_storage", "fsm", "list"}, time.Now())

	f.l.RLock()
	defer f.l.RUnlock()

	var err error
	var keys []string
	err = f.db.View(func(tx *bolt.Tx) error {
		keys, err = listPageInner(ctx, tx, prefix, after, limit)
		if err != nil {
			return err
		}

		return nil
	})

	return keys, err
}

func listPageInner(ctx context.Context, tx *bolt.Tx, prefix string, after string, limit int) ([]string, error) {
	var keys []string

	prefixBytes := []byte(prefix)
	seekPrefix := []byte(filepath.Join(prefix, after))
	if after == "" {
		seekPrefix = prefixBytes
	} else if !bytes.HasPrefix(seekPrefix, prefixBytes) {
		// filepath.Join has the very unfortunate behavior of trimming the
		// trailing slash when after=".". When e.g., prefix=foo/, this gives
		// us seekPrefix=foo, which fails the initial HasPrefix check,
		// skipping all results.
		seekPrefix = prefixBytes
	}

	// Assume bucket exists and has keys
	c := tx.Bucket(dataBucketName).Cursor()

	// By seeking relative to the after location, we can save looking
	// at unnecessary entries before our expected entry.
	for k, _ := c.Seek(seekPrefix); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = c.Next() {
		if limit > 0 && len(keys) >= limit {
			// We've seen enough entries; exit early.
			return keys, nil
		}

		// Note that we push the comparison of 'key' with 'after'
		// until we add in the directory suffix, if necessary.
		key := string(k)
		key = strings.TrimPrefix(key, prefix)
		if i := strings.Index(key, "/"); i == -1 {
			if after != "" && key <= after {
				// Still prior to our cut-off point, so retry.
				continue
			}

			// Add objects only from the current 'folder'
			keys = append(keys, key)
		} else {
			// Add truncated 'folder' paths
			if len(keys) == 0 || keys[len(keys)-1] != key[:i+1] {
				folder := string(key[:i+1])
				if after != "" && folder <= after {
					// Still prior to our cut-off point, so retry.
					continue
				}

				keys = append(keys, folder)
			}
		}
	}

	return keys, nil
}

// Within ApplyBatch, applies non-transactional operations.
func (f *FSM) applyBatchNonTxOps(b *bolt.Bucket, txnState *fsmTxnCommitIndexApplicationState, command *LogData) error {
	for _, op := range command.Operations {
		var err error
		switch op.OpType {
		case putOp:
			err = b.Put([]byte(op.Key), op.Value)
			if err == nil {
				// This log occurs directly to the state tracker, so we
				// want to ensure we only track it when the write succeeded.
				txnState.logWrite(op.Key)
			}
		case deleteOp:
			err = b.Delete([]byte(op.Key))
			if err == nil {
				// See note above.
				txnState.logWrite(op.Key)
			}
		case restoreCallbackOp:
			if f.restoreCb != nil {
				// Kick off the restore callback function in a go routine
				go f.restoreCb(context.Background())
			}
		default:
			if _, ok := f.unknownOpTypes.Load(op.OpType); !ok {
				f.logger.Error("unsupported transaction operation", "op", op.OpType)
				f.unknownOpTypes.Store(op.OpType, struct{}{})
			}
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// Within ApplyBatch, applies a transaction within the broader context of the
// batch's transaction.
func (f *FSM) applyBatchTxOps(tx *bolt.Tx, b *bolt.Bucket, txnState *fsmTxnCommitIndexApplicationState, command *LogData) error {
	txnState.setInTx()

	// First we verify the transaction can apply correctly before doing any
	// write operations. This allows us to safely ignore it if it conflicts
	// with previous writes.
	//
	// This assumes that the transaction is constructed so that all verify
	// operations are relative to the initial state of storage and not the
	// in-transaction updated state.
	var err error
	for index, op := range command.Operations {
		switch op.OpType {
		case beginTxOp, commitTxOp:
			// Ensure a well-formed transaction.
			if command.Operations[0].OpType != beginTxOp || command.Operations[len(command.Operations)-1].OpType != commitTxOp || (index != 0 && index != len(command.Operations)-1) {
				return fmt.Errorf("unsupported transaction: saw beginTxOp/commitTxOp mixed inside other operations: %w", physical.ErrTransactionCommitFailure)
			}

			if op.OpType == beginTxOp {
				s, err := parseBeginTxOpValue(op.Value)
				if err != nil {
					return err
				}
				txnState.setStartIndex(s.Index)
			}
		case putOp:
			// ignore
		case deleteOp:
			// ignore
		case verifyReadOp:
			err = txnState.doVerifyRead(b, op)
		case verifyListOp:
			err = txnState.doVerifyList(tx, b, op)
		default:
			if _, ok := f.unknownOpTypes.Load(op.OpType); !ok {
				f.logger.Error("unsupported transaction operation", "op", op.OpType)
				f.unknownOpTypes.Store(op.OpType, struct{}{})
			}
		}

		if err != nil {
			return err
		}
	}

	// Now we apply the write operations since the verification succeeded.
	for _, op := range command.Operations {
		switch op.OpType {
		case beginTxOp, commitTxOp:
			// ignore
		case putOp:
			err = b.Put([]byte(op.Key), op.Value)
			txnState.logWrite(op.Key)
		case deleteOp:
			err = b.Delete([]byte(op.Key))
			txnState.logWrite(op.Key)
		case verifyReadOp:
			// ignore
		case verifyListOp:
			// ignore
		default:
			if _, ok := f.unknownOpTypes.Load(op.OpType); !ok {
				f.logger.Error("unsupported transaction operation", "op", op.OpType)
				f.unknownOpTypes.Store(op.OpType, struct{}{})
			}
		}

		if err != nil {
			return err
		}
	}

	// Record the transaction as having been applied and merge state back into
	// the central fast application tracking.
	txnState.finishTxn()

	return nil
}

// ApplyBatch will apply a set of logs to the FSM. This is called from the raft
// library.
func (f *FSM) ApplyBatch(logs []*raft.Log) []interface{} {
	numLogs := len(logs)

	if numLogs == 0 {
		return []interface{}{}
	}

	// We will construct one slice per log, each slice containing another slice of results from our get ops
	entrySlices := make([][]*FSMEntry, 0, numLogs)

	// Do the unmarshalling first so we don't hold locks
	var latestConfiguration *ConfigurationValue
	commands := make([]interface{}, 0, numLogs)
	for _, l := range logs {
		switch l.Type {
		case raft.LogCommand:
			command := &LogData{}
			err := proto.Unmarshal(l.Data, command)
			if err != nil {
				f.logger.Error("error proto unmarshaling log data", "error", err)
				panic("error proto unmarshaling log data")
			}
			commands = append(commands, command)
		case raft.LogConfiguration:
			configuration := raft.DecodeConfiguration(l.Data)
			config := raftConfigurationToProtoConfiguration(l.Index, configuration)

			commands = append(commands, config)

			// Update the latest configuration the fsm has received; we will
			// store this after it has been committed to storage.
			latestConfiguration = config

		default:
			panic(fmt.Sprintf("got unexpected log type: %d", l.Type))
		}
	}

	// Only advance latest pointer if this log has a higher index value than
	// what we have seen in the past.
	var logIndex []byte
	var err error
	latestIndex, _ := f.LatestState()
	lastLog := logs[numLogs-1]
	if latestIndex.Index < lastLog.Index {
		logIndex, err = proto.Marshal(&IndexValue{
			Term:  lastLog.Term,
			Index: lastLog.Index,
		})
		if err != nil {
			f.logger.Error("unable to marshal latest index", "error", err)
			panic("unable to marshal latest index")
		}
	}

	f.l.RLock()
	defer f.l.RUnlock()

	if f.applyCallback != nil {
		f.applyCallback()
	}

	// One would think that this f.db.Update(...) and the following loop over
	// commands should be in the opposite order, as we want transactions to be
	// applied atomically. Indeed, 2c154ad516162dcb8b15ad270cd6a15516f2ce59 had
	// this ordered that way. It has two issues though:
	//
	// 1. It is slower, as each bbolt transaction incurs additional storage
	//    writes.
	// 2. Technically, Raft expects the entire batch to succeed or fail as a
	//    unit; thus, we don't want to commit partial state from a previous
	//    log entry (that succeeded) when a later log entry fails.
	//
	// Hence, keep the original upstream ordering of Update w.r.t. batch
	// application and switch to pre-verifying transactions prior to
	// performing any writes in them.
	err = f.db.Update(func(tx *bolt.Tx) error {
		var err error
		b := tx.Bucket(dataBucketName)
		configB := tx.Bucket(configBucketName)
		latestIndex := atomic.LoadUint64(f.latestIndex)

		for commandIndex, commandRaw := range commands {
			entrySlice := make([]*FSMEntry, 0, 1)

			switch command := commandRaw.(type) {
			case *LogData:
				txnState := f.fastTxnTracker.applyState(latestIndex, commandIndex, logs[commandIndex].Index)

				if len(command.Operations) == 0 || command.Operations[0].OpType != beginTxOp {
					err = f.applyBatchNonTxOps(b, txnState, command)
				} else {
					err = f.applyBatchTxOps(tx, b, txnState, command)
				}

				if err != nil {
					// If we're in a transaction, we do not want to err the
					// global f.db.Update call unless this is a critical error
					// worthy of a panic(...).
					//
					// Create a special FSMEntry to send back the error
					// message, that applyLog(...) will look for if it
					// sent a transaction.
					if txnState.getInTx() && errors.Is(err, physical.ErrTransactionCommitFailure) {
						entrySlice = append(entrySlice, &FSMEntry{
							Key:   fsmEntryTxErrorKey,
							Value: []byte(err.Error()),
						})

						// Process other events; this transaction failure was handled
						// appropriately already in applyBatchTxOps.
						err = nil
					}
				}
			case *ConfigurationValue:
				configBytes, err := proto.Marshal(command)
				if err != nil {
					return err
				}
				if err := configB.Put(latestConfigKey, configBytes); err != nil {
					return err
				}
			}

			entrySlices = append(entrySlices, entrySlice)

			if err != nil {
				break
			}
		}

		return err
	})

	// If we had no error, update our last applied log.
	if err == nil {
		err = f.db.Update(func(tx *bolt.Tx) error {
			if len(logIndex) > 0 {
				b := tx.Bucket(configBucketName)
				err = b.Put(latestIndexKey, logIndex)
				if err != nil {
					return err
				}
			}

			return nil
		})
	}

	if err != nil {
		f.logger.Error("failed to store data", "error", err)
		panic("failed to store data")
	}

	// If we advanced the latest value, update the in-memory representation too.
	if len(logIndex) > 0 {
		atomic.StoreUint64(f.latestTerm, lastLog.Term)
		atomic.StoreUint64(f.latestIndex, lastLog.Index)
	}

	// If one or more configuration changes were processed, store the latest one.
	if latestConfiguration != nil {
		f.latestConfig.Store(latestConfiguration)
	}

	// Build the responses. The logs array is used here to ensure we reply to
	// all command values; even if they are not of the types we expect. This
	// should futureproof this function from more log types being provided.
	resp := make([]interface{}, numLogs)
	for i := range logs {
		resp[i] = &FSMApplyResponse{
			Success:    true,
			EntrySlice: entrySlices[i],
		}
	}

	return resp
}

// Apply will apply a log value to the FSM. This is called from the raft
// library.
func (f *FSM) Apply(log *raft.Log) interface{} {
	return f.ApplyBatch([]*raft.Log{log})[0]
}

type writeErrorCloser interface {
	io.WriteCloser
	CloseWithError(error) error
}

// writeTo will copy the FSM's content to a remote sink. The data is written
// twice, once for use in determining various metadata attributes of the dataset
// (size, checksum, etc) and a second for the sink of the data. We also use a
// proto delimited writer so we can stream proto messages to the sink.
func (f *FSM) writeTo(ctx context.Context, metaSink writeErrorCloser, sink writeErrorCloser) {
	defer metrics.MeasureSince([]string{"raft_storage", "fsm", "write_snapshot"}, time.Now())

	protoWriter := NewDelimitedWriter(sink)
	metadataProtoWriter := NewDelimitedWriter(metaSink)

	f.l.RLock()
	defer f.l.RUnlock()

	err := f.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(dataBucketName)

		c := b.Cursor()

		// Do the first scan of the data for metadata purposes.
		for k, v := c.First(); k != nil; k, v = c.Next() {
			err := metadataProtoWriter.WriteMsg(&pb.StorageEntry{
				Key:   string(k),
				Value: v,
			})
			if err != nil {
				metaSink.CloseWithError(err)
				return err
			}
		}
		metaSink.Close()

		// Do the second scan for copy purposes.
		for k, v := c.First(); k != nil; k, v = c.Next() {
			err := protoWriter.WriteMsg(&pb.StorageEntry{
				Key:   string(k),
				Value: v,
			})
			if err != nil {
				return err
			}
		}

		return nil
	})
	sink.CloseWithError(err)
}

// Snapshot implements the FSM interface. It returns a noop snapshot object.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	return &noopSnapshotter{
		fsm: f,
	}, nil
}

// SetNoopRestore is used to disable restore operations on raft startup. Because
// we are using persistent storage in our FSM we do not need to issue a restore
// on startup.
func (f *FSM) SetNoopRestore(enabled bool) {
	f.l.Lock()
	f.noopRestore = enabled
	f.l.Unlock()
}

// Restore installs a new snapshot from the provided reader. It does an atomic
// rename of the snapshot file into the database filepath. While a restore is
// happening the FSM is locked and no writes or reads can be performed.
func (f *FSM) Restore(r io.ReadCloser) error {
	defer metrics.MeasureSince([]string{"raft_storage", "fsm", "restore_snapshot"}, time.Now())

	if f.noopRestore {
		return nil
	}

	snapshotInstaller, ok := r.(*boltSnapshotInstaller)
	if !ok {
		wrapper, ok := r.(raft.ReadCloserWrapper)
		if !ok {
			return fmt.Errorf("expected ReadCloserWrapper object, got: %T", r)
		}
		snapshotInstallerRaw := wrapper.WrappedReadCloser()
		snapshotInstaller, ok = snapshotInstallerRaw.(*boltSnapshotInstaller)
		if !ok {
			return fmt.Errorf("expected snapshot installer object, got: %T", snapshotInstallerRaw)
		}
	}

	f.l.Lock()
	defer f.l.Unlock()

	// Cache the local node config before closing the db file
	lnConfig, err := f.localNodeConfig()
	if err != nil {
		return err
	}

	// Close the db file
	if err := f.db.Close(); err != nil {
		f.logger.Error("failed to close database file", "error", err)
		return err
	}

	dbPath := filepath.Join(f.path, databaseFilename)

	f.logger.Info("installing snapshot to FSM")

	// Install the new boltdb file
	var retErr *multierror.Error
	if err := snapshotInstaller.Install(dbPath); err != nil {
		f.logger.Error("failed to install snapshot", "error", err)
		retErr = multierror.Append(retErr, fmt.Errorf("failed to install snapshot database: %w", err))
	} else {
		f.logger.Info("snapshot installed")
	}

	// Open the db file. We want to do this regardless of if the above install
	// worked. If the install failed we should try to open the old DB file.
	if err := f.openDBFile(dbPath); err != nil {
		f.logger.Error("failed to open new database file", "error", err)
		retErr = multierror.Append(retErr, fmt.Errorf("failed to open new bolt file: %w", err))
	}

	// Handle local node config restore. lnConfig should not be nil here, but
	// adding the nil check anyways for safety.
	if lnConfig != nil {
		// Persist the local node config on the restored fsm.
		if err := f.persistDesiredSuffrage(lnConfig); err != nil {
			f.logger.Error("failed to persist local node config from before the restore", "error", err)
			retErr = multierror.Append(retErr, fmt.Errorf("failed to persist local node config from before the restore: %w", err))
		}
	}

	return retErr.ErrorOrNil()
}

// noopSnapshotter implements the fsm.Snapshot interface. It doesn't do anything
// since our SnapshotStore reads data out of the FSM on Open().
type noopSnapshotter struct {
	fsm *FSM
}

// Persist implements the fsm.Snapshot interface. It doesn't need to persist any
// state data, but it does persist the raft metadata. This is necessary so we
// can be sure to capture indexes for operation types that are not sent to the
// FSM.
func (s *noopSnapshotter) Persist(sink raft.SnapshotSink) error {
	boltSnapshotSink := sink.(*BoltSnapshotSink)

	// We are processing a snapshot, fastforward the index, term, and
	// configuration to the latest seen by the raft system.
	if err := s.fsm.witnessSnapshot(&boltSnapshotSink.meta); err != nil {
		return err
	}

	return nil
}

// Release doesn't do anything.
func (s *noopSnapshotter) Release() {}

// raftConfigurationToProtoConfiguration converts a raft configuration object to
// a proto value.
func raftConfigurationToProtoConfiguration(index uint64, configuration raft.Configuration) *ConfigurationValue {
	servers := make([]*Server, len(configuration.Servers))
	for i, s := range configuration.Servers {
		servers[i] = &Server{
			Suffrage: int32(s.Suffrage),
			Id:       string(s.ID),
			Address:  string(s.Address),
		}
	}
	return &ConfigurationValue{
		Index:   index,
		Servers: servers,
	}
}

// protoConfigurationToRaftConfiguration converts a proto configuration object
// to a raft object.
func protoConfigurationToRaftConfiguration(configuration *ConfigurationValue) (uint64, raft.Configuration) {
	servers := make([]raft.Server, len(configuration.Servers))
	for i, s := range configuration.Servers {
		servers[i] = raft.Server{
			Suffrage: raft.ServerSuffrage(s.Suffrage),
			ID:       raft.ServerID(s.Id),
			Address:  raft.ServerAddress(s.Address),
		}
	}
	return configuration.Index, raft.Configuration{
		Servers: servers,
	}
}

type FSMChunkStorage struct {
	f   *FSM
	ctx context.Context
}

// chunkPaths returns a disk prefix and key given chunkinfo
func (f *FSMChunkStorage) chunkPaths(chunk *raftchunking.ChunkInfo) (string, string) {
	prefix := fmt.Sprintf("%s%d/", chunkingPrefix, chunk.OpNum)
	key := fmt.Sprintf("%s%d", prefix, chunk.SequenceNum)
	return prefix, key
}

func (f *FSMChunkStorage) StoreChunk(chunk *raftchunking.ChunkInfo) (bool, error) {
	b, err := jsonutil.EncodeJSON(chunk)
	if err != nil {
		return false, fmt.Errorf("error encoding chunk info: %w", err)
	}

	prefix, key := f.chunkPaths(chunk)

	entry := &physical.Entry{
		Key:   key,
		Value: b,
	}

	f.f.l.RLock()
	defer f.f.l.RUnlock()

	// Start a write transaction.
	done := new(bool)
	if err := f.f.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(dataBucketName).Put([]byte(entry.Key), entry.Value); err != nil {
			return fmt.Errorf("error storing chunk info: %w", err)
		}

		// Assume bucket exists and has keys
		c := tx.Bucket(dataBucketName).Cursor()

		var keys []string
		prefixBytes := []byte(prefix)
		for k, _ := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = c.Next() {
			key := string(k)
			key = strings.TrimPrefix(key, prefix)
			if i := strings.Index(key, "/"); i == -1 {
				// Add objects only from the current 'folder'
				keys = append(keys, key)
			} else {
				// Add truncated 'folder' paths
				keys = strutil.AppendIfMissing(keys, string(key[:i+1]))
			}
		}

		*done = uint32(len(keys)) == chunk.NumChunks

		return nil
	}); err != nil {
		return false, err
	}

	return *done, nil
}

func (f *FSMChunkStorage) FinalizeOp(opNum uint64) ([]*raftchunking.ChunkInfo, error) {
	ret, err := f.chunksForOpNum(opNum)
	if err != nil {
		return nil, fmt.Errorf("error getting chunks for op keys: %w", err)
	}

	prefix, _ := f.chunkPaths(&raftchunking.ChunkInfo{OpNum: opNum})
	if err := f.f.DeletePrefix(f.ctx, prefix); err != nil {
		return nil, fmt.Errorf("error deleting prefix after op finalization: %w", err)
	}

	return ret, nil
}

func (f *FSMChunkStorage) chunksForOpNum(opNum uint64) ([]*raftchunking.ChunkInfo, error) {
	prefix, _ := f.chunkPaths(&raftchunking.ChunkInfo{OpNum: opNum})

	opChunkKeys, err := f.f.List(f.ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("error fetching op chunk keys: %w", err)
	}

	if len(opChunkKeys) == 0 {
		return nil, nil
	}

	var ret []*raftchunking.ChunkInfo

	for _, v := range opChunkKeys {
		seqNum, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("error converting seqnum to integer: %w", err)
		}

		entry, err := f.f.Get(f.ctx, prefix+v)
		if err != nil {
			return nil, fmt.Errorf("error fetching chunkinfo: %w", err)
		}

		var ci raftchunking.ChunkInfo
		if err := jsonutil.DecodeJSON(entry.Value, &ci); err != nil {
			return nil, fmt.Errorf("error decoding chunkinfo json: %w", err)
		}

		if ret == nil {
			ret = make([]*raftchunking.ChunkInfo, ci.NumChunks)
		}

		ret[seqNum] = &ci
	}

	return ret, nil
}

func (f *FSMChunkStorage) GetChunks() (raftchunking.ChunkMap, error) {
	opNums, err := f.f.List(f.ctx, chunkingPrefix)
	if err != nil {
		return nil, fmt.Errorf("error doing recursive list for chunk saving: %w", err)
	}

	if len(opNums) == 0 {
		return nil, nil
	}

	ret := make(raftchunking.ChunkMap, len(opNums))
	for _, opNumStr := range opNums {
		opNum, err := strconv.ParseInt(opNumStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("error parsing op num during chunk saving: %w", err)
		}

		opChunks, err := f.chunksForOpNum(uint64(opNum))
		if err != nil {
			return nil, fmt.Errorf("error getting chunks for op keys during chunk saving: %w", err)
		}

		ret[uint64(opNum)] = opChunks
	}

	return ret, nil
}

func (f *FSMChunkStorage) RestoreChunks(chunks raftchunking.ChunkMap) error {
	if err := f.f.DeletePrefix(f.ctx, chunkingPrefix); err != nil {
		return fmt.Errorf("error deleting prefix for chunk restoration: %w", err)
	}
	if len(chunks) == 0 {
		return nil
	}

	for opNum, opChunks := range chunks {
		for _, chunk := range opChunks {
			if chunk == nil {
				continue
			}
			if chunk.OpNum != opNum {
				return errors.New("unexpected op number in chunk")
			}
			if _, err := f.StoreChunk(chunk); err != nil {
				return fmt.Errorf("error storing chunk during restoration: %w", err)
			}
		}
	}

	return nil
}
