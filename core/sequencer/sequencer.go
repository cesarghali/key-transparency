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

package sequencer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/google/keytransparency/core/mutator"
	"github.com/google/keytransparency/core/mutator/entry"
	"github.com/google/keytransparency/core/transaction"

	"github.com/golang/glog"
	"github.com/google/trillian"
	"github.com/google/trillian/util"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"

	tpb "github.com/google/keytransparency/core/proto/keytransparency_v1_types"
)

var (
	mutationsCtr = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kt_signer_mutations",
		Help: "Number of mutations the signer has processed.",
	})
	indexCtr = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kt_signer_mutations_unique",
		Help: "Number of mutations the signer has processed post per epoch dedupe.",
	})
	mapUpdateHist = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "kt_signer_map_update_seconds",
		Help:    "Seconds waiting for map update",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, math.Inf(1)},
	})
	createEpochHist = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "kt_signer_create_epoch_seconds",
		Help:    "Seconds spent generating epoch",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, math.Inf(1)},
	})
)

func init() {
	prometheus.MustRegister(mutationsCtr)
	prometheus.MustRegister(indexCtr)
	prometheus.MustRegister(mapUpdateHist)
	prometheus.MustRegister(createEpochHist)
}

// Sequencer processes mutations and sends them to the trillian map.
type Sequencer struct {
	mapID     int64
	tmap      trillian.TrillianMapClient
	logID     int64
	tlog      trillian.TrillianLogClient
	mutator   mutator.Mutator
	mutations mutator.Mutation
	factory   transaction.Factory
	mMux      *sync.Mutex
	mChannels []chan *tpb.GetMutationsResponse
}

// New creates a new instance of the signer.
func New(mapID int64,
	tmap trillian.TrillianMapClient,
	logID int64,
	tlog trillian.TrillianLogClient,
	mutator mutator.Mutator,
	mutations mutator.Mutation,
	factory transaction.Factory) *Sequencer {
	return &Sequencer{
		mapID:     mapID,
		tmap:      tmap,
		logID:     logID,
		tlog:      tlog,
		mutator:   mutator,
		mutations: mutations,
		factory:   factory,
		mMux:      &sync.Mutex{},
	}
}

// Initialize inserts the object hash of an empty struct into the log if it is empty.
// This keeps the log leaves in-sync with the map which starts off with an
// empty log root at map revision 0.
func (s *Sequencer) Initialize(ctx context.Context) error {
	logRoot, err := s.tlog.GetLatestSignedLogRoot(ctx, &trillian.GetLatestSignedLogRootRequest{
		LogId: s.logID,
	})
	if err != nil {
		return fmt.Errorf("GetLatestSignedLogRoot(%v): %v", s.logID, err)
	}
	mapRoot, err := s.tmap.GetSignedMapRoot(ctx, &trillian.GetSignedMapRootRequest{
		MapId: s.mapID,
	})
	if err != nil {
		return fmt.Errorf("GetSignedMapRoot(%v): %v", s.mapID, err)
	}

	// If the tree is empty and the map is empty,
	// add the empty map root to the log.
	if logRoot.GetSignedLogRoot().GetTreeSize() == 0 &&
		mapRoot.GetMapRoot().GetMapRevision() == 0 {
		glog.Infof("Initializing Trillian Log with empty map root")
		if err := queueLogLeaf(ctx, s.tlog, s.logID, mapRoot.GetMapRoot()); err != nil {
			return err
		}
	}
	return nil
}

