package batcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	_ "net/http/pprof"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-batcher/metrics"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/dial"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
)

var ErrBatcherNotRunning = errors.New("batcher is not running")

type L1Client interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

type L2Client interface {
	BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error)
}

type L3Client interface {
	BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error)
	// 수정 필요함
}

type RollupClient interface {
	SyncStatus(ctx context.Context) (*eth.SyncStatus, error)
}

// L3Client에 대한 인터페이스를 구현하고, 아래 DriverSetup에 L2 Client를 포함

// DriverSetup is the collection of input/output interfaces and configuration that the driver operates on.
type DriverSetup struct {
	Log              log.Logger
	Metr             metrics.Metricer
	RollupConfig     *rollup.Config
	Config           BatcherConfig
	Txmgr            txmgr.TxManager
	L1Client         L1Client
	L2Client		 L2Client // L2Client를 추가
	EndpointProvider dial.L2EndpointProvider
	ChannelConfig    ChannelConfig
}

// BatchSubmitter encapsulates a service responsible for submitting L2 tx
// batches to L1 for availability.
type BatchSubmitter struct {
	DriverSetup

	wg sync.WaitGroup

	shutdownCtx       context.Context
	cancelShutdownCtx context.CancelFunc
	killCtx           context.Context
	cancelKillCtx     context.CancelFunc

	mutex   sync.Mutex
	running bool

	// lastStoredBlock is the last block loaded into `state`. If it is empty it should be set to the l2 safe head.
	lastStoredBlock eth.BlockID
	lastL1Tip       eth.L1BlockRef

	// 위 역할을 하는 l2를 위한 포함 요소 추가
	// op.BlockID와 op.L2BlockRef는 구현된 함수가 아닌 가상의 함수
	lastStoredBlockInL2 op.BlockID
	lastL2Tip           op.L2BlockRef

	state *channelManager
}

// NewBatchSubmitter initializes the BatchSubmitter driver from a preconfigured DriverSetup
func NewBatchSubmitter(setup DriverSetup) *BatchSubmitter {
	return &BatchSubmitter{
		DriverSetup: setup,
		state:       NewChannelManager(setup.Log, setup.Metr, setup.ChannelConfig, setup.RollupConfig),
	}
}

func (l *BatchSubmitter) StartBatchSubmitting() error {
	l.Log.Info("Starting Batch Submitter")

	l.mutex.Lock()
	defer l.mutex.Unlock()

	if l.running {
		return errors.New("batcher is already running")
	}
	l.running = true

	l.shutdownCtx, l.cancelShutdownCtx = context.WithCancel(context.Background())
	l.killCtx, l.cancelKillCtx = context.WithCancel(context.Background())
	l.state.Clear()
	l.lastStoredBlock = eth.BlockID{}

	l.wg.Add(1)
	go l.loop()

	l.Log.Info("Batch Submitter started")
	return nil
}

func (l *BatchSubmitter) StopBatchSubmittingIfRunning(ctx context.Context) error {
	err := l.StopBatchSubmitting(ctx)
	if errors.Is(err, ErrBatcherNotRunning) {
		return nil
	}
	return err
}

// StopBatchSubmitting stops the batch-submitter loop, and force-kills if the provided ctx is done.
func (l *BatchSubmitter) StopBatchSubmitting(ctx context.Context) error {
	l.Log.Info("Stopping Batch Submitter")

	l.mutex.Lock()
	defer l.mutex.Unlock()

	if !l.running {
		return ErrBatcherNotRunning
	}
	l.running = false

	// go routine will call cancelKill() if the passed in ctx is ever Done
	cancelKill := l.cancelKillCtx
	wrapped, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-wrapped.Done()
		cancelKill()
	}()

	l.cancelShutdownCtx()
	l.wg.Wait()
	l.cancelKillCtx()

	l.Log.Info("Batch Submitter stopped")
	return nil
}

