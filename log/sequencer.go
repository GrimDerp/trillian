// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package log

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/ptypes"
	"github.com/google/trillian"
	"github.com/google/trillian/merkle/compact"
	"github.com/google/trillian/merkle/hashers"
	"github.com/google/trillian/monitoring"
	"github.com/google/trillian/quota"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/types"
	"github.com/google/trillian/util/clock"

	tcrypto "github.com/google/trillian/crypto"
)

const logIDLabel = "logid"

var (
	sequencerOnce          sync.Once
	seqBatches             monitoring.Counter
	seqTreeSize            monitoring.Gauge
	seqLatency             monitoring.Histogram
	seqDequeueLatency      monitoring.Histogram
	seqGetRootLatency      monitoring.Histogram
	seqInitTreeLatency     monitoring.Histogram
	seqWriteTreeLatency    monitoring.Histogram
	seqUpdateLeavesLatency monitoring.Histogram
	seqSetNodesLatency     monitoring.Histogram
	seqStoreRootLatency    monitoring.Histogram
	seqCounter             monitoring.Counter
	seqMergeDelay          monitoring.Histogram
	seqTimestamp           monitoring.Gauge

	// QuotaIncreaseFactor is the multiplier used for the number of tokens added back to
	// sequencing-based quotas. The resulting PutTokens call is equivalent to
	// "PutTokens(_, numLeaves * QuotaIncreaseFactor, _)".
	// A factor >1 adds resilience to token leakage, on the risk of a system that's overly
	// optimistic in face of true token shortages. The higher the factor, the higher the quota
	// "optimism" is. A factor that's too high (say, >1.5) is likely a sign that the quota
	// configuration should be changed instead.
	// A factor <1 WILL lead to token shortages, therefore it'll be normalized to 1.
	QuotaIncreaseFactor = 1.1
)

func quotaIncreaseFactor() float64 {
	if QuotaIncreaseFactor < 1 {
		QuotaIncreaseFactor = 1
		return 1
	}
	return QuotaIncreaseFactor
}

// TODO(pavelkalinnikov): Create all metrics in this package together.
func createSequencerMetrics(mf monitoring.MetricFactory) {
	if mf == nil {
		mf = monitoring.InertMetricFactory{}
	}
	quota.InitMetrics(mf)
	seqBatches = mf.NewCounter("sequencer_batches", "Number of sequencer batch operations", logIDLabel)
	seqTreeSize = mf.NewGauge("sequencer_tree_size", "Tree size of last SLR signed", logIDLabel)
	seqTimestamp = mf.NewGauge("sequencer_tree_timestamp", "Time of last SLR signed in ms since epoch", logIDLabel)
	seqLatency = mf.NewHistogram("sequencer_latency", "Latency of sequencer batch operation in seconds", logIDLabel)
	seqDequeueLatency = mf.NewHistogram("sequencer_latency_dequeue", "Latency of dequeue-leaves part of sequencer batch operation in seconds", logIDLabel)
	seqGetRootLatency = mf.NewHistogram("sequencer_latency_get_root", "Latency of get-root part of sequencer batch operation in seconds", logIDLabel)
	seqInitTreeLatency = mf.NewHistogram("sequencer_latency_init_tree", "Latency of init-tree part of sequencer batch operation in seconds", logIDLabel)
	seqWriteTreeLatency = mf.NewHistogram("sequencer_latency_write_tree", "Latency of write-tree part of sequencer batch operation in seconds", logIDLabel)
	seqUpdateLeavesLatency = mf.NewHistogram("sequencer_latency_update_leaves", "Latency of update-leaves part of sequencer batch operation in seconds", logIDLabel)
	seqSetNodesLatency = mf.NewHistogram("sequencer_latency_set_nodes", "Latency of set-nodes part of sequencer batch operation in seconds", logIDLabel)
	seqStoreRootLatency = mf.NewHistogram("sequencer_latency_store_root", "Latency of store-root part of sequencer batch operation in seconds", logIDLabel)
	seqCounter = mf.NewCounter("sequencer_sequenced", "Number of leaves sequenced", logIDLabel)
	seqMergeDelay = mf.NewHistogram("sequencer_merge_delay", "Delay between queuing and integration of leaves", logIDLabel)
}

// Sequencer instances are responsible for integrating new leaves into a single log.
// Leaves will be assigned unique sequence numbers when they are processed.
// There is no strong ordering guarantee but in general entries will be processed
// in order of submission to the log.
type Sequencer struct {
	hasher     hashers.LogHasher
	timeSource clock.TimeSource
	logStorage storage.LogStorage
	signer     *tcrypto.Signer
	qm         quota.Manager
}

