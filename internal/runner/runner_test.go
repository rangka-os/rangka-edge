package runner_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rangka-os/rangka-edge/internal/edgesync"
	"github.com/rangka-os/rangka-edge/internal/edgesync/kernelstub"
	"github.com/rangka-os/rangka-edge/internal/queue"
	"github.com/rangka-os/rangka-edge/internal/runner"
)

type env struct {
	q    *queue.Queue
	stub *kernelstub.Stub
	cl   *edgesync.Client
	id   string
}

func setup(t *testing.T) *env {
	t.Helper()
	q, err := queue.Open(t.TempDir(), queue.Options{DeviceID: "dev1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	stub := kernelstub.New()
	stub.AddPairingCode("CODE1234", "OUT-01")
	srv := httptest.NewServer(stub.Handler())
	t.Cleanup(srv.Close)
	cl := &edgesync.Client{BaseURL: srv.URL}
	rr, err := cl.Register(context.Background(), "CODE1234", edgesync.DeviceInfo{Name: "k"})
	if err != nil {
		t.Fatal(err)
	}
	cl.Token = rr.Token
	return &env{q: q, stub: stub, cl: cl, id: rr.DeviceID}
}

func (e *env) enqueue(t *testing.T, kinds ...string) {
	t.Helper()
	for _, k := range kinds {
		if _, _, err := e.q.Enqueue(queue.NewItem{Kind: k, OccurredAt: time.Now(), Payload: []byte(`{"x":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFlushDrainsInBatches(t *testing.T) {
	e := setup(t)
	for i := 0; i < 250; i++ {
		e.enqueue(t, "sale.recorded")
	}
	r := &runner.Runner{Queue: e.q, Pusher: e.cl, BatchSize: 100}
	rep, err := r.Flush(context.Background())
	if err != nil || rep.Pushed != 250 {
		t.Fatalf("%+v %v", rep, err)
	}
	if e.stub.PushCalls() != 3 {
		t.Errorf("push calls = %d", e.stub.PushCalls())
	}
	if st := e.q.Stats(); st.Done != 250 || st.Pending != 0 {
		t.Errorf("%+v", st)
	}
}

func TestFlushAfterLostResponse(t *testing.T) {
	e := setup(t)
	e.enqueue(t, "sale.recorded", "sale.recorded", "sale.voided")
	e.stub.DropResponseNext(1)
	r := &runner.Runner{Queue: e.q, Pusher: e.cl}
	if _, err := r.Flush(context.Background()); err == nil || !edgesync.Retryable(err) {
		t.Fatalf("want retryable error, got %v", err)
	}
	if e.q.Stats().Pending != 3 {
		t.Fatal("batch must stay pending")
	}
	rep, err := r.Flush(context.Background())
	if err != nil || rep.Duplicates != 3 {
		t.Fatalf("%+v %v", rep, err)
	}
	if n := len(e.stub.Events()); n != 3 {
		t.Errorf("kernel stored %d events", n)
	}
	if st := e.q.Stats(); st.Done != 3 || st.Pending != 0 {
		t.Errorf("%+v", st)
	}
}

func TestFlushRejectedGoesDead(t *testing.T) {
	e := setup(t)
	e.enqueue(t, "sale.recorded", "sale.voided", "foo")
	r := &runner.Runner{Queue: e.q, Pusher: e.cl}
	rep, err := r.Flush(context.Background())
	if err != nil || rep.Pushed != 2 || rep.Rejected != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	st := e.q.Stats()
	if st.Pending != 0 || st.Dead != 1 {
		t.Fatalf("%+v", st)
	}
	if d := e.q.DeadItems(); len(d) != 1 || d[0].Code != "unknown_kind" {
		t.Errorf("%+v", d)
	}
	if e.stub.PushCalls() != 1 {
		t.Errorf("rejected must not be retried: %d calls", e.stub.PushCalls())
	}
}

type fakePusher struct{ results []edgesync.Result }

func (f fakePusher) Push(context.Context, []edgesync.Event) ([]edgesync.Result, error) {
	return f.results, nil
}

func TestFlushUnknownKeyIsError(t *testing.T) {
	e := setup(t)
	e.enqueue(t, "sale.recorded")
	r := &runner.Runner{Queue: e.q, Pusher: fakePusher{[]edgesync.Result{{Key: "other", Status: edgesync.StatusAccepted}}}}
	if _, err := r.Flush(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if e.q.Stats().Pending != 1 {
		t.Error("item must stay pending")
	}
}

func TestFlushMissingResultsIsUnknownOutcome(t *testing.T) {
	e := setup(t)
	e.enqueue(t, "sale.recorded", "sale.voided")
	r := &runner.Runner{Queue: e.q, Pusher: fakePusher{}}
	if _, err := r.Flush(context.Background()); !errors.Is(err, edgesync.ErrUnknownOutcome) {
		t.Fatalf("got %v", err)
	}
	if e.q.Stats().Pending != 2 {
		t.Error("items must stay pending")
	}
}

func TestRunBacksOffThenRecovers(t *testing.T) {
	e := setup(t)
	e.enqueue(t, "sale.recorded")
	e.stub.FailNext(3, 503)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	r := &runner.Runner{
		Queue: e.q, Pusher: e.cl, Idle: 7 * time.Second,
		Backoff: runner.Backoff{Jitter: -1},
		Sleep: func(_ context.Context, d time.Duration) error {
			delays = append(delays, d)
			if len(delays) == 4 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	err := r.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 7 * time.Second}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Errorf("delays %v, want %v", delays, want)
	}
	if e.q.Stats().Done != 1 {
		t.Error("item not delivered")
	}
}

func TestRunStopsOnRevoked(t *testing.T) {
	e := setup(t)
	e.enqueue(t, "sale.recorded")
	e.stub.Revoke(e.id)
	r := &runner.Runner{Queue: e.q, Pusher: e.cl, Sleep: func(context.Context, time.Duration) error { t.Fatal("must not sleep"); return nil }}
	if err := r.Run(context.Background()); !errors.Is(err, edgesync.ErrRevoked) {
		t.Fatalf("got %v", err)
	}
	if e.q.Stats().Pending != 1 {
		t.Error("queue must be kept")
	}
}

func TestRunReturnsNonRetryable(t *testing.T) {
	e := setup(t)
	e.enqueue(t, "sale.recorded")
	e.stub.FailNext(1, 400)
	r := &runner.Runner{Queue: e.q, Pusher: e.cl, Sleep: func(context.Context, time.Duration) error { t.Fatal("must not sleep"); return nil }}
	var he *edgesync.HTTPError
	if err := r.Run(context.Background()); !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("got %v", err)
	}
	if st := e.q.Stats(); st.Pending != 1 || st.Dead != 0 {
		t.Errorf("%+v", st)
	}
}

func TestBackoffDelay(t *testing.T) {
	b := runner.Backoff{Jitter: -1}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300}
	for i, w := range want {
		if got := b.Delay(i, 0); got != w*time.Second {
			t.Errorf("attempt %d: %v, want %vs", i, got, w)
		}
	}
	if got := b.Delay(0, 30*time.Second); got != 30*time.Second {
		t.Errorf("retry-after: %v", got)
	}
	if got := b.Delay(10, 10*time.Minute); got != 10*time.Minute {
		t.Errorf("retry-after above cap: %v", got)
	}
	if got := b.Delay(0, 2*time.Hour); got != time.Hour {
		t.Errorf("retry-after limit: %v", got)
	}
	if got := (runner.Backoff{Rand: func() float64 { return 0 }}).Delay(0, 30*time.Second); got != 30*time.Second {
		t.Errorf("jitter must not undercut retry-after: %v", got)
	}
	hi := runner.Backoff{Rand: func() float64 { return 1 }}
	if got := hi.Delay(0, 0); got != 1200*time.Millisecond {
		t.Errorf("+20%%: %v", got)
	}
	lo := runner.Backoff{Rand: func() float64 { return 0 }}
	if got := lo.Delay(1, 0); got != 1600*time.Millisecond {
		t.Errorf("-20%%: %v", got)
	}
}
