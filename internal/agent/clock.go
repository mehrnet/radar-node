package agent

import (
	"sync"
	"time"
)

// clockSync tracks this node's offset from the server's clock,
// measured against server_time on every heartbeat response (see
// heartbeatLoop in agent.go). All due-ness decisions use corrected
// ("server-equivalent") time, not the node's own possibly-drifted
// local clock -- if the server's clock reads an hour behind this
// node's, this node's scheduling should behave exactly as if its own
// clock also read an hour behind, not fight the server about it.
type clockSync struct {
	mu     sync.Mutex
	offset time.Duration
}

// update records a fresh offset measurement. sentAt/receivedAt bound
// the request's local round trip; the server's clock is assumed to
// have been read at roughly the midpoint of that window (a standard
// NTP-style correction) rather than naively diffing against either
// endpoint, which would bake in the request's one-way latency as
// clock error.
func (s *clockSync) update(serverTime, sentAt, receivedAt time.Time) {
	midpoint := sentAt.Add(receivedAt.Sub(sentAt) / 2)
	s.mu.Lock()
	s.offset = serverTime.Sub(midpoint)
	s.mu.Unlock()
}

// now returns this node's best estimate of the server's current time.
func (s *clockSync) now() time.Time {
	s.mu.Lock()
	offset := s.offset
	s.mu.Unlock()
	return time.Now().Add(offset)
}
