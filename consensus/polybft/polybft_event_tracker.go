package polybft

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/0xPolygon/polygon-edge/helper/common"
	edgeTracker "github.com/0xPolygon/polygon-edge/tracker"
	hcf "github.com/hashicorp/go-hclog"
	"github.com/umbracle/ethgo"
	"github.com/umbracle/ethgo/blocktracker"
)

// BlockProvider is an interface that defines methods for retrieving blocks and logs from a blockchain
type BlockProvider interface {
	GetBlockByHash(hash ethgo.Hash, full bool) (*ethgo.Block, error)
	GetBlockByNumber(i ethgo.BlockNumber, full bool) (*ethgo.Block, error)
	GetLogs(filter *ethgo.LogFilter) ([]*ethgo.Log, error)
}

// PolybftTrackerConfig is a struct that holds configuration of a PolybftEventTracker
type PolybftTrackerConfig struct {
	// RpcEndpoint is the full json rpc url on some node on a tracked chain
	RpcEndpoint string

	// StartBlockFromConfig represents a starting block from which tracker starts to
	// track events from a tracked chain.
	// This is only relevant on the first start of the tracker. After it processes blocks,
	// it will start from the last processed block, and not from the StartBlockFromConfig.
	StartBlockFromConfig uint64

	// NumBlockConfirmations defines how many blocks must pass from a certain block,
	// to consider that block as final on the tracked chain.
	// This is very important for reorgs, and events from the given block will only be
	// processed if it hits this confirmation mark.
	// (e.g., NumBlockConfirmations = 3, and if the last tracked block is 10,
	// events from block 10, will only be processed when we get block 13 from the tracked chain)
	NumBlockConfirmations uint64

	// SyncBatchSize defines a batch size of blocks that will be gotten from tracked chain,
	// when tracker is out of sync and needs to sync a number of blocks.
	// (e.g., SyncBatchSize = 10, trackers last processed block is 10, latest block on tracked chain is 100,
	// it will get blocks 11-20, get logs from confirmed blocks of given batch, remove processed confirm logs
	// from memory, and continue to the next batch)
	SyncBatchSize uint64

	// MaxBacklogSize defines how many blocks we will sync up from the latest block on tracked chain.
	// If a node that has tracker, was offline for days, months, a year, it will miss a lot of blocks.
	// In the meantime, we expect the rest of nodes to have collected the desired events and did their
	// logic with them, continuing consensus and relayer stuff.
	// In order to not waste too much unnecessary time in syncing all those blocks, with MaxBacklogSize,
	// we tell the tracker to sync only latestBlock.Number - MaxBacklogSize number of blocks.
	MaxBacklogSize uint64

	// PollInterval defines a time interval in which tracker polls json rpc node
	// for latest block on the tracked chain.
	PollInterval time.Duration

	// Logger is the logger instance for event tracker
	Logger hcf.Logger

	// LogFilter defines which events are tracked and from which contracts on the tracked chain
	LogFilter map[ethgo.Address][]ethgo.Hash

	// Store is the store implementation for data that tracker saves (lastProcessedBlock and logs)
	Store EventTrackerStore

	// BlockProvider is the implementation of a provider that returns blocks and logs from tracked chain
	BlockProvider BlockProvider

	// EventSubscriber is the subscriber that requires events tracked by the event tracker
	EventSubscriber edgeTracker.EventSubscription
}

// PolybftEventTracker represents a tracker for events on desired contracts on some chain
type PolybftEventTracker struct {
	config *PolybftTrackerConfig

	closeCh chan struct{}
	once    sync.Once

	blockTracker   blocktracker.BlockTrackerInterface
	blockContainer *TrackerBlockContainer
}