// loadBlocksIntoState loads all blocks since the previous stored block
// It does the following:
// 1. Fetch the sync status of the sequencer
// 2. Check if the sync status is valid or if we are all the way up to date
// 3. Check if it needs to initialize state OR it is lagging (todo: lagging just means race condition?)
// 4. Load all new blocks into the local state.
// If there is a reorg, it will reset the last stored block but not clear the internal state so
// the state can be flushed to L1.
func (l *BatchSubmitter) loadBlocksIntoState(ctx context.Context) error {
	start, end, err := l.calculateL2BlockRangeToStore(ctx)
	if err != nil {
		l.Log.Warn("Error calculating L2 block range", "err", err)
		return err
	} else if start.Number >= end.Number {
		return errors.New("start number is >= end number")
	}

	var latestBlock *types.Block
	// Add all blocks to "state"
	for i := start.Number + 1; i < end.Number+1; i++ {
		block, err := l.loadBlockIntoState(ctx, i)
		if errors.Is(err, ErrReorg) {
			l.Log.Warn("Found L2 reorg", "block_number", i)
			l.lastStoredBlock = eth.BlockID{}
			return err
		} else if err != nil {
			l.Log.Warn("failed to load block into state", "err", err)
			return err
		}
		l.lastStoredBlock = eth.ToBlockID(block)
		latestBlock = block
	}

	l2ref, err := derive.L2BlockToBlockRef(latestBlock, &l.RollupConfig.Genesis)
	if err != nil {
		l.Log.Warn("Invalid L2 block loaded into state", "err", err)
		return err
	}

	l.Metr.RecordL2BlocksLoaded(l2ref)
	return nil
}

// loadBlockIntoState fetches & stores a single block into `state`. It returns the block it loaded.
func (l *BatchSubmitter) loadBlockIntoState(ctx context.Context, blockNumber uint64) (*types.Block, error) {
	ctx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()
	l2Client, err := l.EndpointProvider.EthClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting L2 client: %w", err)
	}
	block, err := l2Client.BlockByNumber(ctx, new(big.Int).SetUint64(blockNumber))
	if err != nil {
		return nil, fmt.Errorf("getting L2 block: %w", err)
	}

	if err := l.state.AddL2Block(block); err != nil {
		return nil, fmt.Errorf("adding L2 block to state: %w", err)
	}

	l.Log.Info("added L2 block to local state", "block", eth.ToBlockID(block), "tx_count", len(block.Transactions()), "time", block.Time())
	return block, nil
}

// calculateL2BlockRangeToStore determines the range (start,end] that should be loaded into the local state.
// It also takes care of initializing some local state (i.e. will modify l.lastStoredBlock in certain conditions)
func (l *BatchSubmitter) calculateL2BlockRangeToStore(ctx context.Context) (eth.BlockID, eth.BlockID, error) {
	ctx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()
	rollupClient, err := l.EndpointProvider.RollupClient(ctx)
	if err != nil {
		return eth.BlockID{}, eth.BlockID{}, fmt.Errorf("getting rollup client: %w", err)
	}
	syncStatus, err := rollupClient.SyncStatus(ctx)
	/* todo
	syncStatus를 받아올 때, 기존 safe, unsafe 말고
	L3에서 사용하는 unsafe, safeInL2, safeInL1을 반환해주도록
	SyncStatus 함수를 수정해줘야 함
	*/
	// Ensure that we have the sync status
	if err != nil {
		return eth.BlockID{}, eth.BlockID{}, fmt.Errorf("failed to get sync status: %w", err)
	}
	if syncStatus.HeadL1 == (eth.L1BlockRef{}) {
		return eth.BlockID{}, eth.BlockID{}, errors.New("empty sync status")
	}

	// Check last stored to see if it needs to be set on startup OR set if is lagged behind.
	// It lagging implies that the op-node processed some batches that were submitted prior to the current instance of the batcher being alive.
	if l.lastStoredBlock == (eth.BlockID{}) {
		l.Log.Info("Starting batch-submitter work at safe-head", "safe", syncStatus.SafeL2)
		l.lastStoredBlock = syncStatus.SafeL2.ID()
	} else if l.lastStoredBlock.Number < syncStatus.SafeL2.Number {
		l.Log.Warn("last submitted block lagged behind L2 safe head: batch submission will continue from the safe head now", "last", l.lastStoredBlock, "safe", syncStatus.SafeL2)
		l.lastStoredBlock = syncStatus.SafeL2.ID()
	}

	// Check if we should even attempt to load any blocks. TODO: May not need this check
	if syncStatus.SafeL2.Number >= syncStatus.UnsafeL2.Number {
		return eth.BlockID{}, eth.BlockID{}, errors.New("L2 safe head ahead of L2 unsafe head")
	}

	if(L2Status == 1) {
		return syncStatus.SafeInL1.ID(), syncStatus.UnsafeL2.ID(), nil
		// SafeInL1이 된 마지막 블록 다음부터를 싱크할 블록으로 설정함
	}

	return l.lastStoredBlock, syncStatus.UnsafeL2.ID(), nil
}

