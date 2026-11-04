package edgesync_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rangka-os/rangka-edge/internal/edgesync"
	"github.com/rangka-os/rangka-edge/internal/edgesync/kernelstub"
)

func TestClientRegisterAndPush(t *testing.T) {
	stub := kernelstub.New()
	stub.AddPairingCode("K7Q2M9XD", "OUT-02")
	srv := httptest.NewServer(stub.Handler())
	defer srv.Close()

	c := &edgesync.Client{BaseURL: srv.URL}
	rr, err := c.Register(context.Background(), "K7Q2M9XD", edgesync.DeviceInfo{Name: "Kasir 1", Platform: "linux/amd64", Version: "0.1.0"})
	if err != nil || rr.Outlet != "OUT-02" || rr.Token == "" {
		t.Fatalf("%+v %v", rr, err)
	}
	c.Token = rr.Token
	evs := []edgesync.Event{
		{Key: "a", Kind: "sale.recorded", Seq: 1, OccurredAt: time.Now(), Payload: json.RawMessage(`{"x":1}`)},
		{Key: "b", Kind: "nope", Seq: 2, OccurredAt: time.Now(), Payload: json.RawMessage(`{}`)},
	}
	res, err := c.Push(context.Background(), evs)
	if err != nil || len(res) != 2 || res[0].Status != edgesync.StatusAccepted || res[1].Status != edgesync.StatusRejected || res[1].Code != "unknown_kind" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err = c.Register(context.Background(), "K7Q2M9XD", edgesync.DeviceInfo{}); err == nil {
		t.Fatal("reused code must fail")
	}
}

func TestRetryable(t *testing.T) {
	mk := func(status int, ra string) error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ra != "" {
				w.Header().Set("Retry-After", ra)
			}
			code := "x"
			if status == 401 {
				code = "device_revoked"
			}
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"code":%q,"message":"m"}}`, code)
		}))
		defer srv.Close()
		c := &edgesync.Client{BaseURL: srv.URL, Token: "t"}
		_, err := c.Push(context.Background(), []edgesync.Event{{Key: "a"}})
		return err
	}
	for _, tc := range []struct {
		status int
		want   bool
	}{{503, true}, {500, true}, {429, true}, {400, false}, {404, false}} {
		if got := edgesync.Retryable(mk(tc.status, "")); got != tc.want {
			t.Errorf("status %d: got %v", tc.status, got)
		}
	}
	rev := mk(401, "")
	if !errors.Is(rev, edgesync.ErrRevoked) || edgesync.Retryable(rev) {
		t.Errorf("revoked: %v", rev)
	}
	var he *edgesync.HTTPError
	if err := mk(429, "7"); !errors.As(err, &he) || he.RetryAfter != 7*time.Second {
		t.Errorf("retry-after: %v", err)
	}
	if err := mk(429, "Wed, 21 Oct 2026 07:28:00 GMT"); !errors.As(err, &he) || he.RetryAfter != 0 {
		t.Errorf("http-date must be ignored: %v", err)
	}

	// network error
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &edgesync.Client{BaseURL: url, Token: "t"}
	_, err := c.Push(context.Background(), []edgesync.Event{{Key: "a"}})
	if !edgesync.Retryable(err) {
		t.Errorf("net error must be retryable: %v", err)
	}
	if edgesync.Retryable(context.Canceled) || !edgesync.Retryable(context.DeadlineExceeded) ||
		edgesync.Retryable(nil) || edgesync.Retryable(errors.New("plain")) {
		t.Error("ctx/plain handling wrong")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Push(ctx, []edgesync.Event{{Key: "a"}})
	if edgesync.Retryable(err) {
		t.Errorf("canceled must not be retryable: %v", err)
	}
}

func TestPushSendsVersionHeaderAndToken(t *testing.T) {
	var gotV, gotA, gotP, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotV, gotA, gotP, gotCT = r.Header.Get("Rangka-Edge-Sync"), r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("Content-Type")
		var req edgesync.PushRequest
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(edgesync.PushResponse{Results: []edgesync.Result{{Key: "zzz", Status: "accepted"}}})
	}))
	defer srv.Close()
	c := &edgesync.Client{BaseURL: srv.URL, Token: "rke_abc"}
	_, err := c.Push(context.Background(), []edgesync.Event{{Key: "a", Kind: "sale.recorded"}})
	if gotV != "v1" || gotA != "Bearer rke_abc" || gotP != "/api/v1/edge/events" || gotCT != "application/json" {
		t.Fatalf("%q %q %q %q", gotV, gotA, gotP, gotCT)
	}
	if err == nil {
		t.Fatal("mismatched result key must error")
	}
}

func TestUnknownOutcomeIsRetryable(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"short body": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
			w.Write([]byte(`{"results":`))
		},
		"not json": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte("not json"))
		},
		"fewer results": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(edgesync.PushResponse{Results: []edgesync.Result{{Key: "a", Status: "accepted"}}})
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			c := &edgesync.Client{BaseURL: srv.URL, Token: "t"}
			_, err := c.Push(context.Background(), []edgesync.Event{{Key: "a"}, {Key: "b"}})
			if !errors.Is(err, edgesync.ErrUnknownOutcome) || !edgesync.Retryable(err) {
				t.Fatalf("want retryable unknown outcome, got %v", err)
			}
		})
	}
}
