// stm: #integration
package itests

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/crypto/sha3"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/store"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/types/ethtypes"
	"github.com/filecoin-project/lotus/itests/kit"
)

// SolidityContractDef holds information about one of the test contracts
type SolidityContractDef struct {
	Filename string            // filename of the hex of the contract, e.g. contracts/EventMatrix.hex
	Fn       map[string][]byte // mapping of function names to 32-bit selector
	Ev       map[string][]byte // mapping of event names to 256-bit signature hashes
}

var EventMatrixContract = SolidityContractDef{
	Filename: "contracts/EventMatrix.hex",
	Fn: map[string][]byte{
		"logEventZeroData":             ethFunctionHash("logEventZeroData()"),
		"logEventOneData":              ethFunctionHash("logEventOneData(uint256)"),
		"logEventTwoData":              ethFunctionHash("logEventTwoData(uint256,uint256)"),
		"logEventThreeData":            ethFunctionHash("logEventThreeData(uint256,uint256,uint256)"),
		"logEventFourData":             ethFunctionHash("logEventFourData(uint256,uint256,uint256,uint256)"),
		"logEventOneIndexed":           ethFunctionHash("logEventOneIndexed(uint256)"),
		"logEventTwoIndexed":           ethFunctionHash("logEventTwoIndexed(uint256,uint256)"),
		"logEventThreeIndexed":         ethFunctionHash("logEventThreeIndexed(uint256,uint256,uint256)"),
		"logEventOneIndexedWithData":   ethFunctionHash("logEventOneIndexedWithData(uint256,uint256)"),
		"logEventTwoIndexedWithData":   ethFunctionHash("logEventTwoIndexedWithData(uint256,uint256,uint256)"),
		"logEventThreeIndexedWithData": ethFunctionHash("logEventThreeIndexedWithData(uint256,uint256,uint256,uint256)"),
	},
	Ev: map[string][]byte{
		"EventZeroData":             ethTopicHash("EventZeroData()"),
		"EventOneData":              ethTopicHash("EventOneData(uint256)"),
		"EventTwoData":              ethTopicHash("EventTwoData(uint256,uint256)"),
		"EventThreeData":            ethTopicHash("EventThreeData(uint256,uint256,uint256)"),
		"EventFourData":             ethTopicHash("EventFourData(uint256,uint256,uint256,uint256)"),
		"EventOneIndexed":           ethTopicHash("EventOneIndexed(uint256)"),
		"EventTwoIndexed":           ethTopicHash("EventTwoIndexed(uint256,uint256)"),
		"EventThreeIndexed":         ethTopicHash("EventThreeIndexed(uint256,uint256,uint256)"),
		"EventOneIndexedWithData":   ethTopicHash("EventOneIndexedWithData(uint256,uint256)"),
		"EventTwoIndexedWithData":   ethTopicHash("EventTwoIndexedWithData(uint256,uint256,uint256)"),
		"EventThreeIndexedWithData": ethTopicHash("EventThreeIndexedWithData(uint256,uint256,uint256,uint256)"),
	},
}

var EventsContract = SolidityContractDef{
	Filename: "contracts/events.bin",
	Fn: map[string][]byte{
		"log_zero_data":   {0x00, 0x00, 0x00, 0x00},
		"log_zero_nodata": {0x00, 0x00, 0x00, 0x01},
		"log_four_data":   {0x00, 0x00, 0x00, 0x02},
	},
	Ev: map[string][]byte{},
}

func TestEthNewPendingTransactionFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	kit.QuietAllLogsExcept("events", "messagepool")

	client, _, ens := kit.EnsembleMinimal(t, kit.MockProofs(), kit.ThroughRPC(), kit.RealTimeFilterAPI())
	ens.InterconnectAll().BeginMining(10 * time.Millisecond)

	// create a new address where to send funds.
	addr, err := client.WalletNew(ctx, types.KTBLS)
	require.NoError(t, err)

	// get the existing balance from the default wallet to then split it.
	bal, err := client.WalletBalance(ctx, client.DefaultKey.Address)
	require.NoError(t, err)

	// install filter
	filterID, err := client.EthNewPendingTransactionFilter(ctx)
	require.NoError(t, err)

	const iterations = 100

	// we'll send half our balance (saving the other half for gas),
	// in `iterations` increments.
	toSend := big.Div(bal, big.NewInt(2))
	each := big.Div(toSend, big.NewInt(iterations))

	waitAllCh := make(chan struct{})
	go func() {
		headChangeCh, err := client.ChainNotify(ctx)
		require.NoError(t, err)
		<-headChangeCh // skip hccurrent

		defer func() {
			close(waitAllCh)
		}()

		count := 0
		for {
			select {
			case <-ctx.Done():
				return
			case headChanges := <-headChangeCh:
				for _, change := range headChanges {
					if change.Type == store.HCApply {
						msgs, err := client.ChainGetMessagesInTipset(ctx, change.Val.Key())
						require.NoError(t, err)
						count += len(msgs)
						if count == iterations {
							return
						}
					}
				}
			}
		}
	}()

	var sms []*types.SignedMessage
	for i := 0; i < iterations; i++ {
		msg := &types.Message{
			From:  client.DefaultKey.Address,
			To:    addr,
			Value: each,
		}

		sm, err := client.MpoolPushMessage(ctx, msg, nil)
		require.NoError(t, err)
		require.EqualValues(t, i, sm.Message.Nonce)

		sms = append(sms, sm)
	}

	select {
	case <-waitAllCh:
	case <-ctx.Done():
		t.Errorf("timeout waiting to pack messages")
	}

	expected := make(map[string]bool)
	for _, sm := range sms {
		hash, err := ethtypes.EthHashFromCid(sm.Cid())
		require.NoError(t, err)
		expected[hash.String()] = false
	}

	// collect filter results
	res, err := client.EthGetFilterChanges(ctx, filterID)
	require.NoError(t, err)

	// expect to have seen iteration number of mpool messages
	require.Equal(t, iterations, len(res.Results), "expected %d tipsets to have been executed", iterations)

	require.Equal(t, len(res.Results), len(expected), "expected number of filter results to equal number of messages")

	for _, txid := range res.Results {
		expected[txid.(string)] = true
	}

	for _, found := range expected {
		require.True(t, found)
	}
}

func TestEthNewBlockFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	kit.QuietAllLogsExcept("events", "messagepool")

	client, _, ens := kit.EnsembleMinimal(t, kit.MockProofs(), kit.ThroughRPC(), kit.RealTimeFilterAPI())
	ens.InterconnectAll().BeginMining(10 * time.Millisecond)

	// create a new address where to send funds.
	addr, err := client.WalletNew(ctx, types.KTBLS)
	require.NoError(t, err)

	// get the existing balance from the default wallet to then split it.
	bal, err := client.WalletBalance(ctx, client.DefaultKey.Address)
	require.NoError(t, err)

	// install filter
	filterID, err := client.EthNewBlockFilter(ctx)
	require.NoError(t, err)

	const iterations = 30

	// we'll send half our balance (saving the other half for gas),
	// in `iterations` increments.
	toSend := big.Div(bal, big.NewInt(2))
	each := big.Div(toSend, big.NewInt(iterations))

	waitAllCh := make(chan struct{})
	tipsetChan := make(chan *types.TipSet, iterations)
	go func() {
		headChangeCh, err := client.ChainNotify(ctx)
		require.NoError(t, err)
		<-headChangeCh // skip hccurrent

		defer func() {
			close(tipsetChan)
			close(waitAllCh)
		}()

		count := 0
		for {
			select {
			case <-ctx.Done():
				return
			case headChanges := <-headChangeCh:
				for _, change := range headChanges {
					if change.Type == store.HCApply || change.Type == store.HCRevert {
						count++
						tipsetChan <- change.Val
						if count == iterations {
							return
						}
					}
				}
			}
		}
	}()

	for i := 0; i < iterations; i++ {
		msg := &types.Message{
			From:  client.DefaultKey.Address,
			To:    addr,
			Value: each,
		}

		sm, err := client.MpoolPushMessage(ctx, msg, nil)
		require.NoError(t, err)
		require.EqualValues(t, i, sm.Message.Nonce)
	}

	select {
	case <-waitAllCh:
	case <-ctx.Done():
		t.Errorf("timeout waiting to pack messages")
	}

	expected := make(map[string]bool)
	for ts := range tipsetChan {
		c, err := ts.Key().Cid()
		require.NoError(t, err)
		hash, err := ethtypes.EthHashFromCid(c)
		require.NoError(t, err)
		expected[hash.String()] = false
	}

	// collect filter results
	res, err := client.EthGetFilterChanges(ctx, filterID)
	require.NoError(t, err)

	// expect to have seen iteration number of tipsets
	require.Equal(t, iterations, len(res.Results), "expected %d tipsets to have been executed", iterations)

	require.Equal(t, len(res.Results), len(expected), "expected number of filter results to equal number of tipsets")

	for _, blockhash := range res.Results {
		expected[blockhash.(string)] = true
	}

	for _, found := range expected {
		require.True(t, found, "expected all tipsets to be present in filter results")
	}
}

func TestEthNewFilterCatchAll(t *testing.T) {
	require := require.New(t)

	kit.QuietAllLogsExcept("events", "messagepool")

	blockTime := 100 * time.Millisecond
	client, _, ens := kit.EnsembleMinimal(t, kit.MockProofs(), kit.ThroughRPC(), kit.RealTimeFilterAPI(), kit.EthTxHashLookup())
	ens.InterconnectAll().BeginMining(blockTime)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// install contract
	contractHex, err := os.ReadFile("contracts/events.bin")
	require.NoError(err)

	contract, err := hex.DecodeString(string(contractHex))
	require.NoError(err)

	fromAddr, err := client.WalletDefaultAddress(ctx)
	require.NoError(err)

	result := client.EVM().DeployContract(ctx, fromAddr, contract)

	idAddr, err := address.NewIDAddress(result.ActorID)
	require.NoError(err)
	t.Logf("actor ID address is %s", idAddr)

	// install filter
	filterID, err := client.EthNewFilter(ctx, &ethtypes.EthFilterSpec{})
	require.NoError(err)

	const iterations = 3
	ethContractAddr, received := invokeLogFourData(t, client, iterations)

	// collect filter results
	res, err := client.EthGetFilterChanges(ctx, filterID)
	require.NoError(err)

	// expect to have seen iteration number of events
	require.Equal(iterations, len(res.Results))

	expected := []ExpectedEthLog{
		{
			Address: ethContractAddr,
			Topics: []ethtypes.EthBytes{
				paddedEthBytes([]byte{0x11, 0x11}),
				paddedEthBytes([]byte{0x22, 0x22}),
				paddedEthBytes([]byte{0x33, 0x33}),
				paddedEthBytes([]byte{0x44, 0x44}),
			},
			Data: paddedEthBytes([]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}),
		},
		{
			Address: ethContractAddr,
			Topics: []ethtypes.EthBytes{
				paddedEthBytes([]byte{0x11, 0x11}),
				paddedEthBytes([]byte{0x22, 0x22}),
				paddedEthBytes([]byte{0x33, 0x33}),
				paddedEthBytes([]byte{0x44, 0x44}),
			},
			Data: paddedEthBytes([]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}),
		},
		{
			Address: ethContractAddr,
			Topics: []ethtypes.EthBytes{
				paddedEthBytes([]byte{0x11, 0x11}),
				paddedEthBytes([]byte{0x22, 0x22}),
				paddedEthBytes([]byte{0x33, 0x33}),
				paddedEthBytes([]byte{0x44, 0x44}),
			},
			Data: paddedEthBytes([]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}),
		},
	}

	elogs, err := parseEthLogsFromFilterResult(res)
	require.NoError(err)
	AssertEthLogs(t, elogs, expected, received)
}

func TestEthGetLogsAll(t *testing.T) {
	require := require.New(t)
	kit.QuietAllLogsExcept("events", "messagepool")

	blockTime := 100 * time.Millisecond
	dbpath := filepath.Join(t.TempDir(), "actorevents.db")

	client, _, ens := kit.EnsembleMinimal(t, kit.MockProofs(), kit.ThroughRPC(), kit.HistoricFilterAPI(dbpath))
	ens.InterconnectAll().BeginMining(blockTime)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	invocations := 1
	ethContractAddr, received := invokeLogFourData(t, client, invocations)

	// Build filter spec
	spec := newEthFilterBuilder().
		FromBlockEpoch(0).
		Topic1OneOf(paddedEthHash([]byte{0x11, 0x11})).
		Filter()

	expected := []ExpectedEthLog{
		{
			Address: ethContractAddr,
			Topics: []ethtypes.EthBytes{
				paddedEthBytes([]byte{0x11, 0x11}),
				paddedEthBytes([]byte{0x22, 0x22}),
				paddedEthBytes([]byte{0x33, 0x33}),
				paddedEthBytes([]byte{0x44, 0x44}),
			},
			Data: paddedEthBytes([]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}),
		},
	}

	// Use filter
	res, err := client.EthGetLogs(ctx, spec)
	require.NoError(err)

	elogs, err := parseEthLogsFromFilterResult(res)
	require.NoError(err)
	AssertEthLogs(t, elogs, expected, received)
}