// The following things occur:
// New L2 block (reorg or not)
// L1 transaction is confirmed
//
// What the batcher does:
// Ensure that channels are created & submitted as frames for an L2 range
//
// Error conditions:
// Submitted batch, but it is not valid
// Missed L2 block somehow.

/* todo
1. L2가 멈춘 것을 감지 -> loop 시작 부분에 정상 상태 / L2 장애 상태로 case를 나누어 변수에 할당
2. L2가 멈춘 것이 확인 -> SWS 동안의 데이터를 L1으로 올려주는 코드
3. L2가 복구 -> L2로 제출 위치 다시 변경
*/
func (l *BatchSubmitter) loop() {
	defer l.wg.Done()

	ticker := time.NewTicker(l.Config.PollInterval)
	defer ticker.Stop()
	/*
	todo: 1. L2가 멈춘 것을 감지를 구현하는 수도코드를 loop 아래의 getL2Status() 함수에 구현

	L2가 멈춘 것을 감지하는 함수, getL2Status() 라고 가정, 정상이면 0, 멈췄으면 1 리턴한다고 가정
	L2Status := getStatus()
	*/
	receiptsCh := make(chan txmgr.TxReceipt[txData])
	queue := txmgr.NewQueue[txData](l.killCtx, l.Txmgr, l.Config.MaxPendingTransactions)

	for {
		select {
		case <-ticker.C:
			if err := l.loadBlocksIntoState(l.shutdownCtx); errors.Is(err, ErrReorg) {
				// 위 함수에 대해 l2가 멈춘 상황에 대한 코드를 수정함
				err := l.state.Close()
				if err != nil {
					if errors.Is(err, ErrPendingAfterClose) {
						l.Log.Warn("Closed channel manager to handle L2 reorg with pending channel(s) remaining - submitting")
					} else {
						l.Log.Error("Error closing the channel manager to handle a L2 reorg", "err", err)
					}
				}
				l.publishStateToL1(queue, receiptsCh, true)
				l.state.Clear()
				continue
			}
			l.publishStateToL1(queue, receiptsCh, false)
		case r := <-receiptsCh:
			l.handleReceipt(r)
		case <-l.shutdownCtx.Done():
			// This removes any never-submitted pending channels, so these do not have to be drained with transactions.
			// Any remaining unfinished channel is terminated, so its data gets submitted.
			err := l.state.Close()
			if err != nil {
				if errors.Is(err, ErrPendingAfterClose) {
					l.Log.Warn("Closed channel manager on shutdown with pending channel(s) remaining - submitting")
				} else {
					l.Log.Error("Error closing the channel manager on shutdown", "err", err)
				}
			}
			l.publishStateToL1(queue, receiptsCh, true)
			l.Log.Info("Finished publishing all remaining channel data")
			return
		}
	}
}

