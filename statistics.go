package main

import (
	"fmt"
	"math"
	"sync"
	"time"
)

type requestState struct {
	sentTime time.Time
	received bool
	errored  bool
	timedout bool
}

type PingStatistics struct {
	lock          sync.Mutex
	requests      map[uint16]*requestState
	allRTTs       []float64 // milliseconds
	numSent       int
	numErrors     int
	numFailedReqs int
	numDups       int
	respTimeout   time.Duration

	// for --count option:
	doneChan           chan struct{}
	responsesRemaining int
}

func NewPingStatistics(initialResponseTimeout time.Duration) *PingStatistics {
	return &PingStatistics{
		requests:    make(map[uint16]*requestState),
		allRTTs:     make([]float64, 0, 100),
		respTimeout: initialResponseTimeout,
	}
}

// chan struct{} will be closed after count responses or timeouts.
func NewPingStatisticsWithDoneChan(initialResponseTimeout time.Duration,
	count int) (*PingStatistics, <-chan struct{}) {

	ps := NewPingStatistics(initialResponseTimeout)
	ps.doneChan = make(chan struct{})
	ps.responsesRemaining = count
	return ps, ps.doneChan
}

// Called to report a request being sent. Current time is recorded with sequence
// number to calculate RTT later.
func (p *PingStatistics) addRequest(seq uint16) {
	p.lock.Lock()
	p.requests[seq] = &requestState{sentTime: time.Now()}
	p.lock.Unlock()
}

// Called to report an Echo response being received. RTT (in milliseconds) is
// returned.
func (p *PingStatistics) rttMilliseconds(seq uint16) (float64, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	reqState, ok := p.requests[seq]
	if !ok {
		return 0, fmt.Errorf("Received response packet with unexpected sequence number %d.", seq)
	}

	rtt := time.Since(reqState.sentTime)
	rttMs := float64(rtt.Microseconds()) / 1000

	// Update response timeout if this is the new max RTT
	if 2*rtt > p.respTimeout {
		p.respTimeout = 2 * rtt
	}

	// Only record in allRTTs (for RTT statistics) if this is the first response
	// for this sequence number. (For multicast support.)
	if !reqState.received {
		reqState.received = true
		p.allRTTs = append(p.allRTTs, rttMs)

		if !reqState.errored && !reqState.timedout {
			p.responsesRemaining--
			if p.doneChan != nil && p.responsesRemaining == 0 {
				close(p.doneChan)
			}
		}
	} else {
		p.numDups++
	}

	return rttMs, nil
}

// Called to report that a Time Exceeded message was received for the Echo
// request with this sequence number.
func (p *PingStatistics) requestGotTimeExceeded(seq uint16) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	reqState, ok := p.requests[seq]
	if !ok {
		return fmt.Errorf("Received time exceeded packet with unexpected sequence number %d.", seq)
	}

	p.numErrors++
	if !reqState.errored {
		reqState.errored = true

		if !reqState.received && !reqState.timedout {
			p.responsesRemaining--
			if p.doneChan != nil && p.responsesRemaining == 0 {
				close(p.doneChan)
			}
		}
	}
	return nil
}

// Returns true if no response has been received for this sequence number.
// Called to report that timeout window has expired.
func (p *PingStatistics) isStillWaitingForResp(seq uint16) (reqTimedout bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	reqState, ok := p.requests[seq]
	reqTimedout = ok && !reqState.received && !reqState.errored
	if reqTimedout {
		reqState.timedout = true

		p.responsesRemaining--
		if p.doneChan != nil && p.responsesRemaining == 0 {
			close(p.doneChan)
		}
	}
	return
}

// Called to report that an Echo request failed to send.
func (p *PingStatistics) requestFailed() {
	p.lock.Lock()
	p.numErrors++
	p.numFailedReqs++
	p.lock.Unlock()
}

// Returns the duration to wait for a response before announcing that the
// request timedout. The duration is 2 times the maximum RTT unless no response
// has been received in which case the duration is `initialResponseTimeout` from
// NewPingStatistics()
func (p *PingStatistics) responseTimeout() time.Duration {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.respTimeout
}

func (p *PingStatistics) printStatistics() {
	extraCount := func(name string, value int) string {
		if value == 0 {
			return ""
		}
		return fmt.Sprintf("+%d %s, ", value, name)
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	var minRTT, avgRTT, maxRTT, stddevRTT float64
	numRecv := len(p.allRTTs)
	numSent := len(p.requests) + p.numFailedReqs

	// Calculate min, max, avg
	for i, rtt := range p.allRTTs {
		if i == 0 {
			minRTT, maxRTT = rtt, rtt
		} else {
			if rtt < minRTT {
				minRTT = rtt
			}
			if rtt > maxRTT {
				maxRTT = rtt
			}
		}

		avgRTT += rtt
	}
	avgRTT /= float64(numRecv)

	// Caclulate stddev
	for _, rtt := range p.allRTTs {
		stddevRTT += math.Pow(rtt-avgRTT, 2)
	}
	stddevRTT = math.Sqrt(stddevRTT / float64(numRecv))

	var packetLoss float64 = 0
	if numSent != 0 {
		packetLoss = 100 * (1 - float64(numRecv)/float64(numSent))
	}

	// Print out
	fmt.Printf("%v packets transmitted, %v packets received, %s%s%.1f%% packet loss\n",
		numSent, numRecv, extraCount("errors", p.numErrors),
		extraCount("duplicates", p.numDups), packetLoss)

	if numRecv > 0 {
		fmt.Printf("round-trip min/avg/max/stddev = %.3f/%.3f/%.3f/%.3f ms\n",
			minRTT, avgRTT, maxRTT, stddevRTT)
	}
}
