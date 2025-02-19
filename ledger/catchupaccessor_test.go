// Copyright (C) 2019-2022 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package ledger

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/encoded"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/db"
	"github.com/algorand/msgp/msgp"
)

func createTestingEncodedChunks(accountsCount uint64) (encodedAccountChunks [][]byte, last64KIndex int) {
	// pre-create all encoded chunks.
	accounts := uint64(0)
	encodedAccountChunks = make([][]byte, 0, accountsCount/BalancesPerCatchpointFileChunk+1)
	last64KIndex = -1
	for accounts < accountsCount {
		// generate a chunk;
		chunkSize := accountsCount - accounts
		if chunkSize > BalancesPerCatchpointFileChunk {
			chunkSize = BalancesPerCatchpointFileChunk
		}
		if accounts >= accountsCount-64*1024 && last64KIndex == -1 {
			last64KIndex = len(encodedAccountChunks)
		}
		var chunk catchpointFileChunkV6
		chunk.Balances = make([]encoded.BalanceRecordV6, chunkSize)
		for i := uint64(0); i < chunkSize; i++ {
			var randomAccount encoded.BalanceRecordV6
			accountData := store.BaseAccountData{}
			accountData.MicroAlgos.Raw = crypto.RandUint63()
			randomAccount.AccountData = protocol.Encode(&accountData)
			// have the first account be the zero address
			if i > 0 {
				crypto.RandBytes(randomAccount.Address[:])
			}
			binary.LittleEndian.PutUint64(randomAccount.Address[:], accounts+i)
			chunk.Balances[i] = randomAccount
		}
		encodedAccountChunks = append(encodedAccountChunks, protocol.Encode(&chunk))
		accounts += chunkSize
	}
	return
}

func benchmarkRestoringFromCatchpointFileHelper(b *testing.B) {
	genesisInitState, _ := ledgertesting.GenerateInitState(b, protocol.ConsensusCurrentVersion, 100)
	const inMem = false
	log := logging.TestingLog(b)
	cfg := config.GetDefaultLocal()
	cfg.Archival = false
	log.SetLevel(logging.Warn)
	dbBaseFileName := strings.Replace(b.Name(), "/", "_", -1)
	// delete database files, in case they were left there by previous iterations of this test.
	os.Remove(dbBaseFileName + ".block.sqlite")
	os.Remove(dbBaseFileName + ".tracker.sqlite")
	l, err := OpenLedger(log, dbBaseFileName, inMem, genesisInitState, cfg)
	require.NoError(b, err, "could not open ledger")
	defer func() {
		l.Close()
		os.Remove(dbBaseFileName + ".block.sqlite")
		os.Remove(dbBaseFileName + ".tracker.sqlite")
	}()

	catchpointAccessor := MakeCatchpointCatchupAccessor(l, log)
	catchpointAccessor.ResetStagingBalances(context.Background(), true)

	accountsCount := uint64(b.N)
	fileHeader := CatchpointFileHeader{
		Version:           CatchpointFileVersionV6,
		BalancesRound:     basics.Round(0),
		BlocksRound:       basics.Round(0),
		Totals:            ledgercore.AccountTotals{},
		TotalAccounts:     accountsCount,
		TotalChunks:       (accountsCount + BalancesPerCatchpointFileChunk - 1) / BalancesPerCatchpointFileChunk,
		Catchpoint:        "",
		BlockHeaderDigest: crypto.Digest{},
	}
	encodedFileHeader := protocol.Encode(&fileHeader)
	var progress CatchpointCatchupAccessorProgress
	err = catchpointAccessor.ProcessStagingBalances(context.Background(), "content.msgpack", encodedFileHeader, &progress)
	require.NoError(b, err)

	// pre-create all encoded chunks.
	encodedAccountChunks, last64KIndex := createTestingEncodedChunks(accountsCount)

	b.ResetTimer()
	var last64KStart time.Time
	for len(encodedAccountChunks) > 0 {
		encodedAccounts := encodedAccountChunks[0]
		encodedAccountChunks = encodedAccountChunks[1:]

		if last64KIndex == 0 {
			last64KStart = time.Now()
		}

		err = catchpointAccessor.ProcessStagingBalances(context.Background(), "balances.XX.msgpack", encodedAccounts, &progress)
		require.NoError(b, err)
		last64KIndex--
	}
	if !last64KStart.IsZero() {
		last64KDuration := time.Since(last64KStart)
		b.ReportMetric(float64(last64KDuration.Nanoseconds())/float64(64*1024), "ns/last_64k_account")
	}
}

