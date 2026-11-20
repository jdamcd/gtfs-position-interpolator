package feed

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jdamcd/gtfs-position-interpolator/internal/fetch"
)

type Feed struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type Handler func(feed string, msg *gtfs.FeedMessage, fetched time.Time)

// A Poller fetches the feeds only while they are in demand.
type Poller struct {
	client   *http.Client
	feeds    []Feed
	interval time.Duration
	idle     time.Duration
	handle   Handler
	reset    func()

	mu     sync.Mutex
	asked  time.Time
	active bool
	ready  chan struct{}
	wake   chan struct{}
}

// reset is called when polling stops: what the handler last saw will be stale
// by the restart.
func NewPoller(client *http.Client, feeds []Feed, interval, idle time.Duration, handle Handler, reset func()) *Poller {
	return &Poller{
		client:   client,
		feeds:    feeds,
		interval: interval,
		idle:     idle,
		handle:   handle,
		reset:    reset,
		wake:     make(chan struct{}, 1),
	}
}

// Demand starts polling if it had stopped. The channel closes once every
// feed has been tried.
func (p *Poller) Demand() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = time.Now()
	if !p.active {
		p.active = true
		p.ready = make(chan struct{})
		p.wake <- struct{}{}
	}
	return p.ready
}

func (p *Poller) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

func (p *Poller) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
		p.pollWhileInDemand(ctx)
	}
}

func (p *Poller) pollWhileInDemand(ctx context.Context) {
	p.mu.Lock()
	ready := p.ready
	p.mu.Unlock()

	slog.Info("polling feeds", "interval", p.interval)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var polls, first sync.WaitGroup
	for _, f := range p.feeds {
		first.Add(1)
		polls.Go(func() { p.poll(ctx, f, sync.OnceFunc(first.Done)) })
	}
	go func() {
		first.Wait()
		close(ready)
	}()

	p.waitForIdle(ctx)
	cancel()
	polls.Wait()
	p.reset()
	slog.Info("polling stopped", "idle", p.idle)
}

func (p *Poller) waitForIdle(ctx context.Context) {
	for {
		p.mu.Lock()
		left := p.idle - time.Since(p.asked)
		if left <= 0 {
			p.active = false
		}
		p.mu.Unlock()
		if left <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(left):
		}
	}
}

func (p *Poller) poll(ctx context.Context, f Feed, tried func()) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	var last uint64
	for {
		msg, err := get(ctx, p.client, f)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("feed fetch failed", "feed", f.Name, "err", err)
			}
		// Without a header timestamp there is no telling if a feed changed.
		case msg.GetHeader().GetTimestamp() != last || last == 0:
			last = msg.GetHeader().GetTimestamp()
			p.handle(f.Name, msg, time.Now())
		}
		tried()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func get(ctx context.Context, client *http.Client, f Feed) (*gtfs.FeedMessage, error) {
	resp, err := fetch.Get(ctx, client, f.URL, f.Headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	msg := &gtfs.FeedMessage{}
	if err := proto.Unmarshal(body, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
