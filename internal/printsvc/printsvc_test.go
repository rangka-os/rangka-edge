package printsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rangka-os/rangka-edge/internal/escpos"
	"github.com/rangka-os/rangka-edge/internal/transport"
)

const sale = `{"name": "STRK-DGO-2611-00001", "date": "2026-11-11", "docstatus": 1,
 "items": [{"description": "Tahu goreng original", "qty": 2, "rate": 15000, "amount": 30000}],
 "grand_total": 30000, "payments": [{"method": "cash", "amount": 50000}], "change_amount": 20000}`

var token = strings.Repeat("t", 32) // a test print token

// fakePrinters records what each address printed; an address in down fails.
type fakePrinters struct {
	mu   sync.Mutex
	got  map[string][]string
	down map[string]bool
}

type fakePrinter struct {
	f    *fakePrinters
	addr string
}

func (p fakePrinter) Print(_ context.Context, b []byte) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if p.f.down[p.addr] {
		return errors.New("printer offline")
	}
	p.f.got[p.addr] = append(p.f.got[p.addr], escpos.Decode(b))
	return nil
}
func (p fakePrinter) String() string { return p.addr }

func (f *fakePrinters) open(addr string) (transport.Printer, error) { return fakePrinter{f, addr}, nil }

func (f *fakePrinters) count(addr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got[addr])
}

func newSvc(t *testing.T, dir string, f *fakePrinters) (*Service, *[]Event) {
	t.Helper()
	cfg := Config{Printers: map[string]string{"kasir": "tcp://kasir", "dapur": "tcp://dapur"},
		Layouts: map[string]string{"receipt": "kasir", "kitchen": "dapur"}, Origins: []string{"https://tahu.juragan.id"}}
	s, err := New(dir, cfg, token)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	evs := &[]Event{}
	s.Open = f.open
	s.Retry = func(int) time.Duration { return 0 }
	s.Events = func(e Event) { mu.Lock(); *evs = append(*evs, e); mu.Unlock() }
	return s, evs
}

func req(id, layout string) Request {
	return Request{JobID: id, Model: "pos_sale", Name: "STRK-DGO-2611-00001", Layout: layout, Record: json.RawMessage(sale)}
}

func drain(s *Service) {
	for s.next(context.Background()) {
	}
}

func TestPrintOnceAndRoute(t *testing.T) {
	f := &fakePrinters{got: map[string][]string{}, down: map[string]bool{}}
	s, evs := newSvc(t, t.TempDir(), f)
	if _, added, err := s.Submit(req("ob-1:receipt", "receipt")); err != nil || !added {
		t.Fatalf("submit: %v %v", added, err)
	}
	if _, added, err := s.Submit(req("ob-1:kitchen", "kitchen")); err != nil || !added {
		t.Fatalf("submit kitchen: %v %v", added, err)
	}
	drain(s)
	if f.count("tcp://kasir") != 1 || f.count("tcp://dapur") != 1 {
		t.Fatalf("routed: %v", f.got)
	}
	if !strings.Contains(f.got["tcp://kasir"][0], "Rp 20.000") || strings.Contains(f.got["tcp://dapur"][0], "Rp") {
		t.Fatalf("layouts: %q", f.got)
	}
	// the same job id again (the screen resends): nothing printed, its state comes back
	j, added, err := s.Submit(req("ob-1:receipt", "receipt"))
	drain(s)
	if err != nil || added || j.State != "done" || f.count("tcp://kasir") != 1 {
		t.Fatalf("resend: %+v %v %v %d", j, added, err, f.count("tcp://kasir"))
	}
	if len(*evs) != 2 || (*evs)[0].Kind != "print.done" || (*evs)[0].Key != "print:ob-1:receipt:1:done" {
		t.Fatalf("events: %+v", *evs)
	}
}

