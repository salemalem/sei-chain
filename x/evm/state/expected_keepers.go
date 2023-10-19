package state

import (
	"math/big"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	"github.com/ethereum/go-ethereum/common"
)

type EVMKeeper interface {
	PrefixStore(sdk.Context, []byte) sdk.KVStore
	PurgePrefix(sdk.Context, []byte)
	GetSeiAddress(sdk.Context, common.Address) (sdk.AccAddress, bool)
	BankKeeper() bankkeeper.Keeper
	GetBaseDenom(sdk.Context) string
	DeleteAddressMapping(sdk.Context, sdk.AccAddress, common.Address)
	GetBalance(sdk.Context, common.Address) uint64
	SetOrDeleteBalance(sdk.Context, common.Address, uint64)
	GetCode(sdk.Context, common.Address) []byte
	SetCode(sdk.Context, common.Address, []byte)
	GetCodeHash(sdk.Context, common.Address) common.Hash
	GetCodeSize(sdk.Context, common.Address) int
	GetState(sdk.Context, common.Address, common.Hash) common.Hash
	SetState(sdk.Context, common.Address, common.Hash, common.Hash)
	GetModuleBalance(sdk.Context) *big.Int
	AccountKeeper() *authkeeper.AccountKeeper
}