// NewPolybftEventTracker is a constructor function that creates a new instance of the PolybftEventTracker struct.
//
// Example Usage:
//
//	config := &PolybftEventTracker{
//		RpcEndpoint:           "http://some-json-rpc-url.com",
//		StartBlockFromConfig:  100_000,
//		NumBlockConfirmations: 10,
//		SyncBatchSize:         20,
//		MaxBacklogSize:        10_000,
//		PollInterval:          2 * time.Second,
//		Logger:                logger,
//		Store:                 store,
//		EventSubscriber:       subscriber,
//		Provider:              provider,
//		LogFilter: TrackerLogFilter{
//			Addresses: []ethgo.Address{addressOfSomeContract},
//			IDs:       []ethgo.Hash{idHashOfSomeEvent},
//		},
//	}
//		t := NewPolybftEventTracker(config)
//
// Inputs:
//   - config (TrackerConfig): configuration of PolybftEventTracker.
//
// Outputs:
//   - A new instance of the PolybftEventTracker struct.
func NewPolybftEventTracker(config *PolybftTrackerConfig) (*PolybftEventTracker, error) {
	lastProcessedBlock, err := config.Store.GetLastProcessedBlock()
	if err != nil {
		return nil, err
	}

	var definiteLastProcessedBlock uint64
	if config.StartBlockFromConfig > 0 {
		definiteLastProcessedBlock = config.StartBlockFromConfig - 1
	}

	if lastProcessedBlock > definiteLastProcessedBlock {
		definiteLastProcessedBlock = lastProcessedBlock
	}

	return &PolybftEventTracker{
		config:         config,
		closeCh:        make(chan struct{}),
		blockTracker:   blocktracker.NewJSONBlockTracker(config.BlockProvider),
		blockContainer: NewTrackerBlockContainer(definiteLastProcessedBlock),
	}, nil
}

// Close closes the PolybftEventTracker by closing the closeCh channel.
// This method is used to signal the goroutines to stop.
//
// Example Usage:
//
//	tracker := NewPolybftEventTracker(config)
//	tracker.Start()
//	defer tracker.Close()
//
// Inputs: None
//
// Flow:
//  1. The Close() method is called on an instance of PolybftEventTracker.
//  2. The closeCh channel is closed, which signals the goroutines to stop.
//
// Outputs: None
func (p *PolybftEventTracker) Close() {
	close(p.closeCh)
}

// Start is a method in the PolybftEventTracker struct that starts the tracking of blocks
// and retrieval of logs from given blocks from the tracked chain.
// If the tracker was turned off (node was down) for some time, it will sync up all the missed
// blocks and logs from the last start (in regards to MaxBacklogSize field in config).
//
// Returns:
//   - nil if start passes successfully.
//   - An error if there is an error on startup of blocks tracking on tracked chain.
func (p *PolybftEventTracker) Start() error {
	p.config.Logger.Info("Starting event tracker",
		"jsonRpcEndpoint", p.config.RpcEndpoint,
		"startBlockFromConfig", p.config.StartBlockFromConfig,
		"numBlockConfirmations", p.config.NumBlockConfirmations,
		"pollInterval", p.config.PollInterval,
		"syncBatchSize", p.config.SyncBatchSize,
		"maxBacklogSize", p.config.MaxBacklogSize,
		"logFilter", p.config.LogFilter,
	)

	ctx, cancelFn := context.WithCancel(context.Background())
	go func() {
		<-p.closeCh
		cancelFn()
	}()

	go common.RetryForever(ctx, time.Second, func(context.Context) error {
		// sync up all missed blocks on start if it is not already sync up
		if err := p.syncOnStart(); err != nil {
			p.config.Logger.Error("Syncing up on start failed.", "err", err)

			return err
		}

		// start the polling of blocks
		err := p.blockTracker.Track(ctx, func(block *ethgo.Block) error {
			return p.trackBlock(block)
		})

		if common.IsContextDone(err) {
			return nil
		}

		return err
	})

	return nil
}

