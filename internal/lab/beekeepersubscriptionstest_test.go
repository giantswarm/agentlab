package lab

import (
	"io"
	"strings"
	"testing"
)

func TestSubscriptionRead(t *testing.T) {
	stream := strings.Join([]string{
		`event: message`,
		`data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":7}}}`,
		``,
		`data: {"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"beekeeper://mailbox/a@lab.local","_meta":{"io.modelcontextprotocol/subscriptionId":7}}}`,
		``,
		`data: {"jsonrpc":"2.0","id":7,"error":{"code":-32600,"message":"no"}}`,
		``,
	}, "\n")
	s := &subscription{id: 7, cancel: func() {}, events: make(chan subscriptionEvent, 8)}
	s.read(io.NopCloser(strings.NewReader(stream)))

	ev, err := s.next()
	if err != nil || ev.method != notifAcknowledged {
		t.Fatalf("first event %+v, %v", ev, err)
	}
	if err := s.expectUpdate("beekeeper://mailbox/a@lab.local"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.next(); err == nil || !strings.Contains(err.Error(), "refused: no") {
		t.Fatalf("an error response is a refusal, got %v", err)
	}
	if _, err := s.next(); err == nil {
		t.Fatal("a closed stream is an error")
	}
}

func TestSubscriptionExpectUpdate_WrongURIOrID(t *testing.T) {
	s := &subscription{id: 1, cancel: func() {}, events: make(chan subscriptionEvent, 2)}
	s.events <- subscriptionEvent{method: notifResourceUpdated, uri: "beekeeper://mailbox/b@lab.local", subID: float64(1)}
	if err := s.expectUpdate("beekeeper://mailbox/a@lab.local"); err == nil {
		t.Fatal("another person's update must fail the proof")
	}
	s.events <- subscriptionEvent{method: notifResourceUpdated, uri: "beekeeper://mailbox/a@lab.local", subID: float64(2)}
	if err := s.expectUpdate("beekeeper://mailbox/a@lab.local"); err == nil {
		t.Fatal("another subscription's ID must fail the proof")
	}
}