// maxTreeDepth sets an upper limit on the size of Log trees.
// Note: We actually can't go beyond 2^63 entries because we use int64s,
// but we need to calculate tree depths from a multiple of 8 due to the
// subtree assumptions.
const maxTreeDepth = 64

// NewSequencer creates a new Sequencer instance for the specified inputs.
func NewSequencer(
	hasher hashers.LogHasher,
	timeSource clock.TimeSource,
	logStorage storage.LogStorage,
	signer *tcrypto.Signer,
	mf monitoring.MetricFactory,
	qm quota.Manager) *Sequencer {
	sequencerOnce.Do(func() {
		createSequencerMetrics(mf)
	})
	return &Sequencer{
		hasher:     hasher,
		timeSource: timeSource,
		logStorage: logStorage,
		signer:     signer,
		qm:         qm,
	}
}

// initCompactRangeFromStorage builds a compact range that matches the latest
// data in the database. Ensures that the root hash matches the passed in root.
func (s Sequencer) initCompactRangeFromStorage(ctx context.Context, root *types.LogRootV1, tx storage.TreeTX) (*compact.Range, error) {
	fact := compact.RangeFactory{Hash: s.hasher.HashChildren}
	if root.TreeSize == 0 {
		return fact.NewEmptyRange(0), nil
	}

	ids := compact.RangeNodesForPrefix(root.TreeSize)
	storIDs := make([]storage.NodeID, len(ids))
	for i, id := range ids {
		nodeID, err := storage.NewNodeIDForTreeCoords(int64(id.Level), int64(id.Index), maxTreeDepth)
		if err != nil {
			return nil, fmt.Errorf("failed to create nodeID: %v", err)
		}
		storIDs[i] = nodeID
	}

	nodes, err := tx.GetMerkleNodes(ctx, int64(root.Revision), storIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to get Merkle nodes: %v", err)
	}
	if got, want := len(nodes), len(storIDs); got != want {
		return nil, fmt.Errorf("failed to get %d nodes at rev %d, got %d", want, root.Revision, got)
	}
	for i, id := range storIDs {
		if !nodes[i].NodeID.Equivalent(id) {
			return nil, fmt.Errorf("node ID mismatch at %d", i)
		}
	}

	hashes := make([][]byte, len(nodes))
	for i, node := range nodes {
		hashes[i] = node.Hash
	}

	cr, err := fact.NewRange(0, root.TreeSize, hashes)
	if err != nil {
		return nil, fmt.Errorf("failed to create compact.Range: %v", err)
	}
	hash, err := cr.GetRootHash(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to compute the root hash: %v", err)
	}
	// Note: Tree size != 0 at this point, so we don't consider the empty hash.
	if want := root.RootHash; !bytes.Equal(hash, want) {
		return nil, fmt.Errorf("root hash mismatch: got %x, want %x", hash, want)
	}
	return cr, nil
}

func (s Sequencer) buildNodesFromNodeMap(nodeMap map[compact.NodeID][]byte, newVersion int64) ([]storage.Node, error) {
	nodes := make([]storage.Node, 0, len(nodeMap))
	for id, hash := range nodeMap {
		nodeID, err := storage.NewNodeIDForTreeCoords(int64(id.Level), int64(id.Index), maxTreeDepth)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, storage.Node{NodeID: nodeID, Hash: hash, NodeRevision: newVersion})
	}
	return nodes, nil
}

func (s Sequencer) prepareLeaves(leaves []*trillian.LogLeaf, begin uint64, label string) error {
	now := s.timeSource.Now()
	integrateAt, err := ptypes.TimestampProto(now)
	if err != nil {
		return fmt.Errorf("got invalid integrate timestamp: %v", err)
	}
	for i, leaf := range leaves {
		// The leaf should already have the correct index before it's integrated.
		if got, want := leaf.LeafIndex, begin+uint64(i); got < 0 || got != int64(want) {
			return fmt.Errorf("got invalid leaf index: %v, want: %v", got, want)
		}
		leaf.IntegrateTimestamp = integrateAt

		// Old leaves might not have a QueueTimestamp, only calculate the merge
		// delay if this one does.
		if leaf.QueueTimestamp != nil && leaf.QueueTimestamp.Seconds != 0 {
			queueTS, err := ptypes.Timestamp(leaf.QueueTimestamp)
			if err != nil {
				return fmt.Errorf("got invalid queue timestamp: %v", queueTS)
			}
			mergeDelay := now.Sub(queueTS)
			seqMergeDelay.Observe(mergeDelay.Seconds(), label)
		}
	}
	return nil
}

