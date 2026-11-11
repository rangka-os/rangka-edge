// Package printsvc is the local print endpoint of `rangka-edge serve` (draft contract 0045 §1): the cashier screen on
// the same device posts a print job to http://127.0.0.1:9180/v1/print, the job is written to a durable spool before
// the answer (202), and a worker prints it on the printer its layout is routed to. Each try leaves a print.done or
// print.failed event (contract 0006 §3) through the Events hook, which `serve` points at the device's queue.
//
// Safety: the server listens on loopback only; a request needs the local print token (Authorization: Bearer) and,
// when it comes from a browser, an Origin from the allow list (CORS, and Chrome's private network access preflight).
// A job id is printed once: a resend of a queued or printed job id answers with its state and prints nothing.
package printsvc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rangka-os/rangka-edge/internal/escpos"
	"github.com/rangka-os/rangka-edge/internal/printdoc"
	"github.com/rangka-os/rangka-edge/internal/transport"
)

// DefaultAddr is where `rangka-edge serve` listens (contract 0045 §1).
const DefaultAddr = "127.0.0.1:9180"

// MaxBody bounds a print request.
const MaxBody = 1 << 20

// MaxAttempts is how many times a job is tried before it is moved to spool/failed (each failed try is an event).
const MaxAttempts = 5

var jobIDRe = regexp.MustCompile(`^[A-Za-z0-9._:@/-]{1,200}$`)

// Config is the outlet's local print setup: printer names to addresses, layouts to printer names, and the site
// origins allowed to print. It lives on the Edge (printers.json), never in .rk files (contract 0045 §3).
type Config struct {
	Printers map[string]string `json:"printers"` // kasir -> tcp://192.168.1.50:9100
	Layouts  map[string]string `json:"layouts"`  // receipt -> kasir, kitchen -> dapur
	Origins  []string          `json:"origins"`  // https://tahu-contoh.juragan.id
	Cols     int               `json:"cols"`     // 48 (80 mm, default) or 32 (58 mm)
}

// PrinterFor picks the printer of a job: the one it names, else its layout's, else the only printer.
func (c Config) PrinterFor(layout, name string) (string, string, error) {
	if name == "" {
		name = c.Layouts[layout]
	}
	if name == "" && len(c.Printers) == 1 {
		for n := range c.Printers {
			name = n
		}
	}
	addr, ok := c.Printers[name]
	if !ok || addr == "" {
		return "", "", fmt.Errorf("no printer for layout %q (printer %q)", layout, name)
	}
	return name, addr, nil
}

// Request is the body of POST /v1/print.
type Request struct {
	JobID   string          `json:"job_id"`  // the screen's outbox idempotency key + layout
	Model   string          `json:"model"`   // pos_sale, kitchen_ticket
	Name    string          `json:"name"`    // the record's name
	Layout  string          `json:"layout"`  // receipt | kitchen
	Printer string          `json:"printer"` // optional: a printer name of the config
	Record  json.RawMessage `json:"record"`  // the record (or a printdoc job with doc, header, station ...)
	Copy    bool            `json:"copy"`
	Drawer  bool            `json:"drawer"`
}

// Job is a spooled request with its state.
type Job struct {
	Request
	State    string    `json:"state"` // queued | done | failed
	Attempts int       `json:"attempts"`
	Error    string    `json:"error,omitempty"`
	Received time.Time `json:"received"`
	Printed  time.Time `json:"printed,omitempty"`
}

// Event is what a try leaves for the kernel: kind print.done or print.failed, key <job>:<attempt>:done|failed.
type Event struct {
	Kind    string
	Key     string
	At      time.Time
	Payload map[string]any
}

// Service owns the spool directory and prints its jobs one at a time, oldest first; a job that failed waits
// before its next try without holding up the others.
type Service struct {
	Dir    string // spool directory: queued/, done/, failed/
	Config Config
	Token  string                                       // the local print token
	Events func(Event)                                  // optional: record print.done / print.failed
	Open   func(addr string) (transport.Printer, error) // default transport.OpenPrinter
	Logf   func(format string, a ...any)                // optional
	Retry  func(attempt int) time.Duration              // wait before the next try; default 2s, 4s, 8s ...
	Now    func() time.Time

	mu   sync.Mutex
	wake chan struct{}
}

