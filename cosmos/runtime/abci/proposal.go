// SPDX-License-Identifier: MIT
//
// Copyright (c) 2024 Berachain Foundation
//
// Permission is hereby granted, free of charge, to any person obtaining
// a copy of this software and associated documentation files (the
// "Software"), to deal in the Software without restriction, including
// without limitation the rights to use, copy, modify, merge, publish,
// distribute, sublicense, and/or sell copies of the Software, and to
// permit persons to whom the Software is furnished to do so, subject to
// the following conditions:
//
// The above copyright notice and this permission notice shall be
// included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
// NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
// LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
// OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
// WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package abci

import (
	"time"

	"cosmossdk.io/log"

	"github.com/berachain/polaris/cosmos/runtime/chain"
	"github.com/berachain/polaris/cosmos/runtime/miner"

	cometabci "github.com/cometbft/cometbft/abci/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

var sleepOnce = false

// PrepareProposalHook is a function executed after the default Polaris prepare proposal handler. This will be used
// by World Engine EVM base shard implementation to inject side-channel messages.
type PrepareProposalHook func(
	ctx sdk.Context, req *cometabci.RequestPrepareProposal, resp *cometabci.ResponsePrepareProposal,
) (*cometabci.ResponsePrepareProposal, error)

// ProposalProvider is a struct that provides the abci functions required
// for validators to propose blocks and validators/full nodes to process
// said proposals.
type ProposalProvider struct {
	logger              log.Logger
	preBlocker          sdk.PreBlocker
	beginBlocker        sdk.BeginBlocker
	prepareProposalHook PrepareProposalHook
	wrappedMiner        *miner.Miner
	wrappedBlockchain   *chain.WrappedBlockchain

	// TODO: refactor validator commands out of the wbc and miner.
	// valCmdProcessor   *ValidatorCommands
	// *eth.ExecutionLayer
}

// NewProposalProvider creates a new ProposalProvider instance.
// It takes a miner.Miner and a chain.WrappedBlockchain as
// arguments and returns a pointer to the initialized ProposalProvider.
func NewProposalProvider(
	preBlocker sdk.PreBlocker, beginBlocker sdk.BeginBlocker, prepareProposal PrepareProposalHook,
	wrappedMiner *miner.Miner, wrappedBlockchain *chain.WrappedBlockchain,
	logger log.Logger,
) *ProposalProvider {
	return &ProposalProvider{
		preBlocker:          preBlocker,
		beginBlocker:        beginBlocker,
		prepareProposalHook: prepareProposal,
		wrappedMiner:        wrappedMiner,
		wrappedBlockchain:   wrappedBlockchain,
		logger:              logger,
	}
}

// PrepareProposal is responsible for preparing a proposal for the next block.
// It takes a context and a RequestPrepareProposal, simulates finalizing the block,
// and if successful, delegates the proposal preparation to the wrapped miner.
// It returns a ResponsePrepareProposal and an error if any occurs during the process.
func (pp *ProposalProvider) PrepareProposal(
	ctx sdk.Context, req *cometabci.RequestPrepareProposal,
) (*cometabci.ResponsePrepareProposal, error) {
	var (
		start  = time.Now()
		height = ctx.BlockHeight()
	)

	pp.logger.Info(
		"entering prepare proposal",
		"timestamp", start, "height", height)
	defer func() {
		pp.logger.Info(
			"exiting prepare proposal",
			"timestamp", time.Now(),
			"duration", time.Since(start),
			"height", height)
	}()

	if err := pp.simulateFinalizeBlock(ctx, req); err != nil {
		return nil, err
	}

	// TODO: This is a hack to fix race condition with gRPC server initialization
	if !sleepOnce {
		sleepOnce = true
		pp.logger.Debug("Sleeping for 1s here to fix race condition")
		time.Sleep(1 * time.Second)
	}

	resp, err := pp.wrappedMiner.PrepareProposal(ctx, req)
	if err != nil {
		return nil, err
	}

	// Executes injected custom prepare proposal hook.
	return pp.prepareProposalHook(ctx, req, resp)
}

// ProcessProposal processes the incoming proposal.
// It takes a context and a RequestProcessProposal, simulates finalizing the block,
// and if successful, delegates the proposal processing to the wrapped blockchain.
// It returns a ResponseProcessProposal and an error if any occurs during the process.
func (pp *ProposalProvider) ProcessProposal(
	ctx sdk.Context, req *cometabci.RequestProcessProposal,
) (*cometabci.ResponseProcessProposal, error) {
	var (
		start  = time.Now()
		height = ctx.BlockHeight()
	)

	pp.logger.Info(
		"entering process proposal",
		"timestamp", start, "height", height)
	defer func() {
		pp.logger.Info(
			"exiting process proposal",
			"timestamp", time.Now(),
			"duration", time.Since(start),
			"height", height)
	}()

	if err := pp.simulateFinalizeBlock(ctx, req); err != nil {
		return nil, err
	}

	// We set this preblocked, beginblocked, and processed context to the state plugin factory for
	// queries on the node.
	spf := pp.wrappedBlockchain.StatePluginFactory()

	// Technically a race condition here, between here and emitting the chain head
	// event but it is so small and the network latency will most definitely overshadow.
	defer spf.SetLatestQueryContext(ctx)

	// Set the insert chain context for processing the block. NOTE: We insert to the chain but do
	// NOT set the chain head using this context.
	spf.SetInsertChainContext(ctx)
	pp.wrappedBlockchain.PrimePlugins(ctx)

	return pp.wrappedBlockchain.ProcessProposal(ctx, req)
}

// simulateFinalizeBlock simulates the execution of a block.
// We have to run the PreBlocker && BeginBlocker to get the chain into the state
// it'll be in when the EVM transaction actually runs.
func (pp *ProposalProvider) simulateFinalizeBlock(ctx sdk.Context, req abciRequest) error {
	if _, err := pp.preBlocker(ctx, &cometabci.RequestFinalizeBlock{
		Txs:                req.GetTxs(),
		Time:               req.GetTime(),
		Misbehavior:        req.GetMisbehavior(),
		Height:             req.GetHeight(),
		NextValidatorsHash: req.GetNextValidatorsHash(),
		ProposerAddress:    req.GetProposerAddress(),
	}); err != nil {
		return err
	}

	if _, err := pp.beginBlocker(ctx); err != nil {
		return err
	}

	// First check for an abort signal after beginBlock, as it's the first place
	// we spend any significant amount of time.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		// continue
	}

	return nil
}

// NoopPrepareProposalHook is a PrepareProposalHook implementation
// that does nothing and simply returns the ResponsePrepareProposal as is.
func NoopPrepareProposalHook(
	_ sdk.Context, _ *cometabci.RequestPrepareProposal, resp *cometabci.ResponsePrepareProposal,
) (*cometabci.ResponsePrepareProposal, error) {
	return resp, nil
}