// StartSigning advance epochs once per minInterval, if there were mutations,
// and at least once per maxElapsed minIntervals.
func (s *Sequencer) StartSigning(ctx context.Context, minInterval, maxInterval time.Duration) {
	if err := s.Initialize(ctx); err != nil {
		glog.Errorf("Initialize() failed: %v", err)
	}
	var rootResp *trillian.GetSignedMapRootResponse
	ctxTime, cancel := context.WithTimeout(ctx, minInterval)
	rootResp, err := s.tmap.GetSignedMapRoot(ctxTime, &trillian.GetSignedMapRootRequest{
		MapId: s.mapID,
	})
	if err != nil {
		glog.Infof("GetSignedMapRoot failed: %v", err)
		// Immediately create new epoch and write new sth:
		if _, err := s.CreateEpoch(ctxTime, true); err != nil {
			glog.Errorf("CreateEpoch failed: %v", err)
		}
		// Request map head again to get the exact time it was created:
		rootResp, err = s.tmap.GetSignedMapRoot(ctxTime, &trillian.GetSignedMapRootRequest{
			MapId: s.mapID,
		})
		if err != nil {
			glog.Errorf("GetSignedMapRoot failed after CreateEpoch: %v", err)
		}
	}
	cancel()
	// Fetch last time from previous map head (as stored in the map server)
	mapRoot := rootResp.GetMapRoot()
	last := time.Unix(0, mapRoot.GetTimestampNanos())
	// Start issuing epochs:
	clock := util.SystemTimeSource{}
	tc := time.NewTicker(minInterval).C
	for f := range genEpochTicks(clock, last, tc, minInterval, maxInterval) {
		ctxTime, cancel := context.WithTimeout(ctx, minInterval)
		mutations, err := s.CreateEpoch(ctxTime, f)
		if err != nil {
			glog.Errorf("CreateEpoch failed: %v", err)
		}
		// Send mutations to sequencer service handler.
		s.disseminateMutations(mutations)
		cancel()
	}
}

// genEpochTicks returns and sends to a bool channel every time an epoch should
// be created. If the boolean value is true this indicates that the epoch should
// be created regardless of whether mutations exist.
func genEpochTicks(t util.TimeSource, last time.Time, minTick <-chan time.Time, minElapsed, maxElapsed time.Duration) <-chan bool {
	enforce := make(chan bool)
	go func() {
		// Do not wait for the first minDuration to pass but directly
		// resume from last.
		if (t.Now().Sub(last) + minElapsed) >= maxElapsed {
			enforce <- true
			last = t.Now()
		}

		for now := range minTick {
			if (now.Sub(last) + minElapsed) >= maxElapsed {
				enforce <- true
				last = now
			} else {
				enforce <- false
			}
		}
	}()

	return enforce
}

// newMutations returns a list of mutations to process and highest sequence
// number returned.
func (s *Sequencer) newMutations(ctx context.Context, startSequence int64) ([]*tpb.SignedKV, int64, error) {
	txn, err := s.factory.NewTxn(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("NewDBTxn(): %v", err)
	}

	maxSequence, mutations, err := s.mutations.ReadAll(txn, uint64(startSequence))
	if err != nil {
		if err := txn.Rollback(); err != nil {
			glog.Errorf("Cannot rollback the transaction: %v", err)
		}
		return nil, 0, fmt.Errorf("ReadAll(%v): %v", startSequence, err)
	}

	if err := txn.Commit(); err != nil {
		return nil, 0, fmt.Errorf("txn.Commit(): %v", err)
	}
	return mutations, int64(maxSequence), nil
}

// toArray returns the first 32 bytes from b.
// If b is less than 32 bytes long, the output is zero padded.
func toArray(b []byte) [32]byte {
	var i [32]byte
	copy(i[:], b)
	return i
}

// applyMutations takes the set of mutations and applies them to given leafs.
// Multiple mutations for the same leaf will be applied to provided leaf.
// The last valid mutation for each leaf is included in the output.
// Returns a list of map leaves that should be updated.
func (s *Sequencer) applyMutations(mutations []*tpb.SignedKV, leaves []*trillian.MapLeaf) ([]*trillian.MapLeaf, error) {
	// Put leaves in a map from index to leaf value.
	leafMap := make(map[[32]byte]*trillian.MapLeaf)
	for _, l := range leaves {
		leafMap[toArray(l.Index)] = l
	}

	retMap := make(map[[32]byte]*trillian.MapLeaf)
	for _, m := range mutations {
		index := m.GetKeyValue().GetKey()
		var oldValue *tpb.Entry // If no map leaf was found, oldValue will be nil.
		if leaf, ok := leafMap[toArray(index)]; ok {
			var err error
			oldValue, err = entry.FromLeafValue(leaf.GetLeafValue())
			if err != nil {
				glog.Warningf("entry.FromLeafValue(%v): %v", err)
				continue
			}
		}

		newValue, err := s.mutator.Mutate(oldValue, m)
		if err != nil {
			glog.Warningf("Mutate(): %v", err)
			continue // A bad mutation should not make the whole batch fail.
		}

		retMap[toArray(index)] = &trillian.MapLeaf{
			Index:     index,
			LeafValue: newValue,
		}
	}
	// Convert return map back into a list.
	ret := make([]*trillian.MapLeaf, 0, len(retMap))
	for _, v := range retMap {
		ret = append(ret, v)
	}
	return ret, nil
}

