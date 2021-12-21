package rpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"go.viam.com/utils"
)

// A memoryWebRTCCallQueue is an in-memory implementation of a call queue designed to be used for
// testing and single node deployments.
type memoryWebRTCCallQueue struct {
	mu                      sync.Mutex
	activeBackgroundWorkers sync.WaitGroup
	hostQueues              map[string]*singleWebRTCHostQueue

	cancelCtx  context.Context
	cancelFunc func()

	uuidDeterministic        bool
	uuidDeterministicCounter int64
}

// NewMemoryWebRTCCallQueue returns a new, empty in-memory call queue.
func NewMemoryWebRTCCallQueue() WebRTCCallQueue {
	return newMemoryWebRTCCallQueue(false)
}

// newMemoryWebRTCCallQueueTest returns a new, empty in-memory call queue for testing.
// It uses predictable UUIDs.
func newMemoryWebRTCCallQueueTest() *memoryWebRTCCallQueue {
	return newMemoryWebRTCCallQueue(true)
}

func newMemoryWebRTCCallQueue(uuidDeterministic bool) *memoryWebRTCCallQueue {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	queue := &memoryWebRTCCallQueue{
		hostQueues:        map[string]*singleWebRTCHostQueue{},
		cancelCtx:         cancelCtx,
		cancelFunc:        cancelFunc,
		uuidDeterministic: uuidDeterministic,
	}
	queue.activeBackgroundWorkers.Add(1)
	ticker := time.NewTicker(1 * time.Microsecond)
	utils.ManagedGo(func() {
		for {
			if cancelCtx.Err() != nil {
				return
			}
			select {
			case <-cancelCtx.Done():
				return
			case <-ticker.C:
			}
			now := time.Now()
			queue.mu.Lock()
			for hostName, hostQueue := range queue.hostQueues {
				hostQueue.mu.Lock()
				for offerID, offer := range hostQueue.activeOffers {
					if d, ok := offer.offer.answererDoneCtx.Deadline(); ok && d.Before(now) {
						delete(hostQueue.activeOffers, offerID)
					}
				}
				if len(hostQueue.activeOffers) == 0 && hostQueue.expires.Before(now) {
					delete(queue.hostQueues, hostName)
				}
				hostQueue.mu.Unlock()
			}
			queue.mu.Unlock()
		}
	}, func() {
		defer queue.activeBackgroundWorkers.Done()
		defer ticker.Stop()
	})
	return queue
}

// memoryWebRTCCallOfferInit is the offer to start a call where information about the caller
// and how it wishes to speak is contained in the SDP.
type memoryWebRTCCallOfferInit struct {
	uuid               string
	sdp                string
	disableTrickle     bool
	callerCandidates   chan webrtc.ICECandidateInit
	answererResponses  chan<- WebRTCCallAnswer
	answererDoneCtx    context.Context
	answererDoneCancel func()
}

// SendOfferInit initializes an offer associated with the given SDP to the given host.
// It returns a UUID to track/authenticate the offer over time, the initial SDP for the
// sender to start its peer connection with, as well as a channel to receive candidates on
// over time.
func (queue *memoryWebRTCCallQueue) SendOfferInit(
	ctx context.Context,
	host, sdp string,
	disableTrickle bool,
) (string, <-chan WebRTCCallAnswer, <-chan struct{}, func(), error) {
	hostQueueForSend := queue.getOrMakeHostQueue(host)

	var newUUID string
	if queue.uuidDeterministic {
		newUUID = fmt.Sprintf("insecure-uuid-%d", atomic.AddInt64(&queue.uuidDeterministicCounter, 1))
	} else {
		newUUID = uuid.NewString()
	}
	answererResponses := make(chan WebRTCCallAnswer)
	sendCtx, sendCtxCancel := context.WithTimeout(queue.cancelCtx, getDefaultOfferDeadline())
	offer := memoryWebRTCCallOfferInit{
		uuid:               newUUID,
		sdp:                sdp,
		disableTrickle:     disableTrickle,
		callerCandidates:   make(chan webrtc.ICECandidateInit),
		answererResponses:  answererResponses,
		answererDoneCtx:    sendCtx,
		answererDoneCancel: sendCtxCancel,
	}

	callerDoneCtx, callerDoneCancel := context.WithCancel(context.Background())
	hostQueueForSend.mu.Lock()
	exchange := &memoryWebRTCCallOfferExchange{
		offer:            offer,
		callerDoneCtx:    callerDoneCtx,
		callerDoneCancel: callerDoneCancel,
	}
	hostQueueForSend.activeOffers[offer.uuid] = exchange
	hostQueueForSend.mu.Unlock()

	queue.activeBackgroundWorkers.Add(1)
	utils.PanicCapturingGo(func() {
		queue.activeBackgroundWorkers.Done()
		select {
		case <-sendCtx.Done():
		case <-ctx.Done():
		case hostQueueForSend.exchangeCh <- exchange:
		}
	})
	return newUUID, answererResponses, sendCtx.Done(), func() { sendCtxCancel() }, nil
}