// todo 위에 언급된 L2 상태를 확인하는 함수를 구현
func getL2Status() {
	// block derivation 코드를 이용하자
	// op-node / rollup / derive / l1_traversal.go 코드를 이용
	AdvancedL1Block(ctx context.Context);
	// 이걸로 다음 l1 블록의 header 정보를 읽어옴 + L1 reorg 여부를 파악
	// 다음 L1 블록의 receipt를 가져온 후 UpdateSystemConfigWithL1Receipts 함수를 통해 L1 system configuration을 업데이트하고, 이어서 블록의 Header를 L1Traversal 구조체에 업데이트

	// 그 후 L1 Retrieval 코드를 이용하자 (사실 호출 순서는 L1 retrieval -> L1 trieval)
	// op-node / rollup / derive / l1_retrieval.go
	NextData(ctx context.Context);
	// 블록 header 정보가 존재한다면, dataSrc의 OpenData 메소드를 호출하여 context, Next L1 block ID, batcher contract address를 받아와 블록 header 정보를 읽고 그 안에서 batcher transaction 데이터를 추출
}


// publishStateToL1 loops through the block data loaded into `state` and
// submits the associated data to the L1 in the form of channel frames.
func (l *BatchSubmitter) publishStateToL1(queue *txmgr.Queue[txData], receiptsCh chan txmgr.TxReceipt[txData], drain bool) {
	txDone := make(chan struct{})
	// send/wait and receipt reading must be on a separate goroutines to avoid deadlocks
	go func() {
		defer func() {
			if drain {
				// if draining, we wait for all transactions to complete
				queue.Wait()
			}
			close(txDone)
		}()
		for {
			err := l.publishTxToL1(l.killCtx, queue, receiptsCh)
			if err != nil {
				if drain && err != io.EOF {
					l.Log.Error("error sending tx while draining state", "err", err)
				}
				return
			}
		}
	}()

	for {
		select {
		case r := <-receiptsCh:
			l.handleReceipt(r)
		case <-txDone:
			return
		}
	}
}

// publishTxToL1 submits a single state tx to the L1
func (l *BatchSubmitter) publishTxToL1(ctx context.Context, queue *txmgr.Queue[txData], receiptsCh chan txmgr.TxReceipt[txData]) error {
	// send all available transactions
	// L2Status의 값에 따라 해당 체인을 설정

	/* 기존 코드
	l1tip, err := l.l1Tip(ctx)
	if err != nil {
		l.Log.Error("Failed to query L1 tip", "err", err)
		return err
	}
	l.recordL1Tip(l1tip)
	*/

	// 수정한 코드, 변수 이름은 그대로 둠
	if(L2Status == 0) {
		l1tip, err := l.l2Tip(ctx) // l2Tip 함수는 l1Tip 함수 아래에 추가로 구현함
		if err != nil {
			l.Log.Error("Failed to query L2 tip", "err", err)
			return err
		}
		l.recordL1Tip(l1tip)
	}
	else {
		// 기존 코드와 같이 L1과 상호작용
		l1tip, err := l.l1Tip(ctx)
		if err != nil {
			l.Log.Error("Failed to query L1 tip", "err", err)
			return err
		}
		l.recordL1Tip(l1tip)
	}

	// Collect next transaction data
	txdata, err := l.state.TxData(l1tip.ID())
	if err == io.EOF {
		l.Log.Trace("no transaction data available")
		return err
	} else if err != nil {
		l.Log.Error("unable to get tx data", "err", err)
		return err
	}

	l.sendTransaction(txdata, queue, receiptsCh)
	return nil
}