func TestEthGetLogsByTopic(t *testing.T) {
	require := require.New(t)

	kit.QuietAllLogsExcept("events", "messagepool")

	blockTime := 100 * time.Millisecond
	dbpath := filepath.Join(t.TempDir(), "actorevents.db")

	client, _, ens := kit.EnsembleMinimal(t, kit.MockProofs(), kit.ThroughRPC(), kit.HistoricFilterAPI(dbpath))
	ens.InterconnectAll().BeginMining(blockTime)

	invocations := 1
	ethContractAddr, received := invokeLogFourData(t, client, invocations)

	// find log by known topic1
	var spec ethtypes.EthFilterSpec
	err := json.Unmarshal([]byte(`{"fromBlock":"0x0","topics":["0x0000000000000000000000000000000000000000000000000000000000001111"]}`), &spec)
	require.NoError(err)

	res, err := client.EthGetLogs(context.Background(), &spec)
	require.NoError(err)

	expected := []ExpectedEthLog{
		{
			Address: ethContractAddr,
			Topics: []ethtypes.EthBytes{
				paddedEthBytes([]byte{0x11, 0x11}),
				paddedEthBytes([]byte{0x22, 0x22}),
				paddedEthBytes([]byte{0x33, 0x33}),
				paddedEthBytes([]byte{0x44, 0x44}),
			},
			Data: paddedEthBytes([]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}),
		},
	}
	elogs, err := parseEthLogsFromFilterResult(res)
	require.NoError(err)
	AssertEthLogs(t, elogs, expected, received)
}

func TestEthSubscribeLogs(t *testing.T) {
	require := require.New(t)

	kit.QuietAllLogsExcept("events", "messagepool")

	blockTime := 100 * time.Millisecond
	client, _, ens := kit.EnsembleMinimal(t, kit.MockProofs(), kit.ThroughRPC(), kit.RealTimeFilterAPI())
	ens.InterconnectAll().BeginMining(blockTime)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// install contract
	contractHex, err := os.ReadFile("contracts/events.bin")
	require.NoError(err)

	contract, err := hex.DecodeString(string(contractHex))
	require.NoError(err)

	fromAddr, err := client.WalletDefaultAddress(ctx)
	require.NoError(err)

	result := client.EVM().DeployContract(ctx, fromAddr, contract)

	idAddr, err := address.NewIDAddress(result.ActorID)
	require.NoError(err)
	t.Logf("actor ID address is %s", idAddr)

	// install filter
	respCh, err := client.EthSubscribe(ctx, "logs", nil)
	require.NoError(err)

	subResponses := []ethtypes.EthSubscriptionResponse{}
	go func() {
		for resp := range respCh {
			subResponses = append(subResponses, resp)
		}
	}()

	const iterations = 10
	ethContractAddr, messages := invokeLogFourData(t, client, iterations)

	expected := make([]ExpectedEthLog, iterations)
	for i := range expected {
		expected[i] = ExpectedEthLog{
			Address: ethContractAddr,
			Topics: []ethtypes.EthBytes{
				paddedEthBytes([]byte{0x11, 0x11}),
				paddedEthBytes([]byte{0x22, 0x22}),
				paddedEthBytes([]byte{0x33, 0x33}),
				paddedEthBytes([]byte{0x44, 0x44}),
			},
			Data: paddedEthBytes([]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}),
		}
	}

	elogs, err := parseEthLogsFromSubscriptionResponses(subResponses)
	require.NoError(err)
	AssertEthLogs(t, elogs, expected, messages)
}

