// SPDX-License-Identifier: BUSL-1.1
//
// Copyright (C) 2023, Berachain Foundation. All rights reserved.
// Use of this software is govered by the Business Source License included
// in the LICENSE file of this repository and at www.mariadb.com/bsl11.
//
// ANY USE OF THE LICENSED WORK IN VIOLATION OF THIS LICENSE WILL AUTOMATICALLY
// TERMINATE YOUR RIGHTS UNDER THIS LICENSE FOR THE CURRENT AND ALL OTHER
// VERSIONS OF THE LICENSED WORK.
//
// THIS LICENSE DOES NOT GRANT YOU ANY RIGHT IN ANY TRADEMARK OR LOGO OF
// LICENSOR OR ITS AFFILIATES (PROVIDED THAT YOU MAY USE A TRADEMARK OR LOGO OF
// LICENSOR AS EXPRESSLY REQUIRED BY THIS LICENSE).
//
// TO THE EXTENT PERMITTED BY APPLICABLE LAW, THE LICENSED WORK IS PROVIDED ON
// AN “AS IS” BASIS. LICENSOR HEREBY DISCLAIMS ALL WARRANTIES AND CONDITIONS,
// EXPRESS OR IMPLIED, INCLUDING (WITHOUT LIMITATION) WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, NON-INFRINGEMENT, AND
// TITLE.

package prepare

import (
	abci "github.com/cometbft/cometbft/abci/types"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/miner"

	"pkg.berachain.dev/polaris/eth/polar"
)

type (
	// ProposalTxVerifier defines the interface that is implemented by BaseApp,
	// that any custom ABCI PrepareProposal and ProcessProposal handler can use
	// to verify a transaction.
	TxVerifier interface {
		PrepareProposalVerifyTx(tx sdk.Tx) ([]byte, error)
		ProcessProposalVerifyTx(txBz []byte) (sdk.Tx, error)
	}
)

// TxSerializer provides an interface to Serialize Geth Transactions to Bytes (via sdk.Tx).
type TxSerializer interface {
	SerializeToSdkTx(signedTx *types.Transaction) (sdk.Tx, error)
}

// Handler provides the PrepareProposal function to allow a cosmos app to utilize the
// geth txpool.
type Handler struct {
	polaris    *polar.Polaris
	txVerifier TxVerifier
	serializer TxSerializer
}

// NewHandler returns a new Handler.
func NewHandler(polaris *polar.Polaris, txVerifier TxVerifier) *Handler {
	return &Handler{
		polaris:    polaris,
		txVerifier: txVerifier,
	}
}

// Init initializes the handler.
func (h *Handler) Init(serializer TxSerializer) {
	h.serializer = serializer
}

// PrepareProposal implements baseapp.PrepareProposalHandler.
func (h *Handler) PrepareProposal(
	ctx sdk.Context, req *abci.RequestPrepareProposal,
) (*abci.ResponsePrepareProposal, error) {
	var maxBlockGas int64
	if b := ctx.ConsensusParams().Block; b != nil {
		maxBlockGas = b.MaxGas
	}

	var (
		selectedTxs  [][]byte
		totalTxBytes int64
		totalTxGas   uint64
	)
	txs := h.txPoolTransactions()

	for lazyTx := txs.Peek(); lazyTx != nil; lazyTx = txs.Peek() {
		tx := lazyTx.Resolve()
		sdkTx, err := h.serializer.SerializeToSdkTx(tx)
		if err != nil {
			ctx.Logger().Error("Failed sdk.Tx Serialization", tx.Hash(), err)
			continue
		}

		var bz []byte
		bz, _ = h.txVerifier.PrepareProposalVerifyTx(sdkTx)
		txGasLimit := tx.Gas()
		txSize := int64(len(bz))

		// only add the transaction to the proposal if we have enough capacity
		if (txSize + totalTxBytes) < req.MaxTxBytes {
			// If there is a max block gas limit, add the tx only if the limit has
			// not been met.
			if maxBlockGas > 0 {
				if (txGasLimit + totalTxGas) <= uint64(maxBlockGas) {
					totalTxGas += txGasLimit
					totalTxBytes += txSize
					selectedTxs = append(selectedTxs, bz)
				}
			} else {
				totalTxBytes += txSize
				selectedTxs = append(selectedTxs, bz)
			}
		}

		// Check if we've reached capacity. If so, we cannot select any more
		// transactions.
		if totalTxBytes >= req.MaxTxBytes ||
			(maxBlockGas > 0 && (totalTxGas >= uint64(maxBlockGas))) {
			break
		}

		// Shift the transaction off the queue.
		txs.Shift()
	}

	return &abci.ResponsePrepareProposal{Txs: selectedTxs}, nil
}

// txPoolTransactions returns a sorted list of transactions from the txpool.
func (h *Handler) txPoolTransactions() *miner.TransactionsByPriceAndNonce {
	return miner.NewTransactionsByPriceAndNonce(types.LatestSigner(
		h.polaris.Host().GetConfigurationPlugin().ChainConfig(),
	), h.polaris.TxPool().Pending(false), h.polaris.Miner().NextBaseFee())
}