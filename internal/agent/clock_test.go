package agent

import (
	"testing"
	"time"
)

func TestClockSync_ZeroOffsetByDefault(t *testing.T) {
	var c clockSync
	before := time.Now()
	now := c.now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("expected now() to track the local clock before any update, got %v not within [%v, %v]", now, before, after)
	}
}

func TestClockSync_ServerAheadOfNode(t *testing.T) {
	var c clockSync
	sentAt := time.Now()
	receivedAt := sentAt.Add(100 * time.Millisecond)
	serverTime := sentAt.Add(time.Hour + 50*time.Millisecond) // server read at the round-trip midpoint, offset by 1h

	c.update(serverTime, sentAt, receivedAt)

	got := c.now()
	want := time.Now().Add(time.Hour)
	if diff := got.Sub(want); diff < -50*time.Millisecond || diff > 50*time.Millisecond {
		t.Fatalf("expected now() to read ~1h ahead of the local clock, got diff %v", diff)
	}
}

func TestClockSync_ServerBehindNode(t *testing.T) {
	var c clockSync
	sentAt := time.Now()
	receivedAt := sentAt.Add(100 * time.Millisecond)
	serverTime := sentAt.Add(-time.Hour + 50*time.Millisecond)

	c.update(serverTime, sentAt, receivedAt)

	got := c.now()
	want := time.Now().Add(-time.Hour)
	if diff := got.Sub(want); diff < -50*time.Millisecond || diff > 50*time.Millisecond {
		t.Fatalf("expected now() to read ~1h behind the local clock, got diff %v", diff)
	}
}

func TestClockSync_LaterUpdateOverridesEarlier(t *testing.T) {
	var c clockSync
	sentAt := time.Now()
	receivedAt := sentAt

	c.update(sentAt.Add(time.Hour), sentAt, receivedAt)
	c.update(sentAt.Add(2*time.Hour), sentAt, receivedAt)

	got := c.now()
	want := time.Now().Add(2 * time.Hour)
	if diff := got.Sub(want); diff < -50*time.Millisecond || diff > 50*time.Millisecond {
		t.Fatalf("expected the most recent update to win, got diff %v", diff)
	}
}