// updateCompactRange adds the passed in leaves to the compact range. Returns a
// map of all updated tree nodes, and the new root hash.
func (s Sequencer) updateCompactRange(cr *compact.Range, leaves []*trillian.LogLeaf, label string) (map[compact.NodeID][]byte, []byte, error) {
	nodeMap := make(map[compact.NodeID][]byte)
	store := func(id compact.NodeID, hash []byte) { nodeMap[id] = hash }

	// Update the tree state by integrating the leaves one by one.
	for _, leaf := range leaves {
		idx := leaf.LeafIndex
		if size := cr.End(); idx < 0 || idx != int64(size) {
			return nil, nil, fmt.Errorf("leaf index mismatch: got %d, want %d", idx, size)
		}
		// Store the leaf hash in the Merkle tree.
		store(compact.NewNodeID(0, uint64(idx)), leaf.MerkleLeafHash)
		// Store all the new internal nodes.
		if err := cr.Append(leaf.MerkleLeafHash, store); err != nil {
			return nil, nil, err
		}
	}
	// Store ephemeral nodes on the right border of the tree as well.
	hash, err := cr.GetRootHash(store)
	if err != nil {
		return nil, nil, err
	}
	return nodeMap, hash, nil
}

// sequencingTask provides sequenced LogLeaf entries, and updates storage
// according to their ordering if needed.
type sequencingTask interface {
	// fetch returns a batch of sequenced entries obtained from storage, sized up
	// to the specified limit. The returned leaves have consecutive LeafIndex
	// values starting from the current tree size.
	fetch(ctx context.Context, limit int, cutoff time.Time) ([]*trillian.LogLeaf, error)

	// update makes sequencing persisted in storage, if not yet.
	update(ctx context.Context, leaves []*trillian.LogLeaf) error
}

type sequencingTaskData struct {
	label      string
	treeSize   uint64
	timeSource clock.TimeSource
	tx         storage.LogTreeTX
}

// logSequencingTask is a sequencingTask implementation for "normal" Log mode,
// which assigns consecutive sequence numbers to leaves as they are read from
// the pending unsequenced entries.
type logSequencingTask sequencingTaskData

func (s *logSequencingTask) fetch(ctx context.Context, limit int, cutoff time.Time) ([]*trillian.LogLeaf, error) {
	start := s.timeSource.Now()
	// Recent leaves inside the guard window will not be available for sequencing.
	leaves, err := s.tx.DequeueLeaves(ctx, limit, cutoff)
	if err != nil {
		return nil, fmt.Errorf("%v: Sequencer failed to dequeue leaves: %v", s.label, err)
	}
	seqDequeueLatency.Observe(clock.SecondsSince(s.timeSource, start), s.label)

	// Assign leaf sequence numbers.
	for i, leaf := range leaves {
		leaf.LeafIndex = int64(s.treeSize + uint64(i))
		if got := leaf.LeafIndex; got < 0 {
			return nil, fmt.Errorf("%v: leaf index overflow: %d", s.label, got)
		}
	}
	return leaves, nil
}

func (s *logSequencingTask) update(ctx context.Context, leaves []*trillian.LogLeaf) error {
	start := s.timeSource.Now()
	// Write the new sequence numbers to the leaves in the DB.
	if err := s.tx.UpdateSequencedLeaves(ctx, leaves); err != nil {
		return fmt.Errorf("%v: Sequencer failed to update sequenced leaves: %v", s.label, err)
	}
	seqUpdateLeavesLatency.Observe(clock.SecondsSince(s.timeSource, start), s.label)
	return nil
}

// preorderedLogSequencingTask is a sequencingTask implementation for
// Pre-ordered Log mode. It reads sequenced entries past the tree size which
// are already in the storage.
type preorderedLogSequencingTask sequencingTaskData

func (s *preorderedLogSequencingTask) fetch(ctx context.Context, limit int, cutoff time.Time) ([]*trillian.LogLeaf, error) {
	start := s.timeSource.Now()
	leaves, err := s.tx.DequeueLeaves(ctx, limit, cutoff)
	if err != nil {
		return nil, fmt.Errorf("%v: Sequencer failed to load sequenced leaves: %v", s.label, err)
	}
	seqDequeueLatency.Observe(clock.SecondsSince(s.timeSource, start), s.label)
	return leaves, nil
}

