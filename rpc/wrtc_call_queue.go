package rpc

import (
	"context"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
)

// do not change this unless the MongoDB TTL is also modified in advance.
var (
	_defaultOfferDeadlineMu sync.Mutex
	_defaultOfferDeadline   = time.Minute
)

func getDefaultOfferDeadline() time.Duration {
	_defaultOfferDeadlineMu.Lock()
	defer _defaultOfferDeadlineMu.Unlock()
	return _defaultOfferDeadline
}

func setDefaultOfferDeadline(deafultOfferDeadline time.Duration) func() {
	_defaultOfferDeadlineMu.Lock()
	prevDefaultOfferDeadline := _defaultOfferDeadline
	_defaultOfferDeadline = deafultOfferDeadline
	_defaultOfferDeadlineMu.Unlock()
	return func() {
		setDefaultOfferDeadline(prevDefaultOfferDeadline)
	}
}

// A WebRTCCallQueue handles the transmission and reception of call offers. For every
// sending of an offer done, it is expected that there is someone to receive that
// offer and subsequently respond to it.
type WebRTCCallQueue interface {
	// SendOfferInit initializes an offer associated with the given SDP to the given host.
	// It returns a UUID to track/authenticate the offer over time, a channel receive offer updates
	// on over time, and a cancel func to stop inform the sender to stop.
	SendOfferInit(ctx context.Context, host, sdp string, disableTrickle bool) (
		uuid string, respCh <-chan WebRTCCallAnswer, respDone <-chan struct{}, cancel func(), err error)

	// SendOfferUpdate updates the offer associated with the given UUID with a newly discovered
	// ICE candidate.
	SendOfferUpdate(ctx context.Context, host, uuid string, candidate webrtc.ICECandidateInit) error

	// SendOfferDone informs the queue that the offer associated with the UUID is done sending any
	// more information.
	SendOfferDone(ctx context.Context, host, uuid string) error

	// SendOfferError informs the queue that the offer associated with the UUID has encountered
	// an error from the sender side.
	SendOfferError(ctx context.Context, host, uuid string, err error) error

	// RecvOffer receives the next offer for the given host. It should respond with an answer
	// once a decision is made.
	RecvOffer(ctx context.Context, host string) (WebRTCCallOfferExchange, error)

	// Close shuts down the queue.
	Close() error
}

// WebRTCCallOffer contains the information needed to offer to start a call.
type WebRTCCallOffer interface {
	// The UUID uniquely identifies this offer.
	UUID() string

	// The SDP contains initial information the caller wants to tell the answerer about.
	// In this case of Trickle ICE being disabled, this is an SDP with all ICE
	// candidate info gathered.
	SDP() string

	// DisableTrickleICE indicates if Trickle ICE should be used. Currently, both
	// sides must both respect this setting.
	DisableTrickleICE() bool
}

// A WebRTCCallOfferExchange is used by an answerer to respond to a call offer with an
// answer.
type WebRTCCallOfferExchange interface {
	WebRTCCallOffer

	// CallerCandidates transmits all candidates the caller discovers.
	CallerCandidates() <-chan webrtc.ICECandidateInit

	// CallerDone indicates when the caller has no more information to offer.
	CallerDone() <-chan struct{}

	// CallerErr returns any error that happened on the caller side. This should only
	// be called after CallerDone is done.
	CallerErr() error

	// Respond responds to the associated call offer with the given answer which contains
	// the SDP of the answerer or an error.
	AnswererRespond(ctx context.Context, ans WebRTCCallAnswer) error

	// AnswererDone signals that there is no more information to expect from the answerer.
	AnswererDone(ctx context.Context) error
}

// WebRTCCallAnswer is the response to an offer. An agreement to start the call
// will contain an SDP about how the answerer wishes to speak.
type WebRTCCallAnswer struct {
	InitialSDP *string
	Candidate  *webrtc.ICECandidateInit
	Err        error
}

const noActiveOfferStr = "no active offer"

func newInactiveOfferErr(uuid string) error {
	return errors.Errorf("%s for %q", noActiveOfferStr, uuid)
}
