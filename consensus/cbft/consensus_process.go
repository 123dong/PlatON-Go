package cbft

import (
	"fmt"

	"github.com/pingcap/failpoint"

	"github.com/PlatONnetwork/PlatON-Go/log"

	"github.com/PlatONnetwork/PlatON-Go/common"
	"github.com/PlatONnetwork/PlatON-Go/common/math"
	"github.com/PlatONnetwork/PlatON-Go/consensus/cbft/executor"
	"github.com/PlatONnetwork/PlatON-Go/consensus/cbft/protocols"
	"github.com/PlatONnetwork/PlatON-Go/consensus/cbft/state"
	ctypes "github.com/PlatONnetwork/PlatON-Go/consensus/cbft/types"
	"github.com/PlatONnetwork/PlatON-Go/core/cbfttypes"
	"github.com/PlatONnetwork/PlatON-Go/core/types"
)

// Perform security rule verification，store in blockTree, Whether to start synchronization
func (cbft *Cbft) OnPrepareBlock(id string, msg *protocols.PrepareBlock) error {
	cbft.log.Debug("Receive PrepareBlock", "id", id, "msg", msg.String())
	if err := cbft.safetyRules.PrepareBlockRules(msg); err != nil {
		if err.Fetch() {
			cbft.fetchBlock(id, msg.Block.Hash(), msg.Block.NumberU64())
			return err
		} else if err.NewView() {
			var block *types.Block
			var qc *ctypes.QuorumCert
			if msg.ViewChangeQC != nil {
				_, _, hash, number := msg.ViewChangeQC.MaxBlock()
				block, qc = cbft.blockTree.FindBlockAndQC(hash, number)
			} else {
				block, qc = cbft.blockTree.FindBlockAndQC(msg.Block.ParentHash(), msg.Block.NumberU64()-1)
			}
			cbft.log.Debug("Receive new view's block, change view", "newEpoch", msg.Epoch, "newView", msg.ViewNumber)
			cbft.changeView(msg.Epoch, msg.ViewNumber, block, qc, msg.ViewChangeQC)
		} else {
			cbft.log.Error("Prepare block rules fail", "number", msg.Block.Number(), "hash", msg.Block.Hash(), "err", err)
			return err
		}
	}

	// add failpoint
	failpoint.Inject("mock-OnPrepareBlock-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-OnPrepareBlock-panic")
		}
	})

	if _, err := cbft.verifyConsensusMsg(msg); err != nil {
		return err
	}

	// The new block is notified by the PrepareBlockHash to the nodes in the network.
	cbft.state.AddPrepareBlock(msg)
	cbft.prepareBlockFetchRules(id, msg)

	// add failpoint
	failpoint.Inject("mock-OnPrepareBlockAf-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-OnPrepareBlockAf-panic")
		}
	})

	cbft.findExecutableBlock()
	return nil
}

// Perform security rule verification，store in blockTree, Whether to start synchronization
func (cbft *Cbft) OnPrepareVote(id string, msg *protocols.PrepareVote) error {
	if err := cbft.safetyRules.PrepareVoteRules(msg); err != nil {
		if err.Fetch() {
			cbft.fetchBlock(id, msg.BlockHash, msg.BlockNumber)
		}
		return err
	}

	cbft.prepareVoteFetchRules(id, msg)

	var node *cbfttypes.ValidateNode
	var err error
	if node, err = cbft.verifyConsensusMsg(msg); err != nil {
		return err
	}

	cbft.insertPrepareQC(msg.ParentQC)

	cbft.state.AddPrepareVote(uint32(node.Index), msg)

	// add failpoint
	failpoint.Inject("mock-OnPrepareVote-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-OnPrepareVote-panic")
		}
	})

	cbft.findQCBlock()
	return nil
}

// Perform security rule verification, view switching
func (cbft *Cbft) OnViewChange(id string, msg *protocols.ViewChange) error {
	if err := cbft.safetyRules.ViewChangeRules(msg); err != nil {
		if err.Fetch() {
			cbft.fetchBlock(id, msg.BlockHash, msg.BlockNumber)
		}
		return err
	}

	var node *cbfttypes.ValidateNode
	var err error
	if node, err = cbft.verifyConsensusMsg(msg); err != nil {
		return err
	}

	// add failpoint
	failpoint.Inject("mock-OnViewChange-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-OnViewChange-panic")
		}
	})

	cbft.state.AddViewChange(uint32(node.Index), msg)
	cbft.log.Debug("Receive new viewchange", "index", node.Index, "total", cbft.state.ViewChangeLen())
	// It is possible to achieve viewchangeQC every time you add viewchange
	cbft.tryChangeView()
	return nil
}