// trackBlock is a method in the PolybftEventTracker struct that is responsible for tracking blocks and processing their logs
//
// Inputs:
// - block: An instance of the ethgo.Block struct representing a block to track.
//
// Returns:
//   - nil if tracking block passes successfully.
//   - An error if there is an error on tracking given block.
func (p *PolybftEventTracker) trackBlock(block *ethgo.Block) error {
	if !p.blockContainer.IsOutOfSync(block) {
		p.blockContainer.AcquireWriteLock()
		defer p.blockContainer.ReleaseWriteLock()

		if p.blockContainer.LastCachedBlock() < block.Number {
			// we are not out of sync, it's a sequential add of new block
			p.blockContainer.AddBlock(block)
		}

		// check if some blocks reached confirmation level so that we can process their logs
		return p.processLogs()
	}

	// we are out of sync (either we missed some blocks, or a reorg happened)
	// so we get remove the old pending state and get the new one
	return p.getNewState(block)
}

// syncOnStart is a method in the PolybftEventTracker struct that is responsible for syncing the event tracker on startup.
// It retrieves the latest block and checks if the event tracker is out of sync.
// If it is out of sync, it calls the getNewState method to update the state.
//
// Returns:
//   - nil if sync passes successfully, or no sync is done.
//   - An error if there is an error retrieving blocks or logs from the external provider or saving logs to the store.
func (p *PolybftEventTracker) syncOnStart() (err error) {
	var latestBlock *ethgo.Block
	p.once.Do(func() {
		p.config.Logger.Info("Syncing up on start...")
		latestBlock, err = p.config.BlockProvider.GetBlockByNumber(ethgo.Latest, false)
		if err != nil {
			return
		}

		if !p.blockContainer.IsOutOfSync(latestBlock) {
			p.config.Logger.Info("Everything synced up on start")

			return
		}

		err = p.getNewState(latestBlock)
	})

	return err
}

// getNewState is called if tracker is out of sync (it missed some blocks),
// or a reorg happened in the tracked chain.
// It acquires write lock on the block container, so that the state is not changed while it
// retrieves the new blocks (new state).
// It will clean the previously cached state (non confirmed blocks), get the new state,
// set it on the block container and process logs on the confirmed blocks on the new state
//
// Input:
//   - latestBlock - latest block on the tracked chain
//
// Returns:
//   - nil if there are no confirmed blocks.
//   - An error if there is an error retrieving blocks or logs from the external provider or saving logs to the store.
func (p *PolybftEventTracker) getNewState(latestBlock *ethgo.Block) error {
	lastProcessedBlock := p.blockContainer.LastProcessedBlock()

	p.config.Logger.Info("Getting new state, since some blocks were missed",
		"lastProcessedBlock", lastProcessedBlock, "latestBlockFromRpc", latestBlock.Number)

	p.blockContainer.AcquireWriteLock()
	defer p.blockContainer.ReleaseWriteLock()

	// clean old state
	p.blockContainer.CleanState()

	startBlock := lastProcessedBlock + 1

	// sanitize startBlock from which we will start polling for blocks
	if latestBlock.Number > p.config.MaxBacklogSize &&
		latestBlock.Number-p.config.MaxBacklogSize > lastProcessedBlock {
		startBlock = latestBlock.Number - p.config.MaxBacklogSize
	}

	// get blocks in batches
	for i := startBlock; i <= latestBlock.Number; i += p.config.SyncBatchSize {
		end := i + p.config.SyncBatchSize
		if end > latestBlock.Number {
			// we go until the latest block, since we don't need to
			// query for it using an rpc point, since we already have it
			end = latestBlock.Number - 1
		}

		if i < end {
			p.config.Logger.Info("Getting new state for block batch", "fromBlock", i, "toBlock", end)
		}

		// get and add blocks in batch
		for j := i; j < end; j++ {
			block, err := p.config.BlockProvider.GetBlockByNumber(ethgo.BlockNumber(j), false)
			if err != nil {
				p.config.Logger.Error("Getting new state for block batch failed on rpc call",
					"fromBlock", i,
					"toBlock", end,
					"currentBlock", j,
					"err", err)

				return err
			}

			p.blockContainer.AddBlock(block)
		}

		// now process logs from confirmed blocks if any
		if err := p.processLogs(); err != nil {
			return err
		}
	}

	// add latest block
	p.blockContainer.AddBlock(latestBlock)

	// process logs if there are more confirmed events
	if err := p.processLogs(); err != nil {
		p.config.Logger.Error("Getting new state failed",
			"lastProcessedBlock", lastProcessedBlock,
			"latestBlockFromRpc", latestBlock.Number,
			"err", err)

		return err
	}

	p.config.Logger.Info("Getting new state finished",
		"newLastProcessedBlock", p.blockContainer.LastProcessedBlockLocked(),
		"latestBlockFromRpc", latestBlock.Number)

	return nil
}

