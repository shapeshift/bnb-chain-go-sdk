package types

import (
	"time"

	"github.com/tendermint/tendermint/crypto/merkle"
	libbytes "github.com/tendermint/tendermint/libs/bytes"
	"github.com/tendermint/tendermint/proto/tendermint/version"
)

// Block defines the atomic unit of a Tendermint blockchain.
type Block struct {
	Header `json:"header"`
}

//-----------------------------------------------------------------------------

// Header defines the structure of a Tendermint block header.
// NOTE: changes to the Header should be duplicated in:
// - header.Hash()
// - abci.Header
// - /docs/spec/blockchain/blockchain.md
type Header struct {
	// basic block info
	Version  version.Consensus `json:"version"`
	ChainID  string            `json:"chain_id"`
	Height   int64             `json:"height"`
	Time     time.Time         `json:"time"`
	NumTxs   int64             `json:"num_txs"`
	TotalTxs int64             `json:"total_txs"`

	// prev block info
	LastBlockID BlockID `json:"last_block_id"`

	// hashes of block data
	LastCommitHash libbytes.HexBytes `json:"last_commit_hash"` // commit from validators from the last block
	DataHash       libbytes.HexBytes `json:"data_hash"`        // transactions

	// hashes from the app output from the prev block
	ValidatorsHash     libbytes.HexBytes `json:"validators_hash"`      // validators for the current block
	NextValidatorsHash libbytes.HexBytes `json:"next_validators_hash"` // validators for the next block
	ConsensusHash      libbytes.HexBytes `json:"consensus_hash"`       // consensus params for current block
	AppHash            libbytes.HexBytes `json:"app_hash"`             // state after txs from the previous block
	LastResultsHash    libbytes.HexBytes `json:"last_results_hash"`    // root hash of all results from the txs from the previous block

	// consensus info
	EvidenceHash    libbytes.HexBytes `json:"evidence_hash"`    // evidence included in the block
	ProposerAddress Address           `json:"proposer_address"` // original proposer of the block
}

// Hash returns the hash of the header.
// It computes a Merkle tree from the header fields
// ordered as they appear in the Header.
// Returns nil if ValidatorHash is missing,
// since a Header is not valid unless there is
// a ValidatorsHash (corresponding to the validator set).
func (h *Header) Hash() libbytes.HexBytes {
	if h == nil || len(h.ValidatorsHash) == 0 {
		return nil
	}
	return merkle.HashFromByteSlices([][]byte{
		cdcEncode(h.Version),
		cdcEncode(h.ChainID),
		cdcEncode(h.Height),
		cdcEncode(h.Time),
		cdcEncode(h.NumTxs),
		cdcEncode(h.TotalTxs),
		cdcEncode(h.LastBlockID),
		cdcEncode(h.LastCommitHash),
		cdcEncode(h.DataHash),
		cdcEncode(h.ValidatorsHash),
		cdcEncode(h.NextValidatorsHash),
		cdcEncode(h.ConsensusHash),
		cdcEncode(h.AppHash),
		cdcEncode(h.LastResultsHash),
		cdcEncode(h.EvidenceHash),
		cdcEncode(h.ProposerAddress),
	})
}

//-----------------------------------------------------------------------------

type PartSetHeader struct {
	Total int               `json:"total"`
	Hash  libbytes.HexBytes `json:"hash"`
}

// BlockID defines the unique ID of a block as its Hash and its PartSetHeader
type BlockID struct {
	Hash        libbytes.HexBytes `json:"hash"`
	PartsHeader PartSetHeader     `json:"parts"`
}