func (cbft *Cbft) OnViewTimeout() {
	cbft.log.Info("Current view timeout", "view", cbft.state.ViewString())
	node, err := cbft.validatorPool.GetValidatorByNodeID(cbft.state.HighestQCBlock().NumberU64(), cbft.config.Option.NodeID)
	if err != nil {
		cbft.log.Error("ViewTimeout local node is not validator")
		return
	}
	hash, number := cbft.state.HighestQCBlock().Hash(), cbft.state.HighestQCBlock().NumberU64()
	_, qc := cbft.blockTree.FindBlockAndQC(hash, number)

	// add failpoint
	failpoint.Inject("mock-OnViewTimeout-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-OnViewTimeout-panic")
		}
	})

	viewChange := &protocols.ViewChange{
		Epoch:          cbft.state.Epoch(),
		ViewNumber:     cbft.state.ViewNumber(),
		BlockHash:      hash,
		BlockNumber:    number,
		ValidatorIndex: uint32(node.Index),
		PrepareQC:      qc,
	}

	if err := cbft.signMsgByBls(viewChange); err != nil {
		cbft.log.Error("Sign ViewChange failed", "err", err)
		return
	}

	// write sendViewChange info to wal
	if !cbft.isLoading() {
		cbft.bridge.SendViewChange(viewChange)
	}

	cbft.state.AddViewChange(uint32(node.Index), viewChange)
	cbft.log.Debug("Local add viewchange", "index", node.Index, "total", cbft.state.ViewChangeLen())

	cbft.network.Broadcast(viewChange)
	cbft.tryChangeView()
}

//Perform security rule verification, view switching
func (cbft *Cbft) OnInsertQCBlock(blocks []*types.Block, qcs []*ctypes.QuorumCert) error {
	if len(blocks) != len(qcs) {
		return fmt.Errorf("block")
	}
	//todo insert tree, update view
	for i := 0; i < len(blocks); i++ {
		block, qc := blocks[i], qcs[i]
		//todo verify qc

		if err := cbft.safetyRules.QCBlockRules(block, qc); err != nil {
			if err.NewView() {
				cbft.changeView(qc.Epoch, qc.ViewNumber, block, qc, nil)
			} else {
				return err
			}
		}
		cbft.insertQCBlock(block, qc)
		cbft.log.Debug("Insert QC block success", "hash", qc.BlockHash, "number", qc.BlockNumber)

	}

	return nil
}

// Update blockTree, try commit new block
func (cbft *Cbft) insertQCBlock(block *types.Block, qc *ctypes.QuorumCert) {
	cbft.log.Debug("Insert QC block", "qc", qc.String())
	if cbft.state.Epoch() == qc.Epoch && cbft.state.ViewNumber() == qc.ViewNumber {
		cbft.state.AddQC(qc)
	}
	cbft.txPool.Reset(block)

	lock, commit := cbft.blockTree.InsertQCBlock(block, qc)
	cbft.state.SetHighestQCBlock(block)
	cbft.tryCommitNewBlock(lock, commit)
	cbft.tryChangeView()

	// Update validator
	if cbft.validatorPool.ShouldSwitch(block.NumberU64()) {
		if err := cbft.validatorPool.Update(block.NumberU64(), cbft.eventMux); err == nil {
			cbft.state.ResetView(cbft.state.Epoch()+1, state.DefaultViewNumber)
			cbft.log.Debug("Update validator success", "number", block.NumberU64(), "epoch", cbft.state.Epoch())
		}
	}
}

