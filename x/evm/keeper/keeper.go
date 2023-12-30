package keeper

import (
	"fmt"
	"math"
	"math/big"
	"slices"
	"sync"

	"github.com/cosmos/cosmos-sdk/store/prefix"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	paramtypes "github.com/cosmos/cosmos-sdk/x/params/types"
	stakingkeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/vm"
	lru "github.com/hashicorp/golang-lru/v2/simplelru"
	"github.com/sei-protocol/sei-chain/x/evm/types"
	tmtypes "github.com/tendermint/tendermint/types"
)

var zeroAddress = common.HexToAddress("0x0000000000000000000000000000000000000000")

type Keeper struct {
	storeKey   sdk.StoreKey
	Paramstore paramtypes.Subspace

	bankKeeper    bankkeeper.Keeper
	accountKeeper *authkeeper.AccountKeeper
	stakingKeeper *stakingkeeper.Keeper

	cachedFeeCollectorAddressMtx *sync.RWMutex
	cachedFeeCollectorAddress    *common.Address
	evmTxIndicesMtx              *sync.Mutex
	evmTxIndices                 []int
	nonceMx                      *sync.RWMutex
	pendingNonces                map[string][]uint64
	completedNonces              *lru.LRU[string, bool]
}

func NewKeeper(
	storeKey sdk.StoreKey, paramstore paramtypes.Subspace,
	bankKeeper bankkeeper.Keeper, accountKeeper *authkeeper.AccountKeeper, stakingKeeper *stakingkeeper.Keeper) *Keeper {
	if !paramstore.HasKeyTable() {
		paramstore = paramstore.WithKeyTable(types.ParamKeyTable())
	}
	// needs to be bounded to avoid leaking forever
	cn, err := lru.NewLRU[string, bool](100000, nil)
	if err != nil {
		panic(fmt.Sprintf("could not create lru: %v", err))
	}
	k := &Keeper{
		storeKey:                     storeKey,
		Paramstore:                   paramstore,
		bankKeeper:                   bankKeeper,
		accountKeeper:                accountKeeper,
		stakingKeeper:                stakingKeeper,
		evmTxIndices:                 []int{},
		pendingNonces:                make(map[string][]uint64),
		completedNonces:              cn,
		nonceMx:                      &sync.RWMutex{},
		evmTxIndicesMtx:              &sync.Mutex{},
		cachedFeeCollectorAddressMtx: &sync.RWMutex{},
	}
	return k
}

func (k *Keeper) AccountKeeper() *authkeeper.AccountKeeper {
	return k.accountKeeper
}

func (k *Keeper) BankKeeper() bankkeeper.Keeper {
	return k.bankKeeper
}

func (k *Keeper) GetStoreKey() sdk.StoreKey {
	return k.storeKey
}

func (k *Keeper) PrefixStore(ctx sdk.Context, pref []byte) sdk.KVStore {
	store := ctx.KVStore(k.GetStoreKey())
	return prefix.NewStore(store, pref)
}

func (k *Keeper) PurgePrefix(ctx sdk.Context, pref []byte) {
	store := k.PrefixStore(ctx, pref)
	iter := store.Iterator(nil, nil)
	keys := [][]byte{}
	for ; iter.Valid(); iter.Next() {
		keys = append(keys, iter.Key())
	}
	iter.Close()
	for _, key := range keys {
		store.Delete(key)
	}
}

func (k *Keeper) GetVMBlockContext(ctx sdk.Context, gp core.GasPool) (*vm.BlockContext, error) {
	coinbase, err := k.GetFeeCollectorAddress(ctx)
	if err != nil {
		return nil, err
	}
	r, err := ctx.BlockHeader().Time.MarshalBinary()
	if err != nil {
		return nil, err
	}
	rh := common.BytesToHash(r)
	return &vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     k.GetHashFn(ctx),
		Coinbase:    coinbase,
		GasLimit:    gp.Gas(),
		BlockNumber: big.NewInt(ctx.BlockHeight()),
		Time:        uint64(ctx.BlockHeader().Time.Unix()),
		Difficulty:  big.NewInt(0),                               // only needed for PoW
		BaseFee:     k.GetBaseFeePerGas(ctx).RoundInt().BigInt(), // feemarket not enabled
		Random:      &rh,
	}, nil
}