// sendTransaction creates & submits a transaction to the batch inbox address with the given `data`.
// It currently uses the underlying `txmgr` to handle transaction sending & price management.
// This is a blocking method. It should not be called concurrently.
func (l *BatchSubmitter) sendTransaction(txdata txData, queue *txmgr.Queue[txData], receiptsCh chan txmgr.TxReceipt[txData]) {
	// Do the gas estimation offline. A value of 0 will cause the [txmgr] to estimate the gas limit.
	data := txdata.Bytes()
	if(L2Status == 0) {
		intrinsicGas, err := core_op.IntrinsicGas(data, nil, false, true, true, false)
		// 기존 코드는 import한 core를 이용 -> 아직 구현하지 않은 core_op로 op 가스비 가져온다고 가정함
		if err != nil {
			l.Log.Error("Failed to calculate intrinsic gas", "err", err)
			return
		}
		candidate := txmgr.TxCandidate{
			To:       &l.RollupConfig.BatchInboxAddress,
			// BatchInboxAddress에는 L2 batchInboxAddress가 저장되어 있을 것임
			TxData:   data,
			GasLimit: intrinsicGas,
		}
	}
	else {
		intrinsicGas, err := core.IntrinsicGas(data, nil, false, true, true, false)
		if err != nil {
			l.Log.Error("Failed to calculate intrinsic gas", "err", err)
			return
		}
		candidate := txmgr.TxCandidate{
			To:       &l.RollupConfig.BatchInboxAddressL1,
			// BatchInboxAddressL1라는 변수를 RollupConfig에 추가하여 설정해야 함
			// 역추적하다가 어디서 정의되는지 못찾겠어서 나중에..
			TxData:   data,
			GasLimit: intrinsicGas,
		}
	}
	// L2가 멈춘 경우 기존 코드 그대로 L1 가스비 이용

	/*
	candidate := txmgr.TxCandidate{
		To:       &l.RollupConfig.BatchInboxAddress,
		TxData:   data,
		GasLimit: intrinsicGas,
	}
	*/
	// 위 코드를 if문 안에 집어 넣음

	queue.Send(txdata, candidate, receiptsCh)
	// Send에서 사용하는 RPC url을 L1 / L2에 따라서 할당해줘야 할텐데 이걸 어디서 설정해주는지 못찾겠음
}

func (l *BatchSubmitter) handleReceipt(r txmgr.TxReceipt[txData]) {
	// Record TX Status
	if r.Err != nil {
		l.recordFailedTx(r.ID, r.Err)
	} else {
		l.recordConfirmedTx(r.ID, r.Receipt)
	}
}

func (l *BatchSubmitter) recordL1Tip(l1tip eth.L1BlockRef) {
	if l.lastL1Tip == l1tip {
		return
	}
	l.lastL1Tip = l1tip
	l.Metr.RecordLatestL1Block(l1tip)
}

func (l *BatchSubmitter) recordFailedTx(txd txData, err error) {
	l.Log.Warn("Transaction failed to send", logFields(txd, err)...)
	l.state.TxFailed(txd.ID())
}

func (l *BatchSubmitter) recordConfirmedTx(txd txData, receipt *types.Receipt) {
	l.Log.Info("Transaction confirmed", logFields(txd, receipt)...)
	l1block := eth.ReceiptBlockID(receipt)
	l.state.TxConfirmed(txd.ID(), l1block)
}

// l1Tip gets the current L1 tip as a L1BlockRef. The passed context is assumed
// to be a lifetime context, so it is internally wrapped with a network timeout.
func (l *BatchSubmitter) l1Tip(ctx context.Context) (eth.L1BlockRef, error) {
	tctx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()
	head, err := l.L1Client.HeaderByNumber(tctx, nil)
	if err != nil {
		return eth.L1BlockRef{}, fmt.Errorf("getting latest L1 block: %w", err)
	}
	return eth.InfoToL1BlockRef(eth.HeaderBlockInfo(head)), nil
}

// 위의 l1Tip과 l2에서 같은 기능을 수행하는 l2Tip 함수
func (l *BatchSubmitter) l2Tip(ctx context.Context) (eth.L2BlockRef, error) {
	tctx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()
	head, err := l.L1Client.HeaderByNumber(tctx, nil)
	if err != nil {
		return eth.L1BlockRef{}, fmt.Errorf("getting latest L2 block: %w", err)
	}
	return eth.InfoToL1BlockRef(eth.HeaderBlockInfo(head)), nil
}

func logFields(xs ...any) (fs []any) {
	for _, x := range xs {
		switch v := x.(type) {
		case txData:
			fs = append(fs, "frame_id", v.ID(), "data_len", v.Len())
		case *types.Receipt:
			fs = append(fs, "tx", v.TxHash, "block", eth.ReceiptBlockID(v))
		case error:
			fs = append(fs, "err", v)
		default:
			fs = append(fs, "ERROR", fmt.Sprintf("logFields: unknown type: %T", x))
		}
	}
	return fs
}
