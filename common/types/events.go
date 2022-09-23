package types

import (
	amino "github.com/tendermint/go-amino"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/types"
)

func RegisterEventDatas(cdc *amino.Codec) {
	cdc.RegisterInterface((*types.TMEventData)(nil), nil)
	cdc.RegisterConcrete(EventDataNewBlockHeader{}, "tendermint/event/NewBlockHeader", nil)
	cdc.RegisterConcrete(EventDataTx{}, "tendermint/event/Tx", nil)
}

// light weight event for benchmarking
type EventDataNewBlockHeader struct {
	Header Header `json:"header"`

	ResultBeginBlock abci.ResponseBeginBlock `json:"result_begin_block"`
	ResultEndBlock   abci.ResponseEndBlock   `json:"result_end_block"`
}

// All txs fire EventDataTx
type EventDataTx struct {
	abci.TxResult
}