func BenchmarkRestoringFromCatchpointFile(b *testing.B) {
	benchSizes := []int{1024 * 100, 1024 * 200, 1024 * 400, 1024 * 800}
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("Restore-%d", size), func(b *testing.B) {
			b.N = size
			benchmarkRestoringFromCatchpointFileHelper(b)
		})
	}
}

func TestCatchupAccessorFoo(t *testing.T) {
	partitiontest.PartitionTest(t)

	log := logging.TestingLog(t)
	dbBaseFileName := t.Name()
	const inMem = true
	genesisInitState, _ /* initKeys */ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 100)
	cfg := config.GetDefaultLocal()
	l, err := OpenLedger(log, dbBaseFileName, inMem, genesisInitState, cfg)
	require.NoError(t, err, "could not open ledger")
	defer func() {
		l.Close()
	}()
	catchpointAccessor := MakeCatchpointCatchupAccessor(l, log)
	err = catchpointAccessor.ResetStagingBalances(context.Background(), true)
	require.NoError(t, err, "ResetStagingBalances")

	// TODO: GetState/SetState/GetLabel/SetLabel but setup for an error? (disconnected db?)

	err = catchpointAccessor.SetState(context.Background(), CatchpointCatchupStateInactive)
	require.NoError(t, err, "catchpointAccessor.SetState")
	err = catchpointAccessor.SetState(context.Background(), CatchpointCatchupStateLedgerDownload)
	require.NoError(t, err, "catchpointAccessor.SetState")
	err = catchpointAccessor.SetState(context.Background(), CatchpointCatchupStateLatestBlockDownload)
	require.NoError(t, err, "catchpointAccessor.SetState")
	err = catchpointAccessor.SetState(context.Background(), CatchpointCatchupStateBlocksDownload)
	require.NoError(t, err, "catchpointAccessor.SetState")
	err = catchpointAccessor.SetState(context.Background(), CatchpointCatchupStateSwitch)
	require.NoError(t, err, "catchpointAccessor.SetState")
	err = catchpointAccessor.SetState(context.Background(), catchpointCatchupStateLast+1)
	require.Error(t, err, "catchpointAccessor.SetState")

	state, err := catchpointAccessor.GetState(context.Background())
	require.NoError(t, err, "catchpointAccessor.GetState")
	require.Equal(t, CatchpointCatchupState(CatchpointCatchupStateSwitch), state)
	t.Logf("catchpoint state %#v", state)

	// invalid label
	err = catchpointAccessor.SetLabel(context.Background(), "wat")
	require.Error(t, err, "catchpointAccessor.SetLabel")

	// ok
	calabel := "98#QGMCMMUPV74AXXVKSNPRN73XMJG44ZJTZHU25HDG7JH5OHMM6N3Q"
	err = catchpointAccessor.SetLabel(context.Background(), calabel)
	require.NoError(t, err, "catchpointAccessor.SetLabel")

	label, err := catchpointAccessor.GetLabel(context.Background())
	require.NoError(t, err, "catchpointAccessor.GetLabel")
	require.Equal(t, calabel, label)
	t.Logf("catchpoint label %#v", label)

	err = catchpointAccessor.ResetStagingBalances(context.Background(), false)
	require.NoError(t, err, "ResetStagingBalances")
}

