package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rangka-os/rangka-edge/internal/edgesync"
	"github.com/rangka-os/rangka-edge/internal/queue"
)

// Pusher sends events to the kernel; edgesync.Client implements it.
type Pusher interface {
	Push(ctx context.Context, events []edgesync.Event) ([]edgesync.Result, error)
}

// Report counts what one Flush did. Pushed counts accepted events.
type Report struct{ Pushed, Duplicates, Rejected int }

// Runner drains Queue to the kernel through Pusher.
type Runner struct {
	Queue     *queue.Queue
	Pusher    Pusher
	BatchSize int // 0: 100
	Backoff   Backoff
	Idle      time.Duration // poll interval when the queue is empty; 0: 5 s
	// Sleep waits for d or until ctx is done (then it returns the ctx error). nil: a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	Logf  func(format string, args ...any)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Flush pushes pending items in batches until the queue is empty. The first push error is returned as
// is and the batch stays pending (keys are idempotent, so sending it again is safe).
func (r *Runner) Flush(ctx context.Context) (Report, error) {
	var rep Report
	size := r.BatchSize
	if size <= 0 {
		size = 100
	}
	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		items := r.Queue.Pending(size)
		if len(items) == 0 {
			return rep, nil
		}
		events := make([]edgesync.Event, len(items))
		keys := make(map[string]bool, len(items))
		for i, it := range items {
			events[i] = edgesync.Event{Key: it.Key, Kind: it.Kind, Seq: it.Seq, OccurredAt: it.OccurredAt, Actor: it.Actor, Payload: it.Payload}
			keys[it.Key] = true
		}
		results, err := r.Pusher.Push(ctx, events)
		if err != nil {
			return rep, err
		}
		for _, res := range results {
			if !keys[res.Key] {
				return rep, fmt.Errorf("runner: kernel answered for unknown key %q", res.Key)
			}
			switch res.Status {
			case edgesync.StatusAccepted:
				err = r.Queue.Ack(res.Key, res.Ref)
				rep.Pushed++
			case edgesync.StatusDuplicate:
				err = r.Queue.Ack(res.Key, res.Ref)
				rep.Duplicates++
			case edgesync.StatusRejected:
				err = r.Queue.Dead(res.Key, res.Code, res.Message)
				rep.Rejected++
				r.logf("event %s rejected: %s: %s", res.Key, res.Code, res.Message)
			default:
				err = fmt.Errorf("runner: unknown result status %q for key %q", res.Status, res.Key)
			}
			if err != nil {
				return rep, err
			}
		}
	}
}

// Run flushes in a loop until ctx is done (returns ctx.Err()), the device is revoked (ErrRevoked) or a
// push fails in a way that retrying will not fix (returned as is). Retryable errors back off.
func (r *Runner) Run(ctx context.Context) error {
	idle := r.Idle
	if idle <= 0 {
		idle = 5 * time.Second
	}
	attempt := 0
	for {
		rep, err := r.Flush(ctx)
		var wait time.Duration
		switch {
		case err == nil:
			if rep != (Report{}) {
				r.logf("sync: %d pushed, %d duplicate, %d rejected", rep.Pushed, rep.Duplicates, rep.Rejected)
			}
			attempt, wait = 0, idle
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, edgesync.ErrRevoked):
			r.logf("sync stopped: device token revoked; the queue is kept")
			return err
		case edgesync.Retryable(err):
			var he *edgesync.HTTPError
			var ra time.Duration
			if errors.As(err, &he) {
				ra = he.RetryAfter
			}
			wait = r.Backoff.Delay(attempt, ra)
			attempt++
			r.logf("sync failed, retrying in %s: %v", wait, err)
		default:
			return err
		}
		if err := r.sleep(ctx, wait); err != nil {
			return err
		}
	}
}