func TestRetryFailAndOtherPrinterNotHeld(t *testing.T) {
	f := &fakePrinters{got: map[string][]string{}, down: map[string]bool{"tcp://dapur": true}}
	s, evs := newSvc(t, t.TempDir(), f)
	s.Retry = func(int) time.Duration { return time.Hour } // a failed job waits; the receipt behind it does not
	s.Submit(req("k-1", "kitchen"))
	s.Submit(req("r-1", "receipt"))
	drain(s)
	if f.count("tcp://kasir") != 1 {
		t.Fatalf("receipt held up behind a broken kitchen printer: %v", f.got)
	}
	if j, _ := s.Lookup("k-1"); j.State != "queued" || j.Attempts != 1 || j.Error == "" {
		t.Fatalf("kitchen job after one failed try: %+v", j)
	}
	s.Retry = func(int) time.Duration { return 0 }
	drain(s)
	j, _ := s.Lookup("k-1")
	if j.State != "failed" || j.Attempts != MaxAttempts {
		t.Fatalf("gave up: %+v", j)
	}
	failed := 0
	for _, e := range *evs {
		if e.Kind == "print.failed" {
			failed++
		}
	}
	if failed != MaxAttempts {
		t.Fatalf("one print.failed per try: %d %+v", failed, *evs)
	}
	// the screen sends it again after the printer is fixed: queued again and printed
	f.down["tcp://dapur"] = false
	j, added, err := s.Submit(req("k-1", "kitchen"))
	if err != nil || added || j.State != "queued" {
		t.Fatalf("resend of a failed job: %+v %v %v", j, added, err)
	}
	drain(s)
	if j, _ := s.Lookup("k-1"); j.State != "done" || f.count("tcp://dapur") != 1 {
		t.Fatalf("after resend: %+v %d", j, f.count("tcp://dapur"))
	}
}

func TestSpoolSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	f := &fakePrinters{got: map[string][]string{}, down: map[string]bool{}}
	s, _ := newSvc(t, dir, f)
	s.Submit(req("r-1", "receipt")) // accepted, then the process stops before printing
	s2, _ := newSvc(t, dir, f)
	drain(s2)
	if f.count("tcp://kasir") != 1 {
		t.Fatalf("restart did not print the spooled job: %v", f.got)
	}
}