func TestBuildMerkleTrie(t *testing.T) {
	partitiontest.PartitionTest(t)

	// setup boilerplate
	log := logging.TestingLog(t)
	dbBaseFileName := t.Name()
	const inMem = true
	genesisInitState, initKeys := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 100)
	cfg := config.GetDefaultLocal()
	l, err := OpenLedger(log, dbBaseFileName, inMem, genesisInitState, cfg)
	require.NoError(t, err, "could not open ledger")
	defer func() {
		l.Close()
	}()
	catchpointAccessor := MakeCatchpointCatchupAccessor(l, log)

	progressCallCount := 0
	progressNop := func(uint64, uint64) {
		progressCallCount++
	}

	ctx := context.Background()

	// actual testing...
	// insufficient setup, it should fail:
	err = catchpointAccessor.BuildMerkleTrie(ctx, progressNop)
	require.Error(t, err)

	// from reset it's okay, but it doesn't do anything
	err = catchpointAccessor.ResetStagingBalances(ctx, true)
	require.NoError(t, err, "ResetStagingBalances")
	err = catchpointAccessor.BuildMerkleTrie(ctx, progressNop)
	require.NoError(t, err)
	require.False(t, progressCallCount > 0)

	// process some data...
	err = catchpointAccessor.ResetStagingBalances(ctx, true)
	require.NoError(t, err, "ResetStagingBalances")
	// TODO: catchpointAccessor.ProcessStagingBalances() like in ledgerFetcher.downloadLedger(cs.ctx, peer, round) like catchup/catchpointService.go which is the real usage of BuildMerkleTrie()
	var blob []byte = nil // TODO: content!
	var progress CatchpointCatchupAccessorProgress
	err = catchpointAccessor.ProcessStagingBalances(ctx, "ignoredContent", blob, &progress)
	require.NoError(t, err)
	// this shouldn't work yet
	err = catchpointAccessor.ProcessStagingBalances(ctx, "balances.FAKE.msgpack", blob, &progress)
	require.Error(t, err)
	// this needs content
	err = catchpointAccessor.ProcessStagingBalances(ctx, "content.msgpack", blob, &progress)
	require.Error(t, err)

	// content.msgpack from this:
	accountsCount := uint64(len(initKeys))
	fileHeader := CatchpointFileHeader{
		Version:           CatchpointFileVersionV6,
		BalancesRound:     basics.Round(0),
		BlocksRound:       basics.Round(0),
		Totals:            ledgercore.AccountTotals{},
		TotalAccounts:     accountsCount,
		TotalChunks:       (accountsCount + BalancesPerCatchpointFileChunk - 1) / BalancesPerCatchpointFileChunk,
		Catchpoint:        "",
		BlockHeaderDigest: crypto.Digest{},
	}
	encodedFileHeader := protocol.Encode(&fileHeader)
	err = catchpointAccessor.ProcessStagingBalances(ctx, "content.msgpack", encodedFileHeader, &progress)
	require.NoError(t, err)
	// shouldn't work a second time
	err = catchpointAccessor.ProcessStagingBalances(ctx, "content.msgpack", encodedFileHeader, &progress)
	require.Error(t, err)

	// This should still fail, but slightly different coverage path
	err = catchpointAccessor.ProcessStagingBalances(ctx, "balances.FAKE.msgpack", blob, &progress)
	require.Error(t, err)

	// create some catchpoint data
	encodedAccountChunks, _ := createTestingEncodedChunks(accountsCount)

	for _, encodedAccounts := range encodedAccountChunks {

		err = catchpointAccessor.ProcessStagingBalances(context.Background(), "balances.XX.msgpack", encodedAccounts, &progress)
		require.NoError(t, err)
	}

	err = catchpointAccessor.BuildMerkleTrie(ctx, progressNop)
	require.NoError(t, err)
	require.True(t, progressCallCount > 0)

	blockRound, err := catchpointAccessor.GetCatchupBlockRound(ctx)
	require.NoError(t, err)
	require.Equal(t, basics.Round(0), blockRound)
}

// blockdb.go code
// TODO: blockStartCatchupStaging called from StoreFirstBlock()
// TODO: blockCompleteCatchup called from FinishBlocks()
// TODO: blockAbortCatchup called from FinishBlocks()
// TODO: blockPutStaging called from StoreBlock()
// TODO: blockEnsureSingleBlock called from EnsureFirstBlock()

func TestCatchupAccessorBlockdb(t *testing.T) {
	partitiontest.PartitionTest(t)

	// setup boilerplate
	log := logging.TestingLog(t)
	dbBaseFileName := t.Name()
	const inMem = true
	genesisInitState, _ /*initKeys*/ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 100)
	cfg := config.GetDefaultLocal()
	l, err := OpenLedger(log, dbBaseFileName, inMem, genesisInitState, cfg)
	require.NoError(t, err, "could not open ledger")
	defer func() {
		l.Close()
	}()
	catchpointAccessor := MakeCatchpointCatchupAccessor(l, log)
	ctx := context.Background()

	// actual testing...
	err = catchpointAccessor.BuildMerkleTrie(ctx, func(uint64, uint64) {})
	require.Error(t, err)
}

