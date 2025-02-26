package batcher

import (
	"fmt"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/queue"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

type channelStatuser interface {
	isFullySubmitted() bool
	isTimedOut() bool
	LatestL2() eth.BlockID
	MaxInclusionBlock() uint64
}

type inclusiveBlockRange struct{ start, end uint64 }
type syncActions struct {
	clearState      *eth.BlockID
	blocksToPrune   int
	channelsToPrune int
	blocksToLoad    *inclusiveBlockRange // the blocks that should be loaded into the local state.
	// NOTE this range is inclusive on both ends, which is a change to previous behaviour.
}

func (s syncActions) String() string {
	return fmt.Sprintf(
		"SyncActions{blocksToPrune: %d, channelsToPrune: %d, clearState: %v, blocksToLoad: %v}", s.blocksToPrune, s.channelsToPrune, s.clearState, s.blocksToLoad)
}

// computeSyncActions determines the actions that should be taken based on the inputs provided. The inputs are the current
// state of the batcher (blocks and channels), the new sync status, and the previous current L1 block. The actions are returned
// in a struct specifying the number of blocks to prune, the number of channels to prune, whether to wait for node sync, the block
// range to load into the local state, and whether to clear the state entirely. Returns an boolean indicating if the sequencer is out of sync.
func computeSyncActions[T channelStatuser](newSyncStatus eth.SyncStatus, prevCurrentL1 eth.L1BlockRef, blocks queue.Queue[*types.Block], channels []T, l log.Logger) (syncActions, bool) {

	// PART 1: Initial checks on the sync status
	if newSyncStatus.HeadL1 == (eth.L1BlockRef{}) {
		l.Warn("empty sync status")
		return syncActions{}, true
	}

	if newSyncStatus.CurrentL1.Number < prevCurrentL1.Number {
		// This can happen when the sequencer restarts
		l.Warn("sequencer currentL1 reversed")
		return syncActions{}, true
	}

	// PART 2: checks involving only the oldest block in the state
	oldestBlockInState, hasBlocks := blocks.Peek()
	oldestUnsafeBlockNum := newSyncStatus.SafeL2.Number + 1
	youngestUnsafeBlockNum := newSyncStatus.UnsafeL2.Number

	if !hasBlocks {
		s := syncActions{
			blocksToLoad: &inclusiveBlockRange{oldestUnsafeBlockNum, youngestUnsafeBlockNum},
		}
		l.Info("no blocks in state", "syncActions", s)
		return s, false
	}

	// These actions apply in multiple unhappy scenarios below, where
	// we detect that the existing state is invalidated
	// and we need to start over from the sequencer's oldest
	// unsafe (and not safe) block.
	startAfresh := syncActions{
		clearState:   &newSyncStatus.SafeL2.L1Origin,
		blocksToLoad: &inclusiveBlockRange{oldestUnsafeBlockNum, youngestUnsafeBlockNum},
	}

	oldestBlockInStateNum := oldestBlockInState.NumberU64()

	if oldestUnsafeBlockNum < oldestBlockInStateNum {
		l.Warn("oldest unsafe block is below oldest block in state", "syncActions", startAfresh, "oldestBlockInState", oldestBlockInState, "newSafeBlock", newSyncStatus.SafeL2)
		return startAfresh, false
	}

	// PART 3: checks involving all blocks in state
	newestBlockInState := blocks[blocks.Len()-1]
	newestBlockInStateNum := newestBlockInState.NumberU64()

	numBlocksToDequeue := oldestUnsafeBlockNum - oldestBlockInStateNum

	if numBlocksToDequeue > uint64(blocks.Len()) {
		// This could happen if the batcher restarted.
		// The sequencer may have derived the safe chain
		// from channels sent by a previous batcher instance.
		l.Warn("oldest unsafe block above newest block in state, clearing channel manager state",
			"oldestUnsafeBlockNum", oldestUnsafeBlockNum,
			"newestBlockInState", eth.ToBlockID(newestBlockInState),
			"syncActions",
			startAfresh)
		return startAfresh, false
	}

	if numBlocksToDequeue > 0 && blocks[numBlocksToDequeue-1].Hash() != newSyncStatus.SafeL2.Hash {
		l.Warn("safe chain reorg, clearing channel manager state",
			"existingBlock", eth.ToBlockID(blocks[numBlocksToDequeue-1]),
			"newSafeBlock", newSyncStatus.SafeL2,
			"syncActions", startAfresh)
		return startAfresh, false
	}

	// PART 4: checks involving channels
	for _, ch := range channels {
		if ch.isFullySubmitted() &&
			!ch.isTimedOut() &&
			newSyncStatus.CurrentL1.Number > ch.MaxInclusionBlock() &&
			newSyncStatus.SafeL2.Number < ch.LatestL2().Number {
			// Safe head did not make the expected progress
			// for a fully submitted channel. This indicates
			// that the derivation pipeline may have stalled
			// e.g. because of Holocene strict ordering rules.
			l.Warn("sequencer did not make expected progress",
				"existingBlock", eth.ToBlockID(blocks[numBlocksToDequeue-1]),
				"newSafeBlock", newSyncStatus.SafeL2,
				"syncActions", startAfresh)
			return startAfresh, false
		}
	}

	// PART 5: happy path
	numChannelsToPrune := 0
	for _, ch := range channels {
		if ch.LatestL2().Number > newSyncStatus.SafeL2.Number {
			// If the channel has blocks which are not yet safe
			// we do not want to prune it.
			break
		}
		numChannelsToPrune++
	}

	start := newestBlockInStateNum + 1
	end := youngestUnsafeBlockNum

	return syncActions{
		blocksToPrune:   int(numBlocksToDequeue),
		channelsToPrune: numChannelsToPrune,
		blocksToLoad:    &inclusiveBlockRange{start, end},
	}, false
}