// ProcessLogs retrieves logs for confirmed blocks, filters them based on certain criteria,
// passes them to the subscriber, and stores them in a store.
// It also removes the processed blocks from the block container.
//
// Returns:
// - nil if there are no confirmed blocks.
// - An error if there is an error retrieving logs from the external provider or saving logs to the store.
func (p *PolybftEventTracker) processLogs() error {
	confirmedBlocks := p.blockContainer.GetConfirmedBlocks(p.config.NumBlockConfirmations)
	if confirmedBlocks == nil {
		// no confirmed blocks, so nothing to process
		p.config.Logger.Debug("No confirmed blocks. Nothing to process")

		return nil
	}

	fromBlock := confirmedBlocks[0]
	toBlock := confirmedBlocks[len(confirmedBlocks)-1]

	logs, err := p.config.BlockProvider.GetLogs(p.getLogsQuery(fromBlock, toBlock))
	if err != nil {
		p.config.Logger.Error("Process logs failed on getting logs from rpc",
			"fromBlock", fromBlock,
			"toBlock", toBlock,
			"err", err)

		return err
	}

	filteredLogs := make([]*ethgo.Log, 0, len(logs))
	for _, log := range logs {
		logIDs, exist := p.config.LogFilter[log.Address]
		if !exist {
			continue
		}

		for _, id := range logIDs {
			if log.Topics[0] == id {
				filteredLogs = append(filteredLogs, log)
				p.config.EventSubscriber.AddLog(log)

				break
			}
		}
	}

	if err := p.config.Store.InsertLastProcessedBlock(toBlock); err != nil {
		p.config.Logger.Error("Process logs failed on saving last processed block",
			"fromBlock", fromBlock,
			"toBlock", toBlock,
			"err", err)

		return err
	}

	if err := p.config.Store.InsertLogs(filteredLogs); err != nil {
		p.config.Logger.Error("Process logs failed on saving logs to store",
			"fromBlock", fromBlock,
			"toBlock", toBlock,
			"err", err)

		return err
	}

	if err := p.blockContainer.RemoveBlocks(fromBlock, toBlock); err != nil {
		return fmt.Errorf("could not remove processed blocks. Err: %w", err)
	}

	p.config.Logger.Debug("Processing logs for blocks finished",
		"fromBlock", fromBlock,
		"toBlock", toBlock,
		"numOfLogs", len(filteredLogs))

	return nil
}

// getLogsQuery is a method of the PolybftEventTracker struct that creates and returns a LogFilter object with the specified block range.
//
// Input:
//   - from (uint64): The starting block number for the log filter.
//   - to (uint64): The ending block number for the log filter.
//
// Returns:
//   - filter (*ethgo.LogFilter): The created LogFilter object with the specified block range.
func (p *PolybftEventTracker) getLogsQuery(from, to uint64) *ethgo.LogFilter {
	addresses := make([]ethgo.Address, 0, len(p.config.LogFilter))
	for a := range p.config.LogFilter {
		addresses = append(addresses, a)
	}

	filter := &ethgo.LogFilter{Address: addresses}
	filter.SetFromUint64(from)
	filter.SetToUint64(to)

	return filter
}
