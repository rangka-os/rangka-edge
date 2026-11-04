package kernelstub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rangka-os/rangka-edge/internal/edgesync"
)

func post(t *testing.T, srv *httptest.Server, path, token string, withVersion bool, body any) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
	if withVersion {
		req.Header.Set(edgesync.VersionHeader, edgesync.Version)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func register(t *testing.T, srv *httptest.Server, code string) (int, edgesync.RegisterResponse, []byte) {
	resp, body := post(t, srv, "/api/v1/edge/register", "", true, edgesync.RegisterRequest{PairingCode: code})
	var rr edgesync.RegisterResponse
	json.Unmarshal(body, &rr)
	return resp.StatusCode, rr, body
}

func TestRegisterSingleUseCode(t *testing.T) {
	s := New()
	s.AddPairingCode("K7Q2M9XD", "OUT-02")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	st, rr, _ := register(t, srv, "K7Q2M9XD")
	if st != 201 || !strings.HasPrefix(rr.DeviceID, "dev_") || len(rr.DeviceID) != 24 ||
		!strings.HasPrefix(rr.Token, "rke_") || len(rr.Token) != 36 || rr.Outlet != "OUT-02" ||
		rr.Site != "stub-site" || rr.Org != "stub-org" || rr.Sync.PushMaxItems != 100 || rr.Sync.PullIntervalS != 60 {
		t.Fatalf("bad register: %d %+v", st, rr)
	}
	st, _, body := register(t, srv, "K7Q2M9XD")
	if st != 400 || !strings.Contains(string(body), "pairing_code_invalid") {
		t.Fatalf("reuse: %d %s", st, body)
	}
	st, _, _ = register(t, srv, "NOPE")
	if st != 400 {
		t.Fatalf("unknown code: %d", st)
	}
}

func pushEvents(t *testing.T, srv *httptest.Server, token string, evs ...edgesync.Event) (int, edgesync.PushResponse) {
	resp, body := post(t, srv, "/api/v1/edge/events", token, true, edgesync.PushRequest{Events: evs})
	var pr edgesync.PushResponse
	json.Unmarshal(body, &pr)
	return resp.StatusCode, pr
}

func setup(t *testing.T) (*Stub, *httptest.Server, string) {
	s := New()
	s.AddPairingCode("AAAAAAAA", "OUT-01")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	_, rr, _ := register(t, srv, "AAAAAAAA")
	return s, srv, rr.Token
}

func TestPushAcceptDuplicateConflict(t *testing.T) {
	s, srv, tok := setup(t)
	ev := edgesync.Event{Key: "k1", Kind: "sale.recorded", Seq: 1, Payload: json.RawMessage(`{"a": 1}`)}
	_, r1 := pushEvents(t, srv, tok, ev)
	if r1.Results[0].Status != "accepted" || r1.Results[0].Ref != "edge_event/1" {
		t.Fatalf("%+v", r1)
	}
	ev2 := ev
	ev2.Payload = json.RawMessage(`{"a":1}`) // same after compaction
	_, r2 := pushEvents(t, srv, tok, ev2)
	if r2.Results[0].Status != "duplicate" || r2.Results[0].Ref != "edge_event/1" {
		t.Fatalf("%+v", r2)
	}
	ev3 := ev
	ev3.Payload = json.RawMessage(`{"a":2}`)
	_, r3 := pushEvents(t, srv, tok, ev3)
	if r3.Results[0].Status != "rejected" || r3.Results[0].Code != "key_conflict" {
		t.Fatalf("%+v", r3)
	}
	_, r4 := pushEvents(t, srv, tok, edgesync.Event{Key: "k2", Kind: "foo", Payload: json.RawMessage(`{}`)},
		edgesync.Event{Key: "", Kind: "sale.recorded", Payload: json.RawMessage(`{}`)},
		edgesync.Event{Key: "k3", Kind: "sale.recorded", Payload: json.RawMessage(`[1]`)})
	if r4.Results[0].Code != "unknown_kind" || r4.Results[1].Code != "payload_invalid" || r4.Results[2].Code != "payload_invalid" {
		t.Fatalf("%+v", r4)
	}
	if len(s.Events()) != 1 || s.PushCalls() != 4 {
		t.Fatalf("events=%d calls=%d", len(s.Events()), s.PushCalls())
	}
}

func TestVersionHeaderRequired(t *testing.T) {
	_, srv, tok := setup(t)
	resp, body := post(t, srv, "/api/v1/edge/events", tok, false, edgesync.PushRequest{})
	if resp.StatusCode != 400 || !strings.Contains(string(body), "unsupported_sync_version") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestRevokedToken401(t *testing.T) {
	s := New()
	s.AddPairingCode("AAAAAAAA", "OUT-01")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	_, rr, _ := register(t, srv, "AAAAAAAA")
	s.Revoke(rr.DeviceID)
	resp, body := post(t, srv, "/api/v1/edge/events", rr.Token, true, edgesync.PushRequest{})
	if resp.StatusCode != 401 || !strings.Contains(string(body), "device_revoked") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, _ = post(t, srv, "/api/v1/edge/events", "", true, edgesync.PushRequest{})
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func TestFailAndDrop(t *testing.T) {
	s, srv, tok := setup(t)
	ev := edgesync.Event{Key: "k1", Kind: "sale.recorded", Payload: json.RawMessage(`{}`)}
	s.FailNext(1, 500)
	resp, _ := post(t, srv, "/api/v1/edge/events", tok, true, edgesync.PushRequest{Events: []edgesync.Event{ev}})
	if resp.StatusCode != 500 || len(s.Events()) != 0 {
		t.Fatalf("fail: %d", resp.StatusCode)
	}
	s.DropResponseNext(1)
	resp, _ = post(t, srv, "/api/v1/edge/events", tok, true, edgesync.PushRequest{Events: []edgesync.Event{ev}})
	if resp.StatusCode != 503 || len(s.Events()) != 1 {
		t.Fatalf("drop: %d events=%d", resp.StatusCode, len(s.Events()))
	}
	_, r := pushEvents(t, srv, tok, ev)
	if r.Results[0].Status != "duplicate" {
		t.Fatalf("%+v", r)
	}
}

func TestTooManyEvents(t *testing.T) {
	_, srv, tok := setup(t)
	evs := make([]edgesync.Event, 101)
	resp, body := post(t, srv, "/api/v1/edge/events", tok, true, edgesync.PushRequest{Events: evs})
	if resp.StatusCode != 400 || !strings.Contains(string(body), "too_many_events") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}