func (s *preorderedLogSequencingTask) update(ctx context.Context, leaves []*trillian.LogLeaf) error {
	// TODO(pavelkalinnikov): Update integration timestamps.
	return nil
}

// IntegrateBatch wraps up all the operations needed to take a batch of queued
// or sequenced leaves and integrate them into the tree.
func (s Sequencer) IntegrateBatch(ctx context.Context, tree *trillian.Tree, limit int, guardWindow, maxRootDurationInterval time.Duration) (int, error) {
	start := s.timeSource.Now()
	label := strconv.FormatInt(tree.TreeId, 10)

	numLeaves := 0
	var newLogRoot *types.LogRootV1
	var newSLR *trillian.SignedLogRoot
	err := s.logStorage.ReadWriteTransaction(ctx, tree, func(ctx context.Context, tx storage.LogTreeTX) error {
		stageStart := s.timeSource.Now()
		defer seqBatches.Inc(label)
		defer func() { seqLatency.Observe(clock.SecondsSince(s.timeSource, start), label) }()

		// Get the latest known root from storage
		sth, err := tx.LatestSignedLogRoot(ctx)
		if err != nil {
			return fmt.Errorf("%v: Sequencer failed to get latest root: %v", tree.TreeId, err)
		}
		// There is no trust boundary between the signer and the
		// database, so we skip signature verification.
		// TODO(gbelvin): Add signature checking as a santity check.
		var currentRoot types.LogRootV1
		if err := currentRoot.UnmarshalBinary(sth.LogRoot); err != nil {
			return fmt.Errorf("%v: Sequencer failed to unmarshal latest root: %v", tree.TreeId, err)
		}
		seqGetRootLatency.Observe(clock.SecondsSince(s.timeSource, stageStart), label)
		seqTreeSize.Set(float64(currentRoot.TreeSize), label)

		if currentRoot.RootHash == nil {
			glog.Warningf("%v: Fresh log - no previous TreeHeads exist.", tree.TreeId)
			return storage.ErrTreeNeedsInit
		}

		taskData := &sequencingTaskData{
			label:      label,
			treeSize:   currentRoot.TreeSize,
			timeSource: s.timeSource,
			tx:         tx,
		}
		var st sequencingTask
		switch tree.TreeType {
		case trillian.TreeType_LOG:
			st = (*logSequencingTask)(taskData)
		case trillian.TreeType_PREORDERED_LOG:
			st = (*preorderedLogSequencingTask)(taskData)
		default:
			return fmt.Errorf("IntegrateBatch not supported for TreeType %v", tree.TreeType)
		}

		sequencedLeaves, err := st.fetch(ctx, limit, start.Add(-guardWindow))
		if err != nil {
			return fmt.Errorf("%v: Sequencer failed to load sequenced batch: %v", tree.TreeId, err)
		}
		numLeaves = len(sequencedLeaves)

		// We need to create a signed root if entries were added or the latest root
		// is too old.
		if numLeaves == 0 {
			nowNanos := s.timeSource.Now().UnixNano()
			interval := time.Duration(nowNanos - int64(currentRoot.TimestampNanos))
			if maxRootDurationInterval == 0 || interval < maxRootDurationInterval {
				// We have nothing to integrate into the tree.
				glog.V(1).Infof("%v: No leaves sequenced in this signing operation", tree.TreeId)
				return nil
			}
			glog.Infof("%v: Force new root generation as %v since last root", tree.TreeId, interval)
		}

		stageStart = s.timeSource.Now()
		cr, err := s.initCompactRangeFromStorage(ctx, &currentRoot, tx)
		if err != nil {
			return fmt.Errorf("%v: compact range init failed: %v", tree.TreeId, err)
		}
		seqInitTreeLatency.Observe(clock.SecondsSince(s.timeSource, stageStart), label)
		stageStart = s.timeSource.Now()

		// We've done all the reads, can now do the updates in the same transaction.
		// The schema should prevent multiple SLRs being inserted with the same
		// revision number so it should not be possible for colliding updates to
		// commit.
		newVersion, err := tx.WriteRevision(ctx)
		if err != nil {
			return err
		}
		if got, want := newVersion, int64(currentRoot.Revision)+1; got != want {
			return fmt.Errorf("%v: got writeRevision of %v, but expected %v", tree.TreeId, got, want)
		}

		// Collate node updates.
		if err := s.prepareLeaves(sequencedLeaves, cr.End(), label); err != nil {
			return err
		}
		nodeMap, newRoot, err := s.updateCompactRange(cr, sequencedLeaves, label)
		if err != nil {
			return err
		}
		seqWriteTreeLatency.Observe(clock.SecondsSince(s.timeSource, stageStart), label)

		// Store the sequenced batch.
		if err := st.update(ctx, sequencedLeaves); err != nil {
			return err
		}
		stageStart = s.timeSource.Now()

		// Build objects for the nodes to be updated. Because we deduped via the map
		// each node can only be created / updated once in each tree revision and
		// they cannot conflict when we do the storage update.
		targetNodes, err := s.buildNodesFromNodeMap(nodeMap, newVersion)
		if err != nil {
			// Probably an internal error with map building, unexpected.
			return fmt.Errorf("%v: Failed to build target nodes in sequencer: %v", tree.TreeId, err)
		}

		// Now insert or update the nodes affected by the above, at the new tree
		// version.
		if err := tx.SetMerkleNodes(ctx, targetNodes); err != nil {
			return fmt.Errorf("%v: Sequencer failed to set Merkle nodes: %v", tree.TreeId, err)
		}
		seqSetNodesLatency.Observe(clock.SecondsSince(s.timeSource, stageStart), label)
		stageStart = s.timeSource.Now()

		// Create the log root ready for signing.
		if cr.End() == 0 {
			// Override the nil root hash returned by the compact range.
			newRoot = s.hasher.EmptyRoot()
		}
		newLogRoot = &types.LogRootV1{
			RootHash:       newRoot,
			TimestampNanos: uint64(s.timeSource.Now().UnixNano()),
			TreeSize:       cr.End(),
			Revision:       uint64(newVersion),
		}
		seqTreeSize.Set(float64(newLogRoot.TreeSize), label)
		seqTimestamp.Set(float64(time.Duration(newLogRoot.TimestampNanos)*time.Nanosecond/
			time.Millisecond), label)

		if newLogRoot.TimestampNanos <= currentRoot.TimestampNanos {
			return fmt.Errorf("%v: refusing to sign root with timestamp earlier than previous root (%d <= %d)", tree.TreeId, newLogRoot.TimestampNanos, currentRoot.TimestampNanos)
		}

		newSLR, err = s.signer.SignLogRoot(newLogRoot)
		if err != nil {
			return fmt.Errorf("%v: signer failed to sign root: %v", tree.TreeId, err)
		}

		if err := tx.StoreSignedLogRoot(ctx, newSLR); err != nil {
			return fmt.Errorf("%v: failed to write updated tree root: %v", tree.TreeId, err)
		}
		seqStoreRootLatency.Observe(clock.SecondsSince(s.timeSource, stageStart), label)
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Let quota.Manager know about newly-sequenced entries.
	s.replenishQuota(ctx, numLeaves, tree.TreeId)

	seqCounter.Add(float64(numLeaves), label)
	if newSLR != nil {
		glog.Infof("%v: sequenced %v leaves, size %v, tree-revision %v", tree.TreeId, numLeaves, newLogRoot.TreeSize, newLogRoot.Revision)
	}
	return numLeaves, nil
}

// replenishQuota replenishes all quotas, such as {Tree/Global, Read/Write},
// that are possibly influenced by sequencing numLeaves entries for the passed
// in tree ID. Implementations are tasked with filtering quotas that shouldn't
// be replenished.
//
// TODO(codingllama): Consider adding a source-aware replenish method (e.g.,
// qm.Replenish(ctx, tokens, specs, quota.SequencerSource)), so there's no
// ambiguity as to where the tokens come from.
func (s Sequencer) replenishQuota(ctx context.Context, numLeaves int, treeID int64) {
	if numLeaves > 0 {
		tokens := int(float64(numLeaves) * quotaIncreaseFactor())
		specs := []quota.Spec{
			{Group: quota.Tree, Kind: quota.Read, TreeID: treeID},
			{Group: quota.Tree, Kind: quota.Write, TreeID: treeID},
			{Group: quota.Global, Kind: quota.Read},
			{Group: quota.Global, Kind: quota.Write},
		}
		glog.V(2).Infof("%v: replenishing %d tokens (numLeaves = %d)", treeID, tokens, numLeaves)
		err := s.qm.PutTokens(ctx, tokens, specs)
		if err != nil {
			glog.Warningf("%v: failed to replenish %d tokens: %v", treeID, tokens, err)
		}
		quota.Metrics.IncReplenished(tokens, specs, err == nil)
	}
}
