package rescue

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/node"
	"github.com/holiman/uint256"
)

func TestLocalChain_PragueSweepGasLimits(t *testing.T) {
	for _, test := range []struct {
		name              string
		token             bool
		fundedDestination bool
		gasLimit          uint64
		wantEstimateError bool
	}{
		{name: "native existing destination", fundedDestination: true, gasLimit: 100_000},
		{name: "native empty destination", gasLimit: 100_000},
		{name: "native empty destination rejects 60000", gasLimit: 60_000, wantEstimateError: true},
		{name: "standard token cold destination slot", token: true, gasLimit: 180_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			sourceKey := localChainPrivateKey(t, localChainSourceKeyHex)
			sponsorKey := localChainPrivateKey(t, localChainSponsorKeyHex)
			source := crypto.PubkeyToAddress(sourceKey.PublicKey)
			sponsor := crypto.PubkeyToAddress(sponsorKey.PublicKey)
			destination := crypto.PubkeyToAddress(localChainPrivateKey(t, localChainDestinationKeyHex).PublicKey)
			alloc := types.GenesisAlloc{
				source:  {Balance: localChainEther(1)},
				sponsor: {Balance: localChainEther(20)},
			}
			if test.fundedDestination {
				alloc[destination] = types.Account{Balance: big.NewInt(1)}
			}
			token := common.Address{0xa1}
			if test.token {
				content, err := os.ReadFile("testdata/claim-contracts.json")
				if err != nil {
					t.Fatal(err)
				}
				var fixture map[string]string
				if err := json.Unmarshal(content, &fixture); err != nil {
					t.Fatal(err)
				}
				balanceSlot := crypto.Keccak256Hash(common.LeftPadBytes(source.Bytes(), 32), common.LeftPadBytes([]byte{1}, 32))
				alloc[token] = types.Account{
					Balance: new(big.Int), Code: hexutil.MustDecode(fixture["StandardToken"]),
					Storage: map[common.Hash]common.Hash{balanceSlot: common.BigToHash(big.NewInt(100))},
				}
			}
			backend := simulated.NewBackend(alloc, func(_ *node.Config, eth *ethconfig.Config) {
				// Pin Prague: the simulated backend also enables later Ethereum gas repricing by default.
				chain := *eth.Genesis.Config
				chain.OsakaTime = nil
				chain.BogotaTime = nil
				chain.AmsterdamTime = nil
				eth.Genesis.Config = &chain
			})
			t.Cleanup(func() {
				if err := backend.Close(); err != nil {
					t.Error(err)
				}
			})
			contractABI, rescuer, nonce := localChainDeployRescuer(t, ctx, backend, sponsorKey, destination)
			data, err := contractABI.Pack("sweepEth")
			if test.token {
				data, err = contractABI.Pack("sweepAll", []common.Address{token})
			}
			if err != nil {
				t.Fatal(err)
			}
			authorization, err := types.SignSetCode(sourceKey, types.SetCodeAuthorization{
				ChainID: *uint256.NewInt(uint64(localChainID)), Address: rescuer,
			})
			if err != nil {
				t.Fatal(err)
			}
			request := EIP7702SimulationRequest{
				Sponsor: sponsor, Source: source, GasLimit: 300_000,
				GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
				Data: data, AuthorizationList: []types.SetCodeAuthorization{authorization},
			}
			estimate, err := EstimateEIP7702Gas(ctx, backend.Client(), request)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("unrestricted gas estimate: %d", estimate)
			request.GasLimit = test.gasLimit
			_, err = EstimateEIP7702Gas(ctx, backend.Client(), request)
			if test.wantEstimateError {
				if !errors.Is(err, ErrEstimateGasFailed) {
					t.Fatalf("insufficient gas limit %d was not rejected: %v", test.gasLimit, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("gas limit %d: %v", test.gasLimit, err)
			}
			tip, _ := uint256.FromBig(request.GasTipCap)
			fee, _ := uint256.FromBig(request.GasFeeCap)
			transaction := types.NewTx(&types.SetCodeTx{
				ChainID: uint256.NewInt(uint64(localChainID)), Nonce: nonce, To: source,
				Gas: test.gasLimit, GasTipCap: tip, GasFeeCap: fee, Value: new(uint256.Int),
				Data: data, AuthList: request.AuthorizationList,
			})
			transaction, err = types.SignTx(transaction, types.LatestSignerForChainID(transaction.ChainId()), sponsorKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Client().SendTransaction(ctx, transaction); err != nil {
				t.Fatal(err)
			}
			backend.Commit()
			receipt, err := backend.Client().TransactionReceipt(ctx, transaction.Hash())
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Status != types.ReceiptStatusSuccessful {
				t.Fatalf("sweep failed with gas limit %d: gas used %d", test.gasLimit, receipt.GasUsed)
			}
			t.Logf("gas used: %d, gas limit: %d", receipt.GasUsed, test.gasLimit)
		})
	}
}