func (cbft *Cbft) insertPrepareQC(qc *ctypes.QuorumCert) {
	if qc != nil {
		block := cbft.state.ViewBlockByIndex(qc.BlockIndex)

		linked := func(blockNumber uint64) bool {
			if block != nil {
				parent, _ := cbft.blockTree.FindBlockAndQC(block.ParentHash(), block.NumberU64()-1)
				return parent != nil && cbft.state.HighestQCBlock().NumberU64()+1 == blockNumber
			}
			return false
		}
		hasExecuted := func() bool {
			if cbft.validatorPool.IsValidator(qc.BlockNumber, cbft.config.Option.NodeID) {
				return cbft.state.HadSendPrepareVote().Had(qc.BlockIndex) && linked(qc.BlockNumber)
			} else if cbft.validatorPool.IsCandidateNode(cbft.config.Option.NodeID) {
				blockIndex, finish := cbft.state.Executing()
				return blockIndex != math.MaxUint32 && (qc.BlockIndex < blockIndex || (qc.BlockIndex == blockIndex && finish)) && linked(qc.BlockNumber)
			}
			return false
		}

		if block != nil && hasExecuted() {
			cbft.insertQCBlock(block, qc)
		}
	}
}

// Asynchronous execution block callback function
func (cbft *Cbft) onAsyncExecuteStatus(s *executor.BlockExecuteStatus) {
	cbft.log.Debug("Async Execute Block", "hash", s.Hash, "number", s.Number)
	if s.Err != nil {
		cbft.log.Error("Execute block failed", "err", s.Err, "hash", s.Hash, "number", s.Number)
		return
	}
	index, finish := cbft.state.Executing()
	if !finish {
		block := cbft.state.ViewBlockByIndex(index)
		if block != nil {
			if block.Hash() == s.Hash {
				cbft.state.SetExecuting(index, true)
				if err := cbft.signBlock(block.Hash(), block.NumberU64(), index); err != nil {
					cbft.log.Error("Sign block failed", "err", err, "hash", s.Hash, "number", s.Number)
					return
				}
				cbft.log.Debug("Sign block", "hash", s.Hash, "number", s.Number)
			}
		}
	}
	cbft.findQCBlock()
	cbft.findExecutableBlock()
}

// Sign the block that has been executed
// Every time try to trigger a send PrepareVote
func (cbft *Cbft) signBlock(hash common.Hash, number uint64, index uint32) error {
	// todo sign vote
	// parentQC added when sending
	node, err := cbft.validatorPool.GetValidatorByNodeID(number, cbft.config.Option.NodeID)
	if err != nil {
		return err
	}
	prepareVote := &protocols.PrepareVote{
		Epoch:          cbft.state.Epoch(),
		ViewNumber:     cbft.state.ViewNumber(),
		BlockHash:      hash,
		BlockNumber:    number,
		BlockIndex:     index,
		ValidatorIndex: uint32(node.Index),
	}

	if err := cbft.signMsgByBls(prepareVote); err != nil {
		return err
	}
	cbft.state.PendingPrepareVote().Push(prepareVote)

	cbft.trySendPrepareVote()
	return nil
}

// Send a signature,
// obtain a signature from the pending queue,
// determine whether the parent block has reached QC,
// and send a signature if it is reached, otherwise exit the sending logic.
func (cbft *Cbft) trySendPrepareVote() {
	pending := cbft.state.PendingPrepareVote()
	hadSend := cbft.state.HadSendPrepareVote()

	for !pending.Empty() {
		p := pending.Top()
		if err := cbft.voteRules.AllowVote(p); err != nil {
			cbft.log.Debug("Not allow send vote", "err", err, "msg", p.String())
			break
		}

		block := cbft.state.ViewBlockByIndex(p.BlockIndex)
		// The executed block has a signature.
		// Only when the view is switched, the block is cleared but the vote is also cleared.
		// If there is no block, the consensus process is abnormal and should not run.
		if block == nil {
			cbft.log.Crit("Try send PrepareVote failed", "err", "vote corresponding block not found", "view", cbft.state.ViewString(), p.String())
		}
		if b, qc := cbft.blockTree.FindBlockAndQC(block.ParentHash(), block.NumberU64()-1); b != nil || block.NumberU64() == 0 {
			p.ParentQC = qc
			hadSend.Push(p)
			node, _ := cbft.validatorPool.GetValidatorByNodeID(p.BlockNum(), cbft.config.Option.NodeID)
			cbft.state.AddPrepareVote(uint32(node.Index), p)
			pending.Pop()

			// write sendPrepareVote info to wal
			if !cbft.isLoading() {
				cbft.bridge.SendPrepareVote(block, p)
			}

			cbft.network.Broadcast(p)
		} else {
			break
		}
	}
}