func TestSubmitRefuses(t *testing.T) {
	f := &fakePrinters{got: map[string][]string{}, down: map[string]bool{}}
	s, _ := newSvc(t, t.TempDir(), f)
	for name, r := range map[string]Request{
		"no job id":       {Layout: "receipt", Record: json.RawMessage(sale)},
		"bad job id":      {JobID: "id with spaces", Layout: "receipt", Record: json.RawMessage(sale)},
		"label layout":    {JobID: "x", Layout: "label", Record: json.RawMessage(sale)},
		"no record name":  {JobID: "x", Layout: "receipt", Record: json.RawMessage(`{"items": []}`)},
		"unknown printer": {JobID: "x", Layout: "receipt", Printer: "bar", Record: json.RawMessage(sale)},
	} {
		if _, _, err := s.Submit(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHTTPGate(t *testing.T) {
	f := &fakePrinters{got: map[string][]string{}, down: map[string]bool{}}
	s, _ := newSvc(t, t.TempDir(), f)
	srv := httptest.NewServer(s.Handler("test", "dev-1"))
	defer srv.Close()
	body, _ := json.Marshal(req("ob-9:receipt", "receipt"))
	do := func(method, path, origin, tok string, b []byte) (int, map[string]any, http.Header) {
		r, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		if method == http.MethodOptions {
			r.Header.Set("Access-Control-Request-Private-Network", "true")
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m, resp.Header
	}
	if c, _, _ := do("POST", "/v1/print", "", "", body); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	if c, _, _ := do("POST", "/v1/print", "", "wrong-token-wrong-token-wrong-tok", body); c != 401 {
		t.Fatalf("wrong token: %d", c)
	}
	if c, _, _ := do("POST", "/v1/print", "https://evil.example", token, body); c != 403 {
		t.Fatalf("other origin: %d", c)
	}
	c, _, h := do("OPTIONS", "/v1/print", "https://tahu.juragan.id", "", nil)
	if c != 204 || h.Get("Access-Control-Allow-Origin") != "https://tahu.juragan.id" || h.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("preflight: %d %v", c, h)
	}
	c, m, h := do("POST", "/v1/print", "https://tahu.juragan.id", token, body)
	if c != 202 || m["state"] != "queued" || h.Get("Access-Control-Allow-Origin") != "https://tahu.juragan.id" {
		t.Fatalf("print: %d %v", c, m)
	}
	drain(s)
	if c, m, _ := do("POST", "/v1/print", "https://tahu.juragan.id", token, body); c != 200 || m["state"] != "done" || m["duplicate"] != true {
		t.Fatalf("resend: %d %v", c, m)
	}
	if c, m, _ := do("GET", "/v1/jobs/ob-9:receipt", "", token, nil); c != 200 || m["state"] != "done" {
		t.Fatalf("job: %d %v", c, m)
	}
	if c, m, _ := do("GET", "/v1/health", "", token, nil); c != 200 || m["device"] != "dev-1" {
		t.Fatalf("health: %d %v", c, m)
	}
	if c, _, _ := do("POST", "/v1/print", "", token, []byte(`{"job_id": "x", "layout": "receipt", "record": {}}`)); c != 422 {
		t.Fatalf("bad job: %d", c)
	}
	if c, _, _ := do("POST", "/v1/print", "", token, bytes.Repeat([]byte("x"), MaxBody+10)); c != 400 {
		t.Fatalf("too big: %d", c)
	}
	if f.count("tcp://kasir") != 1 {
		t.Fatalf("printed %d times", f.count("tcp://kasir"))
	}
}

// audit E1: the drawer opens for a paid receipt on its first try only, never for a draft, a copy or a retry, and the
// event says whether it opened.
func TestDrawer(t *testing.T) {
	f := &fakePrinters{got: map[string][]string{}, down: map[string]bool{}}
	s, evs := newSvc(t, t.TempDir(), f)
	draft := strings.Replace(sale, `"docstatus": 1`, `"docstatus": 0`, 1)
	for id, r := range map[string]Request{
		"paid":  {JobID: "paid", Layout: "receipt", Drawer: true, Record: json.RawMessage(sale)},
		"draft": {JobID: "draft", Layout: "receipt", Drawer: true, Record: json.RawMessage(draft)},
		"copy":  {JobID: "copy", Layout: "receipt", Drawer: true, Copy: true, Record: json.RawMessage(sale)},
	} {
		if _, _, err := s.Submit(r); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	drain(s)
	kicks := 0
	for _, p := range f.got["tcp://kasir"] {
		kicks += strings.Count(p, "[LACI]")
	}
	if kicks != 1 {
		t.Fatalf("drawer kicks %d, want 1 (paid only): %q", kicks, f.got["tcp://kasir"])
	}
	opened := map[string]bool{}
	for _, e := range *evs {
		opened[e.Payload["job_id"].(string)] = e.Payload["drawer"].(bool)
	}
	if !opened["paid"] || opened["draft"] || opened["copy"] {
		t.Fatalf("drawer in events: %v", opened)
	}
	// a retry of a paid receipt does not kick again
	f.down["tcp://kasir"] = true
	s.Submit(Request{JobID: "paid-2", Layout: "receipt", Drawer: true, Record: json.RawMessage(sale)})
	s.next(context.Background())
	f.down["tcp://kasir"] = false
	drain(s)
	if p := f.got["tcp://kasir"][len(f.got["tcp://kasir"])-1]; strings.Contains(p, "[LACI]") {
		t.Fatalf("retry kicked the drawer: %q", p)
	}
}

// audit E4: a job found in both done/ and queued/ (a stop between the two writes) is not printed again.
func TestDoneNotReprinted(t *testing.T) {
	dir := t.TempDir()
	f := &fakePrinters{got: map[string][]string{}, down: map[string]bool{}}
	s, _ := newSvc(t, dir, f)
	s.Submit(req("r-1", "receipt"))
	drain(s)
	j, _ := s.Lookup("r-1")
	if err := writeJob(filepath.Join(dir, "queued", file("r-1")), j); err != nil {
		t.Fatal(err)
	}
	drain(s)
	if f.count("tcp://kasir") != 1 {
		t.Fatalf("reprinted: %d", f.count("tcp://kasir"))
	}
}