func TestEthGetLogs(t *testing.T) {
	require := require.New(t)
	kit.QuietAllLogsExcept("events", "messagepool")

	blockTime := 100 * time.Millisecond
	dbpath := filepath.Join(t.TempDir(), "actorevents.db")

	client, _, ens := kit.EnsembleMinimal(t, kit.MockProofs(), kit.ThroughRPC(), kit.HistoricFilterAPI(dbpath))
	ens.InterconnectAll().BeginMining(blockTime)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Set up the test fixture with a standard list of invocations
	contract1, contract2, messages := invokeEventMatrix(ctx, t, client)

	testCases := []struct {
		name     string
		spec     *ethtypes.EthFilterSpec
		expected []ExpectedEthLog
	}{
		{
			name: "find all EventZeroData events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventZeroData"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventZeroData"],
					},
					Data: nil,
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventZeroData"],
					},
					Data: nil,
				},
			},
		},
		{
			name: "find all EventOneData events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventOneData"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneData"],
					},
					Data: packUint64Values(23),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneData"],
					},
					Data: packUint64Values(44),
				},
			},
		},
		{
			name: "find all EventTwoData events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventTwoData"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoData"],
					},
					Data: packUint64Values(555, 666),
				},
			},
		},
		{
			name: "find all EventThreeData events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventThreeData"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventThreeData"],
					},
					Data: packUint64Values(1, 2, 3),
				},
			},
		},
		{
			name: "find all EventOneIndexed events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventOneIndexed"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexed"],
						paddedUint64(44),
					},
					Data: nil,
				},
			},
		},
		{
			name: "find all EventTwoIndexed events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventTwoIndexed"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexed"],
						paddedUint64(44),
						paddedUint64(19),
					},
					Data: nil,
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexed"],
						paddedUint64(40),
						paddedUint64(20),
					},
					Data: nil,
				},
			},
		},
		{
			name: "find all EventThreeIndexed events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventThreeIndexed"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventThreeIndexed"],
						paddedUint64(44),
						paddedUint64(27),
						paddedUint64(19),
					},
					Data: nil,
				},
			},
		},
		{
			name: "find all EventOneIndexedWithData events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventOneIndexedWithData"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(44),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(46),
					},
					Data: paddedUint64(12),
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(50),
					},
					Data: paddedUint64(9),
				},
			},
		},
		{
			name: "find all EventTwoIndexedWithData events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventTwoIndexedWithData"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(44),
						paddedUint64(27),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(46),
						paddedUint64(27),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(46),
						paddedUint64(14),
					},
					Data: paddedUint64(19),
				},
			},
		},
		{
			name: "find all EventThreeIndexedWithData events",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventThreeIndexedWithData"])).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventThreeIndexedWithData"],
						paddedUint64(44),
						paddedUint64(27),
						paddedUint64(19),
					},
					Data: paddedUint64(12),
				},
			},
		},

		{
			name: "find all events from contract2",
			spec: newEthFilterBuilder().FromBlockEpoch(0).AddressOneOf(contract2).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventZeroData"],
					},
					Data: nil,
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventThreeIndexed"],
						paddedUint64(44),
						paddedUint64(27),
						paddedUint64(19),
					},
					Data: nil,
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexed"],
						paddedUint64(44),
						paddedUint64(19),
					},
					Data: nil,
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(50),
					},
					Data: paddedUint64(9),
				},
			},
		},

		{
			name: "find all events with topic2 of 44",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic2OneOf(paddedEthHash(paddedUint64(44))).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexed"],
						paddedUint64(44),
					},
					Data: nil,
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexed"],
						paddedUint64(44),
						paddedUint64(19),
					},
					Data: nil,
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventThreeIndexed"],
						paddedUint64(44),
						paddedUint64(27),
						paddedUint64(19),
					},
					Data: nil,
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(44),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(44),
						paddedUint64(27),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventThreeIndexedWithData"],
						paddedUint64(44),
						paddedUint64(27),
						paddedUint64(19),
					},
					Data: paddedUint64(12),
				},
			},
		},

		{
			name: "find all events with topic2 of 44 from contract2",
			spec: newEthFilterBuilder().FromBlockEpoch(0).AddressOneOf(contract2).Topic2OneOf(paddedEthHash(paddedUint64(44))).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventThreeIndexed"],
						paddedUint64(44),
						paddedUint64(27),
						paddedUint64(19),
					},
					Data: nil,
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexed"],
						paddedUint64(44),
						paddedUint64(19),
					},
					Data: nil,
				},
			},
		},

		{
			name: "find all EventOneIndexedWithData events from contract1 or contract2",
			spec: newEthFilterBuilder().
				FromBlockEpoch(0).
				AddressOneOf(contract1, contract2).
				Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventOneIndexedWithData"])).
				Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(44),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(46),
					},
					Data: paddedUint64(12),
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(50),
					},
					Data: paddedUint64(9),
				},
			},
		},

		{
			name: "find all events with topic2 of 46",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic2OneOf(paddedEthHash(paddedUint64(46))).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(46),
					},
					Data: paddedUint64(12),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(46),
						paddedUint64(27),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(46),
						paddedUint64(14),
					},
					Data: paddedUint64(19),
				},
			},
		},
		{
			name: "find all events with topic2 of 50",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic2OneOf(paddedEthHash(paddedUint64(50))).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(50),
					},
					Data: paddedUint64(9),
				},
			},
		},
		{
			name: "find all events with topic2 of 46 or 50",
			spec: newEthFilterBuilder().FromBlockEpoch(0).Topic2OneOf(paddedEthHash(paddedUint64(46)), paddedEthHash(paddedUint64(50))).Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(46),
					},
					Data: paddedUint64(12),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(46),
						paddedUint64(27),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(46),
						paddedUint64(14),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract2,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexedWithData"],
						paddedUint64(50),
					},
					Data: paddedUint64(9),
				},
			},
		},

		{
			name: "find all events with topic1 of EventTwoIndexedWithData and topic3 of 27",
			spec: newEthFilterBuilder().
				FromBlockEpoch(0).
				Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventTwoIndexedWithData"])).
				Topic3OneOf(paddedEthHash(paddedUint64(27))).
				Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(44),
						paddedUint64(27),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(46),
						paddedUint64(27),
					},
					Data: paddedUint64(19),
				},
			},
		},

		{
			name: "find all events with topic1 of EventTwoIndexedWithData or EventOneIndexed and topic2 of 44",
			spec: newEthFilterBuilder().
				FromBlockEpoch(0).
				Topic1OneOf(paddedEthHash(EventMatrixContract.Ev["EventTwoIndexedWithData"]), paddedEthHash(EventMatrixContract.Ev["EventOneIndexed"])).
				Topic2OneOf(paddedEthHash(paddedUint64(44))).
				Filter(),

			expected: []ExpectedEthLog{
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventTwoIndexedWithData"],
						paddedUint64(44),
						paddedUint64(27),
					},
					Data: paddedUint64(19),
				},
				{
					Address: contract1,
					Topics: []ethtypes.EthBytes{
						EventMatrixContract.Ev["EventOneIndexed"],
						paddedUint64(44),
					},
					Data: nil,
				},
			},
		},
	}

	for _, tc := range testCases {
		tc := tc // appease the lint despot
		t.Run(tc.name, func(t *testing.T) {
			res, err := client.EthGetLogs(ctx, tc.spec)
			require.NoError(err)

			elogs, err := parseEthLogsFromFilterResult(res)
			require.NoError(err)
			AssertEthLogs(t, elogs, tc.expected, messages)
		})
	}
}