// Every time there is a new block or a new executed block result will enter this judgment, find the next executable block
func (cbft *Cbft) findExecutableBlock() {
	blockIndex, finish := cbft.state.Executing()
	if blockIndex == math.MaxUint32 {
		block := cbft.state.ViewBlockByIndex(blockIndex + 1)
		if block != nil {
			parent, _ := cbft.blockTree.FindBlockAndQC(block.ParentHash(), block.NumberU64()-1)
			if parent == nil {
				cbft.log.Error(fmt.Sprintf("Find executable block's parent failed :[%d,%d,%s]", blockIndex, block.NumberU64(), block.Hash()))
				return
			}

			cbft.log.Debug("Find Executable Block", "hash", block.Hash(), "number", block.NumberU64())
			if err := cbft.asyncExecutor.Execute(block, parent); err != nil {
				cbft.log.Error("Async Execute block failed", "error", err)
			}
			cbft.state.SetExecuting(0, false)
		}
	}

	// add failpoint
	failpoint.Inject("mock-findExecutableBlock-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-findExecutableBlock-panic")
		}
	})
	if finish {
		block := cbft.state.ViewBlockByIndex(blockIndex + 1)
		if block != nil {
			parent := cbft.state.ViewBlockByIndex(blockIndex)
			if parent == nil {
				cbft.log.Error(fmt.Sprintf("Find executable block's parent failed :[%d,%d,%s]", blockIndex, block.NumberU64(), block.Hash()))
				return
			}

			if err := cbft.asyncExecutor.Execute(block, parent); err != nil {
				cbft.log.Error("Async Execute block failed", "error", err)
			}
			cbft.state.SetExecuting(blockIndex+1, false)
		}
	}
}

// Each time a new vote is triggered, a new QC Block will be triggered, and a new one can be found by the commit block.
func (cbft *Cbft) findQCBlock() {
	index := cbft.state.MaxQCIndex()
	next := index + 1
	size := cbft.state.PrepareVoteLenByIndex(next)

	prepareQC := func() bool {
		return size >= cbft.threshold(cbft.validatorPool.Len(cbft.state.HighestQCBlock().NumberU64())) && cbft.state.HadSendPrepareVote().Had(next)
	}

	if prepareQC() {
		block := cbft.state.ViewBlockByIndex(next)
		qc := cbft.generatePrepareQC(cbft.state.AllPrepareVoteByIndex(next))
		cbft.insertQCBlock(block, qc)
		cbft.network.Broadcast(&protocols.BlockQuorumCert{BlockQC: qc})
	}

	// add failpoint
	failpoint.Inject("mock-findQCBlock-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-findQCBlock-panic")
		}
	})
	cbft.tryChangeView()
}

// Try commit a new block
func (cbft *Cbft) tryCommitNewBlock(lock *types.Block, commit *types.Block) {
	if lock == nil || commit == nil {
		cbft.log.Warn("Try commit failed", "hadLock", lock != nil, "hadCommit", commit != nil)
		return
	}
	highestqc := cbft.state.HighestQCBlock()
	_, oldCommit := cbft.state.HighestLockBlock(), cbft.state.HighestCommitBlock()

	// add failpoint
	failpoint.Inject("mock-tryCommitNewBlock-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-tryCommitNewBlock-panic")
		}
	})
	// Incremental commit block
	if oldCommit.NumberU64()+1 == commit.NumberU64() {
		_, qc := cbft.blockTree.FindBlockAndQC(commit.Hash(), commit.NumberU64())
		cbft.commitBlock(commit, qc)
		cbft.state.SetHighestLockBlock(lock)
		cbft.state.SetHighestCommitBlock(commit)
		cbft.bridge.UpdateChainState(highestqc, lock, commit)
		cbft.blockTree.PruneBlock(commit.Hash(), commit.NumberU64(), nil)
		cbft.blockTree.NewRoot(commit)
	} else {
		cbft.bridge.UpdateChainState(highestqc, nil, nil)
	}
}

