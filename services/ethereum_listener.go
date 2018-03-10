package services

import (
	"errors"
	"fmt"
	"sync"

	"github.com/asdine/storm"
	uuid "github.com/satori/go.uuid"
	"github.com/smartcontractkit/chainlink/logger"
	"github.com/smartcontractkit/chainlink/store"
	"github.com/smartcontractkit/chainlink/store/models"
	"github.com/smartcontractkit/chainlink/utils"
	"go.uber.org/multierr"
)

// EthereumListener manages push notifications from the ethereum node's
// websocket to listen for new heads and log events.
type EthereumListener struct {
	Store            *store.Store
	HeadTracker      *HeadTracker
	jobSubscriptions []JobSubscription
	jobsMutex        sync.Mutex
	headTrackerId    string
}

// Start obtains the jobs from the store and subscribes to logs and newHeads
// in order to start and resume jobs waiting on events or confirmations.
func (el *EthereumListener) Start() error {
	el.headTrackerId = el.HeadTracker.Attach(el)
	return nil
}

// Stop gracefully closes its access to the store's EthNotifications and resets
// resources.
func (el *EthereumListener) Stop() error {
	el.HeadTracker.Detach(el.headTrackerId)
	return nil
}

// AddJob subscribes to ethereum "logs" for each "runlog" and "ethlog"
// initiators in the passed job.
func (el *EthereumListener) AddJob(job models.Job) error {
	if !job.IsLogInitiated() || !el.HeadTracker.IsConnected() {
		return nil
	}

	sub, err := StartJobSubscription(job, el.HeadTracker.Get(), el.Store)
	if err != nil {
		return err
	}
	el.addSubscription(sub)
	return nil
}

func (el *EthereumListener) Jobs() []models.Job {
	var jobs []models.Job
	for _, js := range el.jobSubscriptions {
		jobs = append(jobs, js.Job)
	}
	return jobs
}

func (el *EthereumListener) addSubscription(sub JobSubscription) {
	el.jobsMutex.Lock()
	defer el.jobsMutex.Unlock()
	el.jobSubscriptions = append(el.jobSubscriptions, sub)
}

func (el *EthereumListener) Connect() error {
	jobs, err := el.Store.Jobs()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		err = multierr.Append(err, el.AddJob(j))
	}
	return err
}

func (el *EthereumListener) Disconnect() {
	el.jobsMutex.Lock()
	defer el.jobsMutex.Unlock()
	for _, sub := range el.jobSubscriptions {
		sub.Unsubscribe()
	}
	el.jobSubscriptions = []JobSubscription{}
}

func (el *EthereumListener) OnNewHead(_ *models.BlockHeader) {
	pendingRuns, err := el.Store.PendingJobRuns()
	if err != nil {
		logger.Error(err.Error())
	}
	for _, jr := range pendingRuns {
		if _, err := ExecuteRun(jr, el.Store, models.RunResult{}); err != nil {
			logger.Error(err.Error())
		}
	}
}

type HeadTrackable interface {
	Connect() error
	Disconnect()
	OnNewHead(*models.BlockHeader)
}

type NoOpHeadTrackable struct{}

func (NoOpHeadTrackable) Connect() error                { return nil }
func (NoOpHeadTrackable) Disconnect()                   {}
func (NoOpHeadTrackable) OnNewHead(*models.BlockHeader) {}

// Holds and stores the latest block number experienced by this particular node
// in a thread safe manner. Reconstitutes the last block number from the data
// store on reboot.
type HeadTracker struct {
	trackers         map[string]HeadTrackable
	headers          chan models.BlockHeader
	headSubscription models.EthSubscription
	store            *store.Store
	number           *models.IndexableBlockNumber
	headMutex        sync.RWMutex
	trackersMutex    sync.RWMutex
	connected        bool
	sleeper          utils.Sleeper
}

// Instantiates a new HeadTracker using the orm to persist new block numbers
func NewHeadTracker(store *store.Store, sleepers ...utils.Sleeper) *HeadTracker {
	var sleeper utils.Sleeper
	if len(sleepers) > 0 {
		sleeper = sleepers[0]
	} else {
		sleeper = utils.NewBackoffSleeper()
	}
	return &HeadTracker{store: store, trackers: map[string]HeadTrackable{}, sleeper: sleeper}
}