func TestEthGetLogsWithBlockRanges(t *testing.T) {
	require := require.New(t)
	kit.QuietAllLogsExcept("events", "messagepool")

	blockTime := 100 * time.Millisecond
	dbpath := filepath.Join(t.TempDir(), "actorevents.db")

	client, _, ens := kit.EnsembleMinimal(t, kit.MockProofs(), kit.ThroughRPC(), kit.HistoricFilterAPI(dbpath))
	ens.InterconnectAll().BeginMining(blockTime)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Set up the test fixture with a standard list of invocations
	_, _, messages := invokeEventMatrix(ctx, t, client)

	// Organize expected logs into three partitions for range testing
	expectedByHeight := map[abi.ChainEpoch][]ExpectedEthLog{}
	distinctHeights := map[abi.ChainEpoch]bool{}

	// Select events for partitioning
	for _, m := range messages {
		if bytes.Equal(m.invocation.Selector, EventMatrixContract.Fn["logEventTwoIndexedWithData"]) {
			addr := getContractEthAddress(ctx, t, client, m.invocation.Target)
			args := unpackUint64Values(m.invocation.Data)
			require.Equal(3, len(args), "logEventTwoIndexedWithData should have 3 arguments")

			distinctHeights[m.ts.Height()] = true
			expectedByHeight[m.ts.Height()] = append(expectedByHeight[m.ts.Height()], ExpectedEthLog{
				Address: addr,
				Topics: []ethtypes.EthBytes{
					EventMatrixContract.Ev["EventTwoIndexedWithData"],
					paddedUint64(args[0]),
					paddedUint64(args[1]),
				},
				Data: paddedUint64(args[2]),
			})
		}
	}

	// Divide heights into 3 partitions, they don't have to be equal
	require.True(len(distinctHeights) >= 3, "expected slice should divisible into three partitions")
	heights := make([]abi.ChainEpoch, 0, len(distinctHeights))
	for h := range distinctHeights {
		heights = append(heights, h)
	}
	sort.Slice(heights, func(i, j int) bool {
		return heights[i] < heights[j]
	})
	heightsPerPartition := len(heights) / 3

	type partition struct {
		start    abi.ChainEpoch
		end      abi.ChainEpoch
		expected []ExpectedEthLog
	}

	var partition1, partition2, partition3 partition

	partition1.start = heights[0]
	partition1.end = heights[heightsPerPartition-1]
	for e := partition1.start; e <= partition1.end; e++ {
		exp, ok := expectedByHeight[e]
		if !ok {
			continue
		}
		partition1.expected = append(partition1.expected, exp...)
	}
	t.Logf("partition1 from %d to %d with %d expected", partition1.start, partition1.end, len(partition1.expected))
	require.True(len(partition1.expected) > 0, "partition should have events")

	partition2.start = heights[heightsPerPartition]
	partition2.end = heights[heightsPerPartition*2-1]
	for e := partition2.start; e <= partition2.end; e++ {
		exp, ok := expectedByHeight[e]
		if !ok {
			continue
		}
		partition2.expected = append(partition2.expected, exp...)
	}
	t.Logf("partition2 from %d to %d with %d expected", partition2.start, partition2.end, len(partition2.expected))
	require.True(len(partition2.expected) > 0, "partition should have events")

	partition3.start = heights[heightsPerPartition*2]
	partition3.end = heights[len(heights)-1]
	for e := partition3.start; e <= partition3.end; e++ {
		exp, ok := expectedByHeight[e]
		if !ok {
			continue
		}
		partition3.expected = append(partition3.expected, exp...)
	}
	t.Logf("partition3 from %d to %d with %d expected", partition3.start, partition3.end, len(partition3.expected))
	require.True(len(partition3.expected) > 0, "partition should have events")

	// these are the topics we selected for partitioning earlier
	topics := []ethtypes.EthHash{paddedEthHash(EventMatrixContract.Ev["EventTwoIndexedWithData"])}

	union := func(lists ...[]ExpectedEthLog) []ExpectedEthLog {
		ret := []ExpectedEthLog{}
		for _, list := range lists {
			ret = append(ret, list...)
		}
		return ret
	}

	testCases := []struct {
		name     string
		spec     *ethtypes.EthFilterSpec
		expected []ExpectedEthLog
	}{
		{
			name:     "find all events from genesis",
			spec:     newEthFilterBuilder().FromBlockEpoch(0).Topic1OneOf(topics...).Filter(),
			expected: union(partition1.expected, partition2.expected, partition3.expected),
		},

		{
			name:     "find all from start of partition1",
			spec:     newEthFilterBuilder().FromBlockEpoch(partition1.start).Topic1OneOf(topics...).Filter(),
			expected: union(partition1.expected, partition2.expected, partition3.expected),
		},

		{
			name:     "find all from start of partition2",
			spec:     newEthFilterBuilder().FromBlockEpoch(partition2.start).Topic1OneOf(topics...).Filter(),
			expected: union(partition2.expected, partition3.expected),
		},

		{
			name:     "find all from start of partition3",
			spec:     newEthFilterBuilder().FromBlockEpoch(partition3.start).Topic1OneOf(topics...).Filter(),
			expected: union(partition3.expected),
		},

		{
			name:     "find none after end of partition3",
			spec:     newEthFilterBuilder().FromBlockEpoch(partition3.end + 1).Topic1OneOf(topics...).Filter(),
			expected: nil,
		},

		{
			name:     "find all events from genesis to end of partition1",
			spec:     newEthFilterBuilder().FromBlockEpoch(0).ToBlockEpoch(partition1.end).Topic1OneOf(topics...).Filter(),
			expected: union(partition1.expected),
		},

		{
			name:     "find all events from genesis to end of partition2",
			spec:     newEthFilterBuilder().FromBlockEpoch(0).ToBlockEpoch(partition2.end).Topic1OneOf(topics...).Filter(),
			expected: union(partition1.expected, partition2.expected),
		},

		{
			name:     "find all events from genesis to end of partition3",
			spec:     newEthFilterBuilder().FromBlockEpoch(0).ToBlockEpoch(partition3.end).Topic1OneOf(topics...).Filter(),
			expected: union(partition1.expected, partition2.expected, partition3.expected),
		},

		{
			name:     "find none from genesis to start of partition1",
			spec:     newEthFilterBuilder().FromBlockEpoch(0).ToBlockEpoch(partition1.start - 1).Topic1OneOf(topics...).Filter(),
			expected: nil,
		},

		{
			name:     "find all events in partition1",
			spec:     newEthFilterBuilder().FromBlockEpoch(partition1.start).ToBlockEpoch(partition1.end).Topic1OneOf(topics...).Filter(),
			expected: union(partition1.expected),
		},

		{
			name:     "find all events in partition2",
			spec:     newEthFilterBuilder().FromBlockEpoch(partition2.start).ToBlockEpoch(partition2.end).Topic1OneOf(topics...).Filter(),
			expected: union(partition2.expected),
		},

		{
			name:     "find all events in partition3",
			spec:     newEthFilterBuilder().FromBlockEpoch(partition3.start).ToBlockEpoch(partition3.end).Topic1OneOf(topics...).Filter(),
			expected: union(partition3.expected),
		},

		{
			name:     "find all events from earliest to end of partition1",
			spec:     newEthFilterBuilder().FromBlock("earliest").ToBlockEpoch(partition1.end).Topic1OneOf(topics...).Filter(),
			expected: union(partition1.expected),
		},

		{
			name:     "find all events from start of partition3 to latest",
			spec:     newEthFilterBuilder().FromBlockEpoch(partition3.start).ToBlock("latest").Topic1OneOf(topics...).Filter(),
			expected: union(partition3.expected),
		},

		{
			name:     "find all events from earliest to latest",
			spec:     newEthFilterBuilder().FromBlock("earliest").ToBlock("latest").Topic1OneOf(topics...).Filter(),
			expected: union(partition1.expected, partition2.expected, partition3.expected),
		},
	}

	for _, tc := range testCases {
		tc := tc // appease the lint despot
		t.Run(tc.name, func(t *testing.T) {
			res, err := client.EthGetLogs(ctx, tc.spec)
			require.NoError(err)

			elogs, err := parseEthLogsFromFilterResult(res)
			require.NoError(err)
			AssertEthLogs(t, elogs, tc.expected, messages)
		})
	}
}

// -------------------------------------------------------------------------------
// end of tests
// -------------------------------------------------------------------------------

type msgInTipset struct {
	invocation Invocation // the solidity invocation that generated this message
	msg        api.Message
	events     []types.Event // events extracted from receipt
	ts         *types.TipSet
	reverted   bool
}

func getContractEthAddress(ctx context.Context, t *testing.T, client *kit.TestFullNode, addr address.Address) ethtypes.EthAddress {
	head, err := client.ChainHead(ctx)
	require.NoError(t, err)

	actor, err := client.StateGetActor(ctx, addr, head.Key())
	require.NoError(t, err)
	require.NotNil(t, actor.Address)
	ethContractAddr, err := ethtypes.EthAddressFromFilecoinAddress(*actor.Address)
	require.NoError(t, err)
	return ethContractAddr
}

type Invocation struct {
	Sender    address.Address
	Target    address.Address
	Selector  []byte // function selector
	Data      []byte
	MinHeight abi.ChainEpoch // minimum chain height that must be reached before invoking
}