// According to the current view QC situation, try to switch view
func (cbft *Cbft) tryChangeView() {
	// Had receive all qcs of current view
	block, qc := cbft.blockTree.FindBlockAndQC(cbft.state.HighestQCBlock().Hash(), cbft.state.HighestQCBlock().NumberU64())

	increasing := func() uint64 {
		return cbft.state.ViewNumber() + 1
	}

	enough := func() bool {
		return cbft.state.MaxQCIndex()+1 == cbft.config.Sys.Amount
	}

	// add failpoint
	failpoint.Inject("mock-tryChangeView-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-tryChangeView-panic")
		}
	})
	if enough() {
		cbft.log.Debug("Produce enough blocks, change view", "view", cbft.state.ViewString())
		cbft.changeView(cbft.state.Epoch(), increasing(), block, qc, nil)
		return
	}

	viewChangeQC := func() bool {
		if cbft.state.ViewChangeLen() >= cbft.threshold(cbft.validatorPool.Len(cbft.state.HighestQCBlock().NumberU64())) {
			return true
		}
		cbft.log.Debug("Try change view failed, had receive viewchange", "len", cbft.state.ViewChangeLen(), "view", cbft.state.ViewString())
		return false
	}

	if viewChangeQC() {
		cbft.log.Debug("Receive Enough viewChange, change view", "newEpoch", cbft.state.Epoch(), "newView", increasing())
		viewChangeQC := cbft.generateViewChangeQC(cbft.state.AllViewChange())
		_, _, _, number := viewChangeQC.MaxBlock()
		block, qc := cbft.blockTree.FindBlockAndQC(cbft.state.HighestQCBlock().Hash(), cbft.state.HighestQCBlock().NumberU64())
		if block.NumberU64() != 0 && (number > qc.BlockNumber) {
			//fixme get qc block
			cbft.log.Warn("Local node is behind other validators", "blockState", cbft.state.HighestBlockString(), "viewChangeQC", viewChangeQC.String())
			return
		}
		cbft.changeView(cbft.state.Epoch(), increasing(), block, qc, viewChangeQC)
	}

}

// change view
func (cbft *Cbft) changeView(epoch, viewNumber uint64, block *types.Block, qc *ctypes.QuorumCert, viewChangeQC *ctypes.ViewChangeQC) {
	interval := func() uint64 {
		if block.NumberU64() == 0 || qc.ViewNumber+1 != viewNumber {
			return 1
		} else {
			return uint64(cbft.config.Sys.Amount - qc.BlockIndex)
		}
	}
	cbft.state.ResetView(epoch, viewNumber)
	cbft.state.SetViewTimer(interval())
	cbft.state.SetLastViewChangeQC(viewChangeQC)
	// add failpoint
	failpoint.Inject("mock-changeView-panic", func() {
		if cbft.shouldFailPoint() {
			panic("mock-changeView-panic")
		}
	})
	// write confirmed viewChange info to wal
	if !cbft.isLoading() {
		cbft.bridge.ConfirmViewChange(epoch, viewNumber, block, qc, viewChangeQC)
	}
	cbft.clearInvalidBlocks(block)
	cbft.log = log.New("epoch", cbft.state.Epoch(), "view", cbft.state.ViewNumber())
	cbft.log.Debug(fmt.Sprintf("Current view deadline:%v", cbft.state.Deadline()))
}

// Clean up invalid blocks in the previous view
func (cbft *Cbft) clearInvalidBlocks(newBlock *types.Block) {
	var rollback []*types.Block
	newHead := newBlock.Header()
	for _, p := range cbft.state.HadSendPrepareVote().Peek() {
		if p.BlockNumber > newBlock.NumberU64() {
			block := cbft.state.ViewBlockByIndex(p.BlockIndex)
			rollback = append(rollback, block)
			cbft.blockCacheWriter.ClearCache(block)
		}
	}
	for _, p := range cbft.state.PendingPrepareVote().Peek() {
		if p.BlockNumber > newBlock.NumberU64() {
			block := cbft.state.ViewBlockByIndex(p.BlockIndex)
			rollback = append(rollback, block)
			cbft.blockCacheWriter.ClearCache(block)
		}
	}

	//todo proposer is myself
	cbft.txPool.ForkedReset(newHead, rollback)
}