// CreateEpoch signs the current map head.
func (s *Sequencer) CreateEpoch(ctx context.Context, forceNewEpoch bool) (*tpb.GetMutationsResponse, error) {
	glog.V(2).Infof("CreateEpoch: starting sequencing run")
	start := time.Now()
	// Get the current root.
	rootResp, err := s.tmap.GetSignedMapRoot(ctx, &trillian.GetSignedMapRootRequest{
		MapId: s.mapID,
	})
	if err != nil {
		return nil, fmt.Errorf("GetSignedMapRoot(%v): %v", s.mapID, err)
	}
	startSequence := rootResp.GetMapRoot().GetMetadata().GetHighestFullyCompletedSeq()
	revision := rootResp.GetMapRoot().GetMapRevision()
	glog.V(3).Infof("CreateEpoch: Previous SignedMapRoot: {Revision: %v, HighestFullyCompletedSeq: %v}", revision, startSequence)

	// Get the list of new mutations to process.
	mRange, seq, err := s.newMutations(ctx, startSequence)
	if err != nil {
		return nil, fmt.Errorf("newMutations(%v): %v", startSequence, err)
	}

	// Don't create epoch if there is nothing to process unless explicitly
	// specified by caller
	if len(mRange) == 0 && !forceNewEpoch {
		glog.Infof("CreateEpoch: No mutations found. Exiting.")
		return nil, nil
	}

	// Get current leaf values.
	mutations := make([]*tpb.Mutation, 0, len(mRange))
	indexes := make([][]byte, 0, len(mRange))
	for _, m := range mRange {
		mutations = append(mutations, &tpb.Mutation{Update: m})
		indexes = append(indexes, m.GetKeyValue().GetKey())
	}
	glog.V(2).Infof("CreateEpoch: len(mutations): %v, len(indexes): %v",
		len(mRange), len(indexes))
	getResp, err := s.tmap.GetLeaves(ctx, &trillian.GetMapLeavesRequest{
		MapId:    s.mapID,
		Index:    indexes,
		Revision: -1, // Get the latest version.
	})
	if err != nil {
		return nil, err
	}
	glog.V(3).Infof("CreateEpoch: len(GetLeaves.MapLeafInclusions): %v",
		len(getResp.MapLeafInclusion))

	// Trust the leaf values provided by the map server.
	// If the map server is run by an untrusted entity, perform inclusion
	// and signature verification here.
	leaves := make([]*trillian.MapLeaf, 0, len(getResp.MapLeafInclusion))
	for i, m := range getResp.GetMapLeafInclusion() {
		leaves = append(leaves, m.Leaf)
		mutations[i].Proof = m
	}

	// Apply mutations to values.
	newLeaves, err := s.applyMutations(mRange, leaves)
	if err != nil {
		return nil, err
	}
	glog.V(2).Infof("CreateEpoch: applied %v mutations to %v leaves",
		len(mRange), len(leaves))

	// Set new leaf values.
	mapSetStart := time.Now()
	setResp, err := s.tmap.SetLeaves(ctx, &trillian.SetMapLeavesRequest{
		MapId:  s.mapID,
		Leaves: newLeaves,
		MapperData: &trillian.MapperMetadata{
			HighestFullyCompletedSeq: seq,
		},
	})
	mapSetEnd := time.Now()
	if err != nil {
		return nil, err
	}
	revision = setResp.GetMapRoot().GetMapRevision()
	glog.V(2).Infof("CreateEpoch: SetLeaves:{Revision: %v, HighestFullyCompletedSeq: %v}", revision, seq)

	// Put SignedMapHead in an append only log.
	if err := queueLogLeaf(ctx, s.tlog, s.logID, setResp.GetMapRoot()); err != nil {
		// TODO(gdbelvin): If the log doesn't do this, we need to generate an emergency alert.
		return nil, err
	}

	mutationsCtr.Add(float64(len(mRange)))
	indexCtr.Add(float64(len(indexes)))
	mapUpdateHist.Observe(mapSetEnd.Sub(mapSetStart).Seconds())
	createEpochHist.Observe(time.Since(start).Seconds())
	glog.Infof("CreatedEpoch: rev: %v, root: %x", revision, setResp.GetMapRoot().GetRootHash())

	// MapRevisions start at 1. Log leave's index starts at 0.
	// MapRevision should be at least 1 since the Signer is
	// supposed to create at least one revision on startup.
	respEpoch := setResp.GetMapRoot().GetMapRevision() - 1
	// Fetch log proofs.
	// TODO(cesarghali): fill in the right value for first tree size.
	logRoot, logConsistency, logInclusion, err := s.logProofs(ctx, 0, respEpoch)
	if err != nil {
		return nil, err
	}
	// Create mutations response to be sent to frontend servers.
	mutationsResp := &tpb.GetMutationsResponse{
		Epoch:          revision,
		Smr:            setResp.GetMapRoot(),
		LogRoot:        logRoot.GetSignedLogRoot(),
		LogConsistency: logConsistency.GetProof().GetHashes(),
		LogInclusion:   logInclusion.GetProof().GetHashes(),
		Mutations:      mutations,
	}
	return mutationsResp, nil
}

