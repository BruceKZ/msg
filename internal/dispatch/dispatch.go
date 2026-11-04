// Package dispatch delivers queued envelopes to senders, retrying failures with backoff.
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"msg/internal/core"
	"msg/internal/store"
)

var DefaultBackoff = []time.Duration{
	5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour,
}

// Lookup finds an active sender by name.
type Lookup interface {
	Lookup(name string) (core.Sender, bool)
}

type Dispatcher struct {
	store   *store.Store
	senders Lookup
	log     *slog.Logger
	wake    chan struct{}

	Backoff     []time.Duration // retry delays; after the last one the delivery fails
	PollEvery   time.Duration
	SendTimeout time.Duration
}

func New(st *store.Store, senders Lookup, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store: st, senders: senders, log: log, wake: make(chan struct{}, 1),
		Backoff: DefaultBackoff, PollEvery: 5 * time.Second, SendTimeout: 20 * time.Second,
	}
}

// Wake asks the dispatcher to look for due deliveries now.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) Run(ctx context.Context) {
	tick := time.NewTicker(d.PollEvery)
	defer tick.Stop()
	for {
		d.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-tick.C:
		}
	}
}

func (d *Dispatcher) drain(ctx context.Context) {
	for ctx.Err() == nil {
		due, err := d.store.DueDeliveries(ctx, 50)
		if err != nil {
			d.log.Error("load deliveries", "err", err)
			return
		}
		for _, dl := range due {
			d.deliver(ctx, dl)
		}
		if len(due) < 50 {
			return
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, dl store.Due) {
	log := d.log.With("sender", dl.Sender, "message", dl.MessageID)
	snd, ok := d.senders.Lookup(dl.Sender)
	if !ok {
		d.store.MarkFailed(ctx, dl.ID, "sender not active (deleted, disabled or misconfigured)", time.Time{})
		log.Warn("delivery failed: sender not active")
		return
	}
	sctx, cancel := context.WithTimeout(ctx, d.SendTimeout)
	err := snd.Send(sctx, dl.Envelope)
	cancel()
	if err == nil {
		d.store.MarkSent(ctx, dl.ID)
		log.Info("delivered")
		return
	}
	var perm core.Permanent
	if errors.As(err, &perm) || dl.Attempts >= len(d.Backoff) {
		d.store.MarkFailed(ctx, dl.ID, err.Error(), time.Time{})
		log.Error("delivery failed", "attempts", dl.Attempts+1, "err", err)
		return
	}
	d.store.MarkFailed(ctx, dl.ID, err.Error(), time.Now().Add(d.Backoff[dl.Attempts]))
	log.Warn("delivery failed, will retry", "attempt", dl.Attempts+1, "err", err)
}