// SendOfferUpdate updates the offer associated with the given UUID with a newly discovered
// ICE candidate.
func (queue *memoryWebRTCCallQueue) SendOfferUpdate(ctx context.Context, host, uuid string, candidate webrtc.ICECandidateInit) error {
	hostQueue := queue.getOrMakeHostQueue(host)

	hostQueue.mu.RLock()
	offer, ok := hostQueue.activeOffers[uuid]
	if !ok {
		defer hostQueue.mu.RUnlock()
		return newInactiveOfferErr(uuid)
	}
	hostQueue.mu.RUnlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case offer.offer.callerCandidates <- candidate:
		return nil
	}
}

// SendOfferDone informs the queue that the offer associated with the UUID is done sending any
// more information.
func (queue *memoryWebRTCCallQueue) SendOfferDone(ctx context.Context, host, uuid string) error {
	hostQueue := queue.getOrMakeHostQueue(host)

	hostQueue.mu.Lock()
	offer, ok := hostQueue.activeOffers[uuid]
	if !ok {
		defer hostQueue.mu.Unlock()
		return newInactiveOfferErr(uuid)
	}
	offer.callerDoneCancel()
	delete(hostQueue.activeOffers, uuid)
	hostQueue.mu.Unlock()
	return nil
}

// SendOfferError informs the queue that the offer associated with the UUID has encountered
// an error from the sender side.
func (queue *memoryWebRTCCallQueue) SendOfferError(ctx context.Context, host, uuid string, err error) error {
	hostQueue := queue.getOrMakeHostQueue(host)

	hostQueue.mu.Lock()
	offer, ok := hostQueue.activeOffers[uuid]
	if !ok {
		hostQueue.mu.Unlock()
		return newInactiveOfferErr(uuid)
	}
	offer.callerErr = err
	offer.callerDoneCancel()
	delete(hostQueue.activeOffers, uuid)
	hostQueue.mu.Unlock()
	return nil
}

// RecvOffer receives the next offer for the given host. It should respond with an answer
// once a decision is made.
func (queue *memoryWebRTCCallQueue) RecvOffer(ctx context.Context, host string) (WebRTCCallOfferExchange, error) {
	hostQueue := queue.getOrMakeHostQueue(host)

	recvCtx, recvCtxCancel := context.WithCancel(queue.cancelCtx)
	defer recvCtxCancel()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-recvCtx.Done():
		return nil, recvCtx.Err()
	case exchange := <-hostQueue.exchangeCh:
		return exchange, nil
	}
}

// Close cancels all actives offers and waits to cleanly close all background workers.
func (queue *memoryWebRTCCallQueue) Close() error {
	queue.cancelFunc()
	queue.activeBackgroundWorkers.Wait()
	return nil
}

type memoryWebRTCCallOfferExchange struct {
	offer            memoryWebRTCCallOfferInit
	callerDoneCtx    context.Context
	callerDoneCancel func()
	callerErr        error
	answererDoneOnce sync.Once
}

func (resp *memoryWebRTCCallOfferExchange) UUID() string {
	return resp.offer.uuid
}

func (resp *memoryWebRTCCallOfferExchange) SDP() string {
	return resp.offer.sdp
}

func (resp *memoryWebRTCCallOfferExchange) DisableTrickleICE() bool {
	return resp.offer.disableTrickle
}

func (resp *memoryWebRTCCallOfferExchange) CallerCandidates() <-chan webrtc.ICECandidateInit {
	return resp.offer.callerCandidates
}

func (resp *memoryWebRTCCallOfferExchange) CallerDone() <-chan struct{} {
	return resp.callerDoneCtx.Done()
}

func (resp *memoryWebRTCCallOfferExchange) CallerErr() error {
	if resp.callerDoneCtx.Err() == nil {
		return nil
	}
	if resp.callerErr != nil {
		return resp.callerErr
	}
	if errors.Is(resp.callerDoneCtx.Err(), context.Canceled) {
		return nil
	}
	return resp.callerDoneCtx.Err()
}

func (resp *memoryWebRTCCallOfferExchange) AnswererRespond(ctx context.Context, ans WebRTCCallAnswer) error {
	select {
	case resp.offer.answererResponses <- ans:
		return nil
	case <-resp.offer.answererDoneCtx.Done():
		return resp.offer.answererDoneCtx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (resp *memoryWebRTCCallOfferExchange) AnswererDone(ctx context.Context) error {
	resp.answererDoneOnce.Do(func() {
		resp.offer.answererDoneCancel()
	})
	return nil
}

type singleWebRTCHostQueue struct {
	mu           sync.RWMutex
	exchangeCh   chan *memoryWebRTCCallOfferExchange
	activeOffers map[string]*memoryWebRTCCallOfferExchange
	expires      time.Time
}

func (queue *memoryWebRTCCallQueue) getOrMakeHostQueue(host string) *singleWebRTCHostQueue {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	hostQueue, ok := queue.hostQueues[host]
	if ok {
		hostQueue.expires = time.Now().Add(getDefaultOfferDeadline())
		return hostQueue
	}
	hostQueue = &singleWebRTCHostQueue{
		exchangeCh:   make(chan *memoryWebRTCCallOfferExchange),
		activeOffers: make(map[string]*memoryWebRTCCallOfferExchange),
		expires:      time.Now().Add(getDefaultOfferDeadline()),
	}
	queue.hostQueues[host] = hostQueue
	return hostQueue
}