func invokeAndWaitUntilAllOnChain(t *testing.T, client *kit.TestFullNode, invocations []Invocation) map[ethtypes.EthHash]msgInTipset {
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	msgChan := make(chan msgInTipset, len(invocations))

	waitAllCh := make(chan struct{})
	waitForFirstHeadChange := make(chan struct{})
	go func() {
		headChangeCh, err := client.ChainNotify(ctx)
		require.NoError(err)
		select {
		case <-ctx.Done():
			return
		case <-headChangeCh: // skip hccurrent
		}

		close(waitForFirstHeadChange)

		defer func() {
			close(msgChan)
			close(waitAllCh)
		}()

		count := 0
		for {
			select {
			case <-ctx.Done():
				return
			case headChanges := <-headChangeCh:
				for _, change := range headChanges {
					if change.Type == store.HCApply || change.Type == store.HCRevert {
						msgs, err := client.ChainGetMessagesInTipset(ctx, change.Val.Key())
						require.NoError(err)

						count += len(msgs)
						for _, m := range msgs {
							select {
							case msgChan <- msgInTipset{msg: m, ts: change.Val, reverted: change.Type == store.HCRevert}:
							default:
							}
						}

						if count == len(invocations) {
							return
						}
					}
				}
			}
		}
	}()

	select {
	case <-waitForFirstHeadChange:
	case <-ctx.Done():
		t.Fatalf("timeout waiting for first head change")
	}

	eventMap := map[cid.Cid][]types.Event{}
	invocationMap := map[cid.Cid]Invocation{}
	for _, inv := range invocations {
		if inv.MinHeight > 0 {
			for {
				ts, err := client.ChainHead(ctx)
				require.NoError(err)
				if ts.Height() >= inv.MinHeight {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("context cancelled")
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		ret := client.EVM().InvokeSolidity(ctx, inv.Sender, inv.Target, inv.Selector, inv.Data)
		require.True(ret.Receipt.ExitCode.IsSuccess(), "contract execution failed")

		invocationMap[ret.Message] = inv

		require.NotNil(t, ret.Receipt.EventsRoot, "no event root on receipt")

		evs := client.EVM().LoadEvents(ctx, *ret.Receipt.EventsRoot)
		eventMap[ret.Message] = evs
	}

	select {
	case <-waitAllCh:
	case <-ctx.Done():
		t.Fatalf("timeout waiting to pack messages")
	}

	received := make(map[ethtypes.EthHash]msgInTipset)
	for m := range msgChan {
		inv, ok := invocationMap[m.msg.Cid]
		require.True(ok)
		m.invocation = inv

		evs, ok := eventMap[m.msg.Cid]
		require.True(ok)
		m.events = evs

		eh, err := client.EthGetTransactionHashByCid(ctx, m.msg.Cid)
		require.NoError(err)
		received[*eh] = m
	}
	require.Equal(len(invocations), len(received), "all messages on chain")

	return received
}

func invokeLogFourData(t *testing.T, client *kit.TestFullNode, iterations int) (ethtypes.EthAddress, map[ethtypes.EthHash]msgInTipset) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	fromAddr, idAddr := client.EVM().DeployContractFromFilename(ctx, EventsContract.Filename)

	invocations := make([]Invocation, iterations)
	for i := range invocations {
		invocations[i] = Invocation{
			Sender:   fromAddr,
			Target:   idAddr,
			Selector: EventsContract.Fn["log_four_data"],
			Data:     nil,
		}
	}

	messages := invokeAndWaitUntilAllOnChain(t, client, invocations)

	ethAddr := getContractEthAddress(ctx, t, client, idAddr)

	return ethAddr, messages
}

func invokeEventMatrix(ctx context.Context, t *testing.T, client *kit.TestFullNode) (ethtypes.EthAddress, ethtypes.EthAddress, map[ethtypes.EthHash]msgInTipset) {
	sender1, contract1 := client.EVM().DeployContractFromFilename(ctx, EventMatrixContract.Filename)
	sender2, contract2 := client.EVM().DeployContractFromFilename(ctx, EventMatrixContract.Filename)

	invocations := []Invocation{
		// log EventZeroData()
		// topic1: hash(EventZeroData)
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventZeroData"],
			Data:     nil,
		},

		// log EventOneData(23)
		// topic1: hash(EventOneData)
		// data: 23
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventOneData"],
			Data:     packUint64Values(23),
		},

		// log EventOneIndexed(44)
		// topic1: hash(EventOneIndexed)
		// topic2: 44
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventOneIndexed"],
			Data:     packUint64Values(44),
		},

		// log EventTwoIndexed(44,19) from contract2
		// topic1: hash(EventTwoIndexed)
		// topic2: 44
		// topic3: 19
		{
			Sender:   sender2,
			Target:   contract2,
			Selector: EventMatrixContract.Fn["logEventTwoIndexed"],
			Data:     packUint64Values(44, 19),
		},

		// log EventOneData(44)
		// topic1: hash(EventOneData)
		// data: 44
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventOneData"],
			Data:     packUint64Values(44),
		},

		// log EventTwoData(555,666)
		// topic1: hash(EventTwoData)
		// data: 555,666
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventTwoData"],
			Data:     packUint64Values(555, 666),
		},

		// log EventZeroData() from contract2
		// topic1: hash(EventZeroData)
		{
			Sender:   sender2,
			Target:   contract2,
			Selector: EventMatrixContract.Fn["logEventZeroData"],
			Data:     nil,
		},

		// log EventThreeData(1,2,3)
		// topic1: hash(EventTwoData)
		// data: 1,2,3
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventThreeData"],
			Data:     packUint64Values(1, 2, 3),
		},

		// log EventThreeIndexed(44,27,19) from contract2
		// topic1: hash(EventThreeIndexed)
		// topic2: 44
		// topic3: 27
		// topic4: 19
		{
			Sender:   sender1,
			Target:   contract2,
			Selector: EventMatrixContract.Fn["logEventThreeIndexed"],
			Data:     packUint64Values(44, 27, 19),
		},

		// log EventOneIndexedWithData(44,19)
		// topic1: hash(EventOneIndexedWithData)
		// topic2: 44
		// data: 19
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventOneIndexedWithData"],
			Data:     packUint64Values(44, 19),
		},

		// log EventOneIndexedWithData(46,12)
		// topic1: hash(EventOneIndexedWithData)
		// topic2: 46
		// data: 12
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventOneIndexedWithData"],
			Data:     packUint64Values(46, 12),
		},

		// log EventTwoIndexedWithData(44,27,19)
		// topic1: hash(EventTwoIndexedWithData)
		// topic2: 44
		// topic3: 27
		// data: 19
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventTwoIndexedWithData"],
			Data:     packUint64Values(44, 27, 19),
		},

		// log EventThreeIndexedWithData(44,27,19,12)
		// topic1: hash(EventThreeIndexedWithData)
		// topic2: 44
		// topic3: 27
		// topic4: 19
		// data: 12
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventThreeIndexedWithData"],
			Data:     packUint64Values(44, 27, 19, 12),
		},

		// log EventOneIndexedWithData(50,9)
		// topic1: hash(EventOneIndexedWithData)
		// topic2: 50
		// data: 9
		{
			Sender:   sender2,
			Target:   contract2,
			Selector: EventMatrixContract.Fn["logEventOneIndexedWithData"],
			Data:     packUint64Values(50, 9),
		},

		// log EventTwoIndexedWithData(46,27,19)
		// topic1: hash(EventTwoIndexedWithData)
		// topic2: 46
		// topic3: 27
		// data: 19
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventTwoIndexedWithData"],
			Data:     packUint64Values(46, 27, 19),
		},

		// log EventTwoIndexedWithData(46,14,19)
		// topic1: hash(EventTwoIndexedWithData)
		// topic2: 46
		// topic3: 14
		// data: 19
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventTwoIndexedWithData"],
			Data:     packUint64Values(46, 14, 19),
		},
		// log EventTwoIndexed(44,19) from contract1
		// topic1: hash(EventTwoIndexed)
		// topic2: 44
		// topic3: 19
		{
			Sender:   sender1,
			Target:   contract1,
			Selector: EventMatrixContract.Fn["logEventTwoIndexed"],
			Data:     packUint64Values(40, 20),
		},
	}

	messages := invokeAndWaitUntilAllOnChain(t, client, invocations)
	ethAddr1 := getContractEthAddress(ctx, t, client, contract1)
	ethAddr2 := getContractEthAddress(ctx, t, client, contract2)
	return ethAddr1, ethAddr2, messages
}