// returns a function that provides block header hash based on block number
func (k *Keeper) GetHashFn(ctx sdk.Context) vm.GetHashFunc {
	return func(height uint64) common.Hash {
		if height > math.MaxInt64 {
			ctx.Logger().Error("Sei block height is bounded by int64 range")
			return common.Hash{}
		}
		h := int64(height)
		if ctx.BlockHeight() == h {
			// current header hash is in the context already
			return common.BytesToHash(ctx.HeaderHash())
		}
		if ctx.BlockHeight() < h {
			// future block doesn't have a hash yet
			return common.Hash{}
		}
		// fetch historical hash from historical info
		return k.getHistoricalHash(ctx, h)
	}
}

func (k *Keeper) ClearEVMTxIndices() {
	// no need to acquire mutex here since it's only called by BeginBlock
	k.evmTxIndices = []int{}
}

func (k *Keeper) GetEVMTxIndices() []int {
	// no need to acquire mutex here since it's only called by EndBlock
	return k.evmTxIndices
}

func (k *Keeper) AppendToEVMTxIndices(idx int) {
	k.evmTxIndicesMtx.Lock()
	defer k.evmTxIndicesMtx.Unlock()
	k.evmTxIndices = append(k.evmTxIndices, idx)
}

func (k *Keeper) getHistoricalHash(ctx sdk.Context, h int64) common.Hash {
	histInfo, found := k.stakingKeeper.GetHistoricalInfo(ctx, h)
	if !found {
		// too old, already pruned
		return common.Hash{}
	}
	header, _ := tmtypes.HeaderFromProto(&histInfo.Header)

	return common.BytesToHash(header.Hash())
}

// nonceCacheKey is a helper function to create a key for the completed nonces cache
func nonceCacheKey(addr common.Address, nonce uint64) string {
	return fmt.Sprintf("%s|%d", addr.Hex(), nonce)
}

// CalculateNextNonce calculates the next nonce for an address
// If includePending is true, it will consider pending nonces
// If includePending is false, it will only return the latest nonce
func (k *Keeper) CalculateNextNonce(ctx sdk.Context, addr common.Address, includePending bool) uint64 {
	k.nonceMx.Lock()
	defer k.nonceMx.Unlock()

	latest := k.GetNonce(ctx, addr)
	nextNonce := latest

	// we only want the latest nonce if we're not including pending
	if !includePending {
		return nextNonce
	}

	// get the pending nonces (nil is fine)
	pending, _ := k.pendingNonces[addr.Hex()]

	// Check each nonce starting from latest until we find a gap
	// That gap is the next nonce we should use.
	// The completed nonces are limited to 100k entries
	for {
		// if it's not in pending and not completed, then it's the next nonce
		if !sortedListContains(pending, nextNonce) && !k.completedNonces.Contains(nonceCacheKey(addr, nextNonce)) {
			return nextNonce
		}
		nextNonce++
	}
}

// sortedListContains is a helper function to check if a sorted slice contains a specific element
func sortedListContains(slice []uint64, item uint64) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
		// because it's sorted, we can bail if it's higher
		if v > item {
			return false
		}
	}
	return false
}

// AddPendingNonce adds a pending nonce to the keeper
func (k *Keeper) AddPendingNonce(addr common.Address, nonce uint64) {
	k.nonceMx.Lock()
	defer k.nonceMx.Unlock()
	addrStr := addr.Hex()
	k.pendingNonces[addrStr] = append(k.pendingNonces[addrStr], nonce)
	slices.Sort(k.pendingNonces[addrStr])
}

// RemovePendingNonce removes a pending nonce from the keeper
// success means this transaction was processed and this nonce is used
func (k *Keeper) RemovePendingNonce(addr common.Address, nonce uint64, success bool) {
	// geth calls this with the burn address and there isn't any reason to track it
	if addr == zeroAddress {
		return
	}
	k.nonceMx.Lock()
	defer k.nonceMx.Unlock()

	addrStr := addr.Hex()

	if success {
		k.completedNonces.Add(nonceCacheKey(addr, nonce), true)
	}

	if _, ok := k.pendingNonces[addrStr]; !ok {
		return
	}

	for i, n := range k.pendingNonces[addrStr] {
		if success && n >= nonce {
			copy(k.pendingNonces[addrStr], k.pendingNonces[addrStr][i+1:])
			k.pendingNonces[addrStr] = k.pendingNonces[addrStr][:len(k.pendingNonces[addrStr])-i-1]

			// If the slice is empty, delete the key from the map
			if len(k.pendingNonces[addrStr]) == 0 {
				delete(k.pendingNonces, addrStr)
			}

			return
		} else if !success && n == nonce {
			// only remove that one item (it is eligible to be used)
			k.pendingNonces[addrStr] = append(k.pendingNonces[addrStr][:i], k.pendingNonces[addrStr][i+1:]...)
			return
		}
	}
}