func TestVerifyCatchpoint(t *testing.T) {
	partitiontest.PartitionTest(t)

	// setup boilerplate
	log := logging.TestingLog(t)
	dbBaseFileName := t.Name()
	const inMem = true
	genesisInitState, _ /*initKeys*/ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 100)
	cfg := config.GetDefaultLocal()
	l, err := OpenLedger(log, dbBaseFileName, inMem, genesisInitState, cfg)
	require.NoError(t, err, "could not open ledger")
	defer func() {
		l.Close()
	}()
	catchpointAccessor := MakeCatchpointCatchupAccessor(l, log)

	ctx := context.Background()

	// actual testing...
	var blk bookkeeping.Block
	err = catchpointAccessor.VerifyCatchpoint(ctx, &blk)
	require.Error(t, err)

	err = catchpointAccessor.ResetStagingBalances(ctx, true)
	require.NoError(t, err, "ResetStagingBalances")

	err = catchpointAccessor.VerifyCatchpoint(ctx, &blk)
	require.Error(t, err)
	// TODO: verify a catchpoint block that is valid

	// StoreBalancesRound assumes things are valid, so just does the db put
	err = catchpointAccessor.StoreBalancesRound(ctx, &blk)
	require.NoError(t, err)
	// StoreFirstBlock is a dumb wrapper on some db logic
	err = catchpointAccessor.StoreFirstBlock(ctx, &blk)
	require.NoError(t, err)

	_, err = catchpointAccessor.EnsureFirstBlock(ctx)
	require.NoError(t, err)

	blk.BlockHeader.Round++
	err = catchpointAccessor.StoreBlock(ctx, &blk)
	require.NoError(t, err)

	// TODO: write a case with working no-err
	err = catchpointAccessor.CompleteCatchup(ctx)
	require.Error(t, err)
	//require.NoError(t, err)
}

func TestCatchupAccessorResourceCountMismatch(t *testing.T) {
	partitiontest.PartitionTest(t)

	// setup boilerplate
	log := logging.TestingLog(t)
	dbBaseFileName := t.Name()
	const inMem = true
	genesisInitState, _ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 100)
	cfg := config.GetDefaultLocal()
	l, err := OpenLedger(log, dbBaseFileName, inMem, genesisInitState, cfg)
	require.NoError(t, err, "could not open ledger")
	defer func() {
		l.Close()
	}()
	catchpointAccessor := MakeCatchpointCatchupAccessor(l, log)
	var progress CatchpointCatchupAccessorProgress
	ctx := context.Background()

	// content.msgpack from this:
	fileHeader := CatchpointFileHeader{
		Version:           CatchpointFileVersionV6,
		BalancesRound:     basics.Round(0),
		BlocksRound:       basics.Round(0),
		Totals:            ledgercore.AccountTotals{},
		TotalAccounts:     1,
		TotalChunks:       1,
		Catchpoint:        "",
		BlockHeaderDigest: crypto.Digest{},
	}
	encodedFileHeader := protocol.Encode(&fileHeader)
	err = catchpointAccessor.ProcessStagingBalances(ctx, "content.msgpack", encodedFileHeader, &progress)
	require.NoError(t, err)

	var balances catchpointFileChunkV6
	balances.Balances = make([]encoded.BalanceRecordV6, 1)
	var randomAccount encoded.BalanceRecordV6
	accountData := store.BaseAccountData{}
	accountData.MicroAlgos.Raw = crypto.RandUint63()
	accountData.TotalAppParams = 1
	randomAccount.AccountData = protocol.Encode(&accountData)
	crypto.RandBytes(randomAccount.Address[:])
	binary.LittleEndian.PutUint64(randomAccount.Address[:], 0)
	balances.Balances[0] = randomAccount
	encodedAccounts := protocol.Encode(&balances)

	// expect error since there is a resource count mismatch
	err = catchpointAccessor.ProcessStagingBalances(ctx, "balances.XX.msgpack", encodedAccounts, &progress)
	require.Error(t, err)
}

type testStagingWriter struct {
	t      *testing.T
	hashes map[[4 + crypto.DigestSize]byte]int
}

func (w *testStagingWriter) writeBalances(ctx context.Context, balances []store.NormalizedAccountBalance) error {
	return nil
}

func (w *testStagingWriter) writeCreatables(ctx context.Context, balances []store.NormalizedAccountBalance) error {
	return nil
}

func (w *testStagingWriter) writeKVs(ctx context.Context, kvrs []encoded.KVRecordV6) error {
	return nil
}

func (w *testStagingWriter) writeHashes(ctx context.Context, balances []store.NormalizedAccountBalance) error {
	for _, bal := range balances {
		for _, hash := range bal.AccountHashes {
			var key [4 + crypto.DigestSize]byte
			require.Len(w.t, hash, 4+crypto.DigestSize)
			copy(key[:], hash)
			w.hashes[key] = w.hashes[key] + 1
		}
	}
	return nil
}

func (w *testStagingWriter) isShared() bool {
	return false
}