type ExpectedEthLog struct {
	// Address is the address of the actor that produced the event log.
	Address ethtypes.EthAddress `json:"address"`

	// List of topics associated with the event log.
	Topics []ethtypes.EthBytes `json:"topics"`

	// Data is the value of the event log, excluding topics
	Data ethtypes.EthBytes `json:"data"`
}

func AssertEthLogs(t *testing.T, actual []*ethtypes.EthLog, expected []ExpectedEthLog, messages map[ethtypes.EthHash]msgInTipset) {
	require := require.New(t)
	// require.Equal(len(expected), len(actual), "number of results equal to expected")

	formatTopics := func(topics []ethtypes.EthBytes) string {
		ss := make([]string, len(topics))
		for i := range topics {
			ss[i] = fmt.Sprintf("%d:%x", i, topics[i])
		}
		return strings.Join(ss, ",")
	}

	expectedMatched := map[int]bool{}

	for _, elog := range actual {
		msg, exists := messages[elog.TransactionHash]
		require.True(exists, "message seen on chain")

		tsCid, err := msg.ts.Key().Cid()
		require.NoError(err)

		tsCidHash, err := ethtypes.EthHashFromCid(tsCid)
		require.NoError(err)

		require.Equal(tsCidHash, elog.BlockHash, "block hash matches tipset key")

		// Try and match the received log against an expected log
		matched := false
	LoopExpected:
		for i, want := range expected {
			// each expected log must match only once
			if expectedMatched[i] {
				continue
			}

			if elog.Address != want.Address {
				continue
			}

			if len(elog.Topics) != len(want.Topics) {
				continue
			}

			for j := range elog.Topics {
				if !bytes.Equal(elog.Topics[j], want.Topics[j]) {
					continue LoopExpected
				}
			}

			if !bytes.Equal(elog.Data, want.Data) {
				continue
			}

			expectedMatched[i] = true
			matched = true
			break
		}

		if !matched {
			var buf strings.Builder
			buf.WriteString(fmt.Sprintf("found unexpected log at height %d:\n", msg.ts.Height()))
			buf.WriteString(fmt.Sprintf("  address: %s\n", elog.Address))
			buf.WriteString(fmt.Sprintf("  topics: %s\n", formatTopics(elog.Topics)))
			buf.WriteString(fmt.Sprintf("  data: %x\n", elog.Data))
			buf.WriteString("original events from receipt were:\n")
			for i, ev := range msg.events {
				buf.WriteString(fmt.Sprintf("event %d\n", i))
				buf.WriteString(fmt.Sprintf("  emitter: %v\n", ev.Emitter))
				for _, en := range ev.Entries {
					buf.WriteString(fmt.Sprintf("  %s=%x\n", en.Key, decodeLogBytes(en.Value)))
				}
			}

			t.Errorf(buf.String())
		}
	}

	for i := range expected {
		if _, ok := expectedMatched[i]; !ok {
			var buf strings.Builder
			buf.WriteString(fmt.Sprintf("did not find expected log with index %d:\n", i))
			buf.WriteString(fmt.Sprintf("  address: %s\n", expected[i].Address))
			buf.WriteString(fmt.Sprintf("  topics: %s\n", formatTopics(expected[i].Topics)))
			buf.WriteString(fmt.Sprintf("  data: %x\n", expected[i].Data))
			t.Errorf(buf.String())
		}
	}
}

func parseEthLogsFromSubscriptionResponses(subResponses []ethtypes.EthSubscriptionResponse) ([]*ethtypes.EthLog, error) {
	elogs := make([]*ethtypes.EthLog, 0, len(subResponses))
	for i := range subResponses {
		rlist, ok := subResponses[i].Result.([]interface{})
		if !ok {
			return nil, xerrors.Errorf("expected subscription result to be []interface{}, but was %T", subResponses[i].Result)
		}

		for _, r := range rlist {
			rmap, ok := r.(map[string]interface{})
			if !ok {
				return nil, xerrors.Errorf("expected subscription result entry to be map[string]interface{}, but was %T", r)
			}

			elog, err := ParseEthLog(rmap)
			if err != nil {
				return nil, err
			}
			elogs = append(elogs, elog)
		}
	}

	return elogs, nil
}

func parseEthLogsFromFilterResult(res *ethtypes.EthFilterResult) ([]*ethtypes.EthLog, error) {
	elogs := make([]*ethtypes.EthLog, 0, len(res.Results))

	for _, r := range res.Results {
		rmap, ok := r.(map[string]interface{})
		if !ok {
			return nil, xerrors.Errorf("expected filter result entry to be map[string]interface{}, but was %T", r)
		}

		elog, err := ParseEthLog(rmap)
		if err != nil {
			return nil, err
		}
		elogs = append(elogs, elog)
	}

	return elogs, nil
}