func (ht *HeadTracker) Start() error {
	numbers := []models.IndexableBlockNumber{}
	err := ht.store.Select().OrderBy("Digits", "Number").Limit(1).Reverse().Find(&numbers)
	if err != nil && err != storm.ErrNotFound {
		return err
	}
	if len(numbers) > 0 {
		ht.number = &numbers[0]
	}

	ht.headers = make(chan models.BlockHeader)
	sub, err := ht.subscribeToNewHeads()
	if err != nil {
		return err
	}
	ht.headSubscription = sub
	ht.Connect()
	go ht.listenToNewHeads()
	return nil
}

func (ht *HeadTracker) Stop() error {
	if ht.headSubscription != nil {
		ht.headSubscription.Unsubscribe()
		ht.headSubscription = nil
	}
	if ht.headers != nil {
		close(ht.headers)
		ht.headers = nil
	}
	ht.Disconnect()
	return nil
}

// Updates the latest block number, if indeed the latest, and persists
// this number in case of reboot. Thread safe.
func (ht *HeadTracker) Save(n *models.IndexableBlockNumber) error {
	if n == nil {
		return errors.New("Cannot save a nil block header")
	}

	ht.headMutex.Lock()
	if ht.number == nil || ht.number.ToInt().Cmp(n.ToInt()) < 0 {
		copy := *n
		ht.number = &copy
	}
	ht.headMutex.Unlock()
	return ht.store.Save(n)
}

// Returns the latest block header being tracked, or nil.
func (ht *HeadTracker) Get() *models.IndexableBlockNumber {
	ht.headMutex.RLock()
	defer ht.headMutex.RUnlock()
	return ht.number
}

func (ht *HeadTracker) Attach(t HeadTrackable) string {
	ht.trackersMutex.Lock()
	defer ht.trackersMutex.Unlock()
	id := uuid.Must(uuid.NewV4()).String()
	ht.trackers[id] = t
	if ht.connected {
		t.Connect()
	}
	return id
}

func (ht *HeadTracker) Detach(id string) {
	ht.trackersMutex.Lock()
	defer ht.trackersMutex.Unlock()
	t, present := ht.trackers[id]
	if ht.connected && present {
		t.Disconnect()
	}
	delete(ht.trackers, id)
}

func (ht *HeadTracker) IsConnected() bool { return ht.connected }

func (ht *HeadTracker) Connect() {
	ht.trackersMutex.RLock()
	defer ht.trackersMutex.RUnlock()
	ht.connected = true
	for _, t := range ht.trackers {
		logger.WarnIf(t.Connect())
	}
}

func (ht *HeadTracker) Disconnect() {
	ht.trackersMutex.RLock()
	defer ht.trackersMutex.RUnlock()
	ht.connected = false
	for _, t := range ht.trackers {
		t.Disconnect()
	}
}

func (ht *HeadTracker) OnNewHead(head *models.BlockHeader) {
	ht.trackersMutex.RLock()
	defer ht.trackersMutex.RUnlock()
	for _, t := range ht.trackers {
		t.OnNewHead(head)
	}
}

func (ht *HeadTracker) subscribeToNewHeads() (models.EthSubscription, error) {
	sub, err := ht.store.TxManager.SubscribeToNewHeads(ht.headers)
	if err != nil {
		return nil, err
	}
	go func() {
		err := <-sub.Err()
		if err != nil {
			logger.Warnw("Error in new head subscription, disconnected", "err", err)
			ht.Stop()
			ht.reconnectLoop()
		}
	}()
	return sub, nil
}

func (ht *HeadTracker) listenToNewHeads() {
	if ht.number != nil {
		logger.Info("Tracking logs from block ", ht.number.FriendlyString(), " with hash ", ht.number.Hash.String())
	}
	for header := range ht.headers {
		number := header.IndexableBlockNumber()
		logger.Debugw(fmt.Sprintf("Received header %v", number.FriendlyString()), "hash", header.Hash())
		if err := ht.Save(number); err != nil {
			logger.Error(err.Error())
		} else {
			ht.OnNewHead(&header)
		}
	}
}

func (ht *HeadTracker) reconnectLoop() {
	ht.sleeper.Reset()
	for {
		logger.Info("Reconnecting to node ", ht.store.Config.EthereumURL, " in ", ht.sleeper.Duration())
		ht.sleeper.Sleep()
		err := ht.Start()
		if err != nil {
			logger.Warnw(fmt.Sprintf("Error reconnecting to %v", ht.store.Config.EthereumURL), "err", err)
			ht.Stop()
		} else {
			logger.Info("Reconnected to node ", ht.store.Config.EthereumURL)
			break
		}
	}
}
