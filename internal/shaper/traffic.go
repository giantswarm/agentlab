package shaper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// The proof's traffic, standing in for iperf3: a bulk source in the fake
// internet, and fetchers (the mirror's four streams, the person's one) that
// report what they received every second. agentlab runs both itself
// (`agentlab shaper-test --role serve|fetch`), so the proof needs no tool
// beyond iproute2.

// trafficBuffer is the size of one write and one read.
const trafficBuffer = 64 << 10

// ServeBulk accepts connections on listen and writes to each until it goes
// away or ctx ends.
func ServeBulk(ctx context.Context, listen string) error {
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = l.Close() }()
	buf := make([]byte, trafficBuffer)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer func() { _ = c.Close() }()
			for {
				if _, err := c.Write(buf); err != nil {
					return
				}
			}
		}()
	}
}

// Sample is one second of a fetch: the second since the proof's start and
// the bits per second received in it.
type Sample struct {
	T   int     `json:"t"`
	Bps float64 `json:"bps"`
}

// FetchBulk opens streams connections to from at start, reads from them
// for the given duration and writes one Sample per second as a JSON line.
func FetchBulk(ctx context.Context, w io.Writer, from string, streams int, start time.Time, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(time.Until(start)):
	}
	ctx, cancel := context.WithDeadline(ctx, start.Add(duration))
	defer cancel()
	var received atomic.Int64
	var wg sync.WaitGroup
	for range streams {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", from)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", from, err)
		}
		wg.Go(func() {
			defer func() { _ = c.Close() }()
			go func() { <-ctx.Done(); _ = c.Close() }()
			buf := make([]byte, trafficBuffer)
			for {
				n, err := c.Read(buf)
				received.Add(int64(n))
				if err != nil {
					return
				}
			}
		})
	}
	enc := json.NewEncoder(w)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last, lastAt := int64(0), time.Now()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case now := <-t.C:
			got := received.Load()
			_ = enc.Encode(Sample{
				T:   int(now.Sub(start).Round(time.Second) / time.Second),
				Bps: float64(got-last) * 8 / now.Sub(lastAt).Seconds(),
			})
			last, lastAt = got, now
		}
	}
}