func ParseEthLog(in map[string]interface{}) (*ethtypes.EthLog, error) {
	el := &ethtypes.EthLog{}

	ethHash := func(k string, v interface{}) (ethtypes.EthHash, error) {
		s, ok := v.(string)
		if !ok {
			return ethtypes.EthHash{}, xerrors.Errorf(k + " not a string")
		}
		return ethtypes.ParseEthHash(s)
	}

	ethUint64 := func(k string, v interface{}) (ethtypes.EthUint64, error) {
		s, ok := v.(string)
		if !ok {
			return 0, xerrors.Errorf(k + " not a string")
		}
		parsedInt, err := strconv.ParseUint(strings.Replace(s, "0x", "", -1), 16, 64)
		if err != nil {
			return 0, err
		}
		return ethtypes.EthUint64(parsedInt), nil
	}

	var err error
	for k, v := range in {
		switch k {
		case "removed":
			b, ok := v.(bool)
			if ok {
				el.Removed = b
				continue
			}
			s, ok := v.(string)
			if !ok {
				return nil, xerrors.Errorf(k + ": not a string")
			}
			el.Removed, err = strconv.ParseBool(s)
			if err != nil {
				return nil, xerrors.Errorf("%s: %w", k, err)
			}
		case "address":
			s, ok := v.(string)
			if !ok {
				return nil, xerrors.Errorf(k + ": not a string")
			}
			el.Address, err = ethtypes.ParseEthAddress(s)
			if err != nil {
				return nil, xerrors.Errorf("%s: %w", k, err)
			}
		case "logIndex":
			el.LogIndex, err = ethUint64(k, v)
			if err != nil {
				return nil, xerrors.Errorf("%s: %w", k, err)
			}
		case "transactionIndex":
			el.TransactionIndex, err = ethUint64(k, v)
			if err != nil {
				return nil, xerrors.Errorf("%s: %w", k, err)
			}
		case "blockNumber":
			el.BlockNumber, err = ethUint64(k, v)
			if err != nil {
				return nil, xerrors.Errorf("%s: %w", k, err)
			}
		case "transactionHash":
			el.TransactionHash, err = ethHash(k, v)
			if err != nil {
				return nil, xerrors.Errorf("%s: %w", k, err)
			}
		case "blockHash":
			el.BlockHash, err = ethHash(k, v)
			if err != nil {
				return nil, xerrors.Errorf("%s: %w", k, err)
			}
		case "data":
			s, ok := v.(string)
			if !ok {
				return nil, xerrors.Errorf(k + ": not a string")
			}
			data, err := hex.DecodeString(s[2:])
			if err != nil {
				return nil, xerrors.Errorf("%s: %w", k, err)
			}
			el.Data = data

		case "topics":
			s, ok := v.(string)
			if ok {
				topic, err := hex.DecodeString(s[2:])
				if err != nil {
					return nil, xerrors.Errorf("%s: %w", k, err)
				}
				el.Topics = append(el.Topics, topic)
				continue
			}

			sl, ok := v.([]interface{})
			if !ok {
				return nil, xerrors.Errorf(k + ": not a slice")
			}
			for _, s := range sl {
				topic, err := hex.DecodeString(s.(string)[2:])
				if err != nil {
					return nil, xerrors.Errorf("%s: %w", k, err)
				}
				el.Topics = append(el.Topics, topic)
			}
		}
	}

	return el, err
}

func decodeLogBytes(orig []byte) []byte {
	if orig == nil {
		return orig
	}
	decoded, err := cbg.ReadByteArray(bytes.NewReader(orig), uint64(len(orig)))
	if err != nil {
		return orig
	}
	return decoded
}

func paddedEthBytes(orig []byte) ethtypes.EthBytes {
	needed := 32 - len(orig)
	if needed <= 0 {
		return orig
	}
	ret := make([]byte, 32)
	copy(ret[needed:], orig)
	return ret
}

func paddedUint64(v uint64) ethtypes.EthBytes {
	buf := make([]byte, 32)
	binary.BigEndian.PutUint64(buf[24:], v)
	return buf
}

func paddedEthHash(orig []byte) ethtypes.EthHash {
	if len(orig) > 32 {
		panic("exceeds EthHash length")
	}
	var ret ethtypes.EthHash
	needed := 32 - len(orig)
	copy(ret[needed:], orig)
	return ret
}

func ethTopicHash(sig string) []byte {
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write([]byte(sig))
	return hasher.Sum(nil)
}

func ethFunctionHash(sig string) []byte {
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write([]byte(sig))
	return hasher.Sum(nil)[:4]
}

func packUint64Values(vals ...uint64) []byte {
	ret := []byte{}
	for _, v := range vals {
		buf := paddedUint64(v)
		ret = append(ret, buf...)
	}
	return ret
}

func unpackUint64Values(data []byte) []uint64 {
	if len(data)%32 != 0 {
		panic("data length not a multiple of 32")
	}

	var vals []uint64
	for i := 0; i < len(data); i += 32 {
		v := binary.BigEndian.Uint64(data[i+24 : i+32])
		vals = append(vals, v)
	}
	return vals
}

func newEthFilterBuilder() *ethFilterBuilder { return &ethFilterBuilder{} }

type ethFilterBuilder struct {
	filter ethtypes.EthFilterSpec
}

func (e *ethFilterBuilder) Filter() *ethtypes.EthFilterSpec { return &e.filter }

func (e *ethFilterBuilder) FromBlock(v string) *ethFilterBuilder {
	e.filter.FromBlock = &v
	return e
}

func (e *ethFilterBuilder) FromBlockEpoch(v abi.ChainEpoch) *ethFilterBuilder {
	s := ethtypes.EthUint64(v).Hex()
	e.filter.FromBlock = &s
	return e
}

func (e *ethFilterBuilder) ToBlock(v string) *ethFilterBuilder {
	e.filter.ToBlock = &v
	return e
}

func (e *ethFilterBuilder) ToBlockEpoch(v abi.ChainEpoch) *ethFilterBuilder {
	s := ethtypes.EthUint64(v).Hex()
	e.filter.ToBlock = &s
	return e
}

func (e *ethFilterBuilder) BlockHash(h ethtypes.EthHash) *ethFilterBuilder {
	e.filter.BlockHash = &h
	return e
}

func (e *ethFilterBuilder) AddressOneOf(as ...ethtypes.EthAddress) *ethFilterBuilder {
	e.filter.Address = as
	return e
}

func (e *ethFilterBuilder) Topic1OneOf(hs ...ethtypes.EthHash) *ethFilterBuilder {
	if len(e.filter.Topics) == 0 {
		e.filter.Topics = make(ethtypes.EthTopicSpec, 1)
	}
	e.filter.Topics[0] = hs
	return e
}

func (e *ethFilterBuilder) Topic2OneOf(hs ...ethtypes.EthHash) *ethFilterBuilder {
	for len(e.filter.Topics) < 2 {
		e.filter.Topics = append(e.filter.Topics, nil)
	}
	e.filter.Topics[1] = hs
	return e
}

func (e *ethFilterBuilder) Topic3OneOf(hs ...ethtypes.EthHash) *ethFilterBuilder {
	for len(e.filter.Topics) < 3 {
		e.filter.Topics = append(e.filter.Topics, nil)
	}
	e.filter.Topics[2] = hs
	return e
}

func (e *ethFilterBuilder) Topic4OneOf(hs ...ethtypes.EthHash) *ethFilterBuilder {
	for len(e.filter.Topics) < 4 {
		e.filter.Topics = append(e.filter.Topics, nil)
	}
	e.filter.Topics[3] = hs
	return e
}