// New prepares the spool directories.
func New(dir string, cfg Config, token string) (*Service, error) {
	for _, d := range []string{"queued", "done", "failed"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	if cfg.Cols == 0 {
		cfg.Cols = escpos.Cols80
	}
	return &Service{Dir: dir, Config: cfg, Token: token, wake: make(chan struct{}, 1)}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

// file is a job's spool file name: a hash of its id, so any id is a safe file name.
func file(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:12]) + ".json"
}

// Lookup returns a job by id and its state, or ok false.
func (s *Service) Lookup(id string) (Job, bool) {
	for _, st := range []string{"done", "queued", "failed"} {
		if j, err := readJob(filepath.Join(s.Dir, st, file(id))); err == nil {
			return j, true
		}
	}
	return Job{}, false
}

// Submit validates a request and spools it (written and synced before it returns). A job id seen before is not
// spooled again: its current state comes back with added false.
func (s *Service) Submit(r Request) (Job, bool, error) {
	if !jobIDRe.MatchString(r.JobID) {
		return Job{}, false, errors.New("job_id is required (letters, digits and ._:@/-, at most 200)")
	}
	if r.Layout == "" {
		r.Layout = "receipt"
	}
	if r.Layout != "receipt" && r.Layout != "kitchen" {
		return Job{}, false, fmt.Errorf("layout %q: want receipt or kitchen", r.Layout)
	}
	if _, _, err := s.Config.PrinterFor(r.Layout, r.Printer); err != nil {
		return Job{}, false, err
	}
	pj, err := printdoc.ParseJob(r.Record)
	if err != nil {
		return Job{}, false, err
	}
	pj.Layout = r.Layout
	if _, err := printdoc.Render(pj, s.Config.Cols); err != nil {
		return Job{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.Lookup(r.JobID); ok {
		return j, false, nil
	}
	j := Job{Request: r, State: "queued", Received: s.now().UTC()}
	if err := writeJob(filepath.Join(s.Dir, "queued", file(r.JobID)), j); err != nil {
		return Job{}, false, err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return j, true, nil
}

// Run prints spooled jobs until ctx ends; jobs left from before a restart are printed first, oldest first. It looks
// again when a job comes in and every second (a retry that is due).
func (s *Service) Run(ctx context.Context) {
	for {
		for s.next(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(time.Second):
		}
	}
}

// next prints the oldest queued job once; false when there is nothing to do now.
func (s *Service) next(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	path, j, ok := s.oldest()
	if !ok {
		return false
	}
	err := s.print(ctx, j)
	j.Attempts++
	now := s.now().UTC()
	j.Printed = now
	kind := "print.done"
	if err != nil {
		kind = "print.failed"
		j.Error = err.Error()
	} else {
		j.Error = ""
	}
	s.emit(j, kind, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		j.State = "done"
		s.move(path, "done", j)
		s.logf("printed %s %s (%s)", j.Layout, j.Name, j.JobID)
	case j.Attempts >= MaxAttempts:
		j.State = "failed"
		s.move(path, "failed", j)
		s.logf("gave up %s after %d tries: %v", j.JobID, j.Attempts, err)
	default:
		_ = writeJob(path, j)
		s.logf("print %s failed (try %d): %v", j.JobID, j.Attempts, err)
	}
	return true
}

func (s *Service) retryWait(attempt int) time.Duration {
	if s.Retry != nil {
		return s.Retry(attempt)
	}
	return time.Duration(1<<min(attempt, 6)) * time.Second
}

func (s *Service) print(ctx context.Context, j Job) error {
	pj, err := printdoc.ParseJob(j.Record)
	if err != nil {
		return err
	}
	pj.Layout = j.Layout
	pj.Copy = pj.Copy || j.Copy
	pj.Drawer = pj.Drawer || j.Drawer
	if pj.PrintedAt == "" {
		pj.PrintedAt = j.Received.Format(time.RFC3339)
	}
	out, err := printdoc.Render(pj, s.Config.Cols)
	if err != nil {
		return err
	}
	_, addr, err := s.Config.PrinterFor(j.Layout, j.Printer)
	if err != nil {
		return err
	}
	open := s.Open
	if open == nil {
		open = transport.OpenPrinter
	}
	p, err := open(addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return p.Print(ctx, out)
}

func (s *Service) emit(j Job, kind string, at time.Time) {
	if s.Events == nil {
		return
	}
	name, _, _ := s.Config.PrinterFor(j.Layout, j.Printer)
	pl := map[string]any{"model": orDefault(j.Model, "pos_sale"), "doc": j.Name, "layout": j.Layout, "printer": name,
		"job_id": j.JobID, "attempt": j.Attempts, "copy": j.Copy}
	if j.Error != "" {
		pl["error"] = j.Error
	}
	s.Events(Event{Kind: kind, Key: fmt.Sprintf("print:%s:%d:%s", j.JobID, j.Attempts, strings.TrimPrefix(kind, "print.")), At: at, Payload: pl})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (s *Service) oldest() (string, Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, _ := os.ReadDir(filepath.Join(s.Dir, "queued"))
	var jobs []struct {
		path string
		j    Job
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(s.Dir, "queued", e.Name())
		j, err := readJob(p)
		if err != nil {
			s.logf("spool: skipping unreadable %s: %v", e.Name(), err)
			continue
		}
		// a job that failed waits before its next try (Printed holds the last try's time while queued); the jobs
		// behind it, on another printer, do not wait for it
		if j.Attempts > 0 && s.now().Before(j.Printed.Add(s.retryWait(j.Attempts))) {
			continue
		}
		jobs = append(jobs, struct {
			path string
			j    Job
		}{p, j})
	}
	if len(jobs) == 0 {
		return "", Job{}, false
	}
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].j.Received.Before(jobs[b].j.Received) })
	return jobs[0].path, jobs[0].j, true
}

func (s *Service) move(from, state string, j Job) {
	to := filepath.Join(s.Dir, state, filepath.Base(from))
	if err := writeJob(to, j); err != nil {
		s.logf("spool: %v", err)
		return
	}
	_ = os.Remove(from)
}

func readJob(path string) (Job, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Job{}, err
	}
	var j Job
	return j, json.Unmarshal(b, &j)
}

// writeJob writes a job atomically and durably: a temp file, fsync, rename, fsync of the directory.
func writeJob(path string, j Job) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".job-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Handler serves /v1/health, /v1/print and /v1/jobs/<job_id>.
func (s *Service) Handler(version, device string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if !s.gate(w, r, http.MethodGet) {
			return
		}
		names := make([]string, 0, len(s.Config.Printers))
		for n := range s.Config.Printers {
			names = append(names, n)
		}
		sort.Strings(names)
		reply(w, 200, map[string]any{"ok": true, "version": version, "device": device, "printers": names, "layouts": s.Config.Layouts})
	})
	mux.HandleFunc("/v1/print", func(w http.ResponseWriter, r *http.Request) {
		if !s.gate(w, r, http.MethodPost) {
			return
		}
		var req Request
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err != nil {
			reply(w, 400, map[string]any{"error": map[string]string{"code": "bad_request", "message": "body must be a JSON print job of at most 1 MB"}})
			return
		}
		j, added, err := s.Submit(req)
		if err != nil {
			reply(w, 422, map[string]any{"error": map[string]string{"code": "invalid_job", "message": err.Error()}})
			return
		}
		code := 202
		if !added && j.State == "done" {
			code = 200
		}
		reply(w, code, map[string]any{"job_id": j.JobID, "state": j.State, "attempts": j.Attempts, "duplicate": !added})
	})
	mux.HandleFunc("/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if !s.gate(w, r, http.MethodGet) {
			return
		}
		j, ok := s.Lookup(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"))
		if !ok {
			reply(w, 404, map[string]any{"error": map[string]string{"code": "not_found"}})
			return
		}
		reply(w, 200, map[string]any{"job_id": j.JobID, "state": j.State, "attempts": j.Attempts, "error": j.Error})
	})
	return mux
}

// gate applies CORS (allowed origins only, private network access preflight), the method and the print token.
func (s *Service) gate(w http.ResponseWriter, r *http.Request, method string) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		if !slices.Contains(s.Config.Origins, origin) {
			reply(w, 403, map[string]any{"error": map[string]string{"code": "origin_not_allowed"}})
			return false
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Allow-Methods", method+", OPTIONS")
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			h.Set("Access-Control-Allow-Private-Network", "true")
		}
	}
	if r.Method == http.MethodOptions && origin != "" {
		w.WriteHeader(204)
		return false
	}
	if r.Method != method {
		reply(w, 405, map[string]any{"error": map[string]string{"code": "method_not_allowed"}})
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
		reply(w, 401, map[string]any{"error": map[string]string{"code": "bad_token"}})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