func (s *Sequencer) logProofs(ctx context.Context, firstTreeSize int64, epoch int64) (*trillian.GetLatestSignedLogRootResponse, *trillian.GetConsistencyProofResponse, *trillian.GetInclusionProofResponse, error) {
	logRoot, err := s.tlog.GetLatestSignedLogRoot(ctx,
		&trillian.GetLatestSignedLogRootRequest{
			LogId: s.logID,
		})
	if err != nil {
		glog.Errorf("tlog.GetLatestSignedLogRoot(%v): %v", s.logID, err)
		return nil, nil, nil, grpc.Errorf(codes.Internal, "Cannot fetch SignedLogRoot")
	}
	secondTreeSize := logRoot.GetSignedLogRoot().GetTreeSize()
	// Consistency proof.
	var logConsistency *trillian.GetConsistencyProofResponse
	if firstTreeSize != 0 {
		logConsistency, err = s.tlog.GetConsistencyProof(ctx,
			&trillian.GetConsistencyProofRequest{
				LogId:          s.logID,
				FirstTreeSize:  firstTreeSize,
				SecondTreeSize: secondTreeSize,
			})
		if err != nil {
			glog.Errorf("tlog.GetConsistency(%v, %v, %v): %v", s.logID, firstTreeSize, secondTreeSize, err)
			return nil, nil, nil, grpc.Errorf(codes.Internal, "Cannot fetch log consistency proof")
		}
	}
	// Inclusion proof.
	logInclusion, err := s.tlog.GetInclusionProof(ctx,
		&trillian.GetInclusionProofRequest{
			LogId: s.logID,
			// SignedMapRoot must be in the log at MapRevision.
			LeafIndex: epoch,
			TreeSize:  secondTreeSize,
		})
	if err != nil {
		glog.Errorf("tlog.GetInclusionProof(%v, %v, %v): %v", s.logID, epoch, secondTreeSize, err)
		return nil, nil, nil, grpc.Errorf(codes.Internal, "Cannot fetch log inclusion proof")
	}
	return logRoot, logConsistency, logInclusion, nil
}

// TODO(gdbelvin): Add leaf at a specific index. trillian#423
func queueLogLeaf(ctx context.Context, tlog trillian.TrillianLogClient, logID int64, smr *trillian.SignedMapRoot) error {
	smrJSON, err := json.Marshal(smr)
	if err != nil {
		return err
	}
	idHash := sha256.Sum256(smrJSON)

	if _, err := tlog.QueueLeaf(ctx, &trillian.QueueLeafRequest{
		LogId: logID,
		Leaf: &trillian.LogLeaf{
			LeafValue:        smrJSON,
			LeafIdentityHash: idHash[:],
		},
	}); err != nil {
		return fmt.Errorf("trillianLog.QueueLeaf(logID: %v, leaf: %v): %v",
			logID, smrJSON, err)
	}
	return nil
}

// RegisterMutationsChannel registers a mutations channel created by the
// sequencer service handler.
func (s *Sequencer) RegisterMutationsChannel(ch chan *tpb.GetMutationsResponse) {
	s.mMux.Lock()
	defer s.mMux.Unlock()
	s.mChannels = append(s.mChannels, ch)
}

// disseminateMutations sends mutations to sequencer service handler through the
// registered channels.
func (s *Sequencer) disseminateMutations(m *tpb.GetMutationsResponse) {
	s.mMux.Lock()
	defer s.mMux.Unlock()
	for _, ch := range s.mChannels {
		ch <- m
	}
}