// makeTestCatchpointCatchupAccessor creates a CatchpointCatchupAccessor given a ledger
func makeTestCatchpointCatchupAccessor(ledger *Ledger, log logging.Logger, writer stagingWriter) *catchpointCatchupAccessorImpl {
	return &catchpointCatchupAccessorImpl{
		ledger:        ledger,
		stagingWriter: writer,
		log:           log,
	}
}

func TestCatchupAccessorProcessStagingBalances(t *testing.T) {
	partitiontest.PartitionTest(t)

	log := logging.TestingLog(t)
	writer := &testStagingWriter{t: t, hashes: make(map[[4 + crypto.DigestSize]byte]int)}
	l := Ledger{
		log:             log,
		genesisProto:    config.Consensus[protocol.ConsensusCurrentVersion],
		synchronousMode: db.SynchronousMode(100), // non-existing in order to skip the underlying db call in ledger.setSynchronousMode
	}
	catchpointAccessor := makeTestCatchpointCatchupAccessor(&l, log, writer)

	randomSimpleBaseAcct := func() store.BaseAccountData {
		accountData := store.BaseAccountData{
			RewardsBase: crypto.RandUint63(),
			MicroAlgos:  basics.MicroAlgos{Raw: crypto.RandUint63()},
			AuthAddr:    ledgertesting.RandomAddress(),
		}
		return accountData
	}

	encodedBalanceRecordFromBase := func(addr basics.Address, base store.BaseAccountData, resources map[uint64]msgp.Raw, more bool) encoded.BalanceRecordV6 {
		ebr := encoded.BalanceRecordV6{
			Address:              addr,
			AccountData:          protocol.Encode(&base),
			Resources:            resources,
			ExpectingMoreEntries: more,
		}
		return ebr
	}

	const numAccounts = 5
	const acctXNumRes = 13
	const expectHashes = numAccounts + acctXNumRes
	progress := CatchpointCatchupAccessorProgress{
		TotalAccounts: numAccounts,
		TotalChunks:   2,
		SeenHeader:    true,
		Version:       CatchpointFileVersionV6,
	}

	// create some walking gentlemen
	acctA := randomSimpleBaseAcct()
	acctB := randomSimpleBaseAcct()
	acctC := randomSimpleBaseAcct()
	acctD := randomSimpleBaseAcct()

	// prepare chunked account
	addrX := ledgertesting.RandomAddress()
	acctX := randomSimpleBaseAcct()
	acctX.TotalAssets = acctXNumRes
	acctXRes1 := make(map[uint64]msgp.Raw, acctXNumRes/2+1)
	acctXRes2 := make(map[uint64]msgp.Raw, acctXNumRes/2)
	emptyRes := store.ResourcesData{ResourceFlags: store.ResourceFlagsEmptyAsset}
	emptyResEnc := protocol.Encode(&emptyRes)
	for i := 0; i < acctXNumRes; i++ {
		if i <= acctXNumRes/2 {
			acctXRes1[rand.Uint64()] = emptyResEnc
		} else {
			acctXRes2[rand.Uint64()] = emptyResEnc
		}
	}

	// make chunks
	chunks := []catchpointFileChunkV6{
		{
			Balances: []encoded.BalanceRecordV6{
				encodedBalanceRecordFromBase(ledgertesting.RandomAddress(), acctA, nil, false),
				encodedBalanceRecordFromBase(ledgertesting.RandomAddress(), acctB, nil, false),
				encodedBalanceRecordFromBase(addrX, acctX, acctXRes1, true),
			},
		},
		{
			Balances: []encoded.BalanceRecordV6{
				encodedBalanceRecordFromBase(addrX, acctX, acctXRes2, false),
				encodedBalanceRecordFromBase(ledgertesting.RandomAddress(), acctC, nil, false),
				encodedBalanceRecordFromBase(ledgertesting.RandomAddress(), acctD, nil, false),
			},
		},
	}

	// process chunks
	ctx := context.Background()
	progress.SeenHeader = true
	for _, chunk := range chunks {
		blob := protocol.Encode(&chunk)
		err := catchpointAccessor.processStagingBalances(ctx, blob, &progress)
		require.NoError(t, err)
	}

	// compare account counts and hashes
	require.Equal(t, progress.TotalAccounts, progress.ProcessedAccounts)

	// ensure no duplicate hashes
	require.Equal(t, uint64(expectHashes), progress.TotalAccountHashes)
	require.Equal(t, expectHashes, len(writer.hashes))
	for _, count := range writer.hashes {
		require.Equal(t, 1, count)
	}
}
