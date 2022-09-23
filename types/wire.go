package types

import (
	ntypes "github.com/shapeshift/bnb-chain-go-sdk/common/types"
	"github.com/shapeshift/bnb-chain-go-sdk/types/tx"
	"github.com/tendermint/go-amino"
)

func NewCodec() *amino.Codec {
	cdc := amino.NewCodec()
	ntypes.RegisterWire(cdc)
	tx.RegisterCodec(cdc)
	return cdc
}
