// Package kernelstub is an in-process stand-in for the kernel's edge-sync endpoints (contract 0006 §2-3).
// It exists for tests and the CLI demo until the kernel ships them.
package kernelstub

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/rangka-os/rangka-edge/internal/edgesync"
)

const maxItems = 100

type stored struct {
	ev  edgesync.Event
	sum [32]byte
	ref string
}

type Stub struct {
	mu        sync.Mutex
	codes     map[string]string // code -> outlet
	tokens    map[string]string // token -> device id
	revoked   map[string]bool   // device id
	byKey     map[string]*stored
	order     []*stored
	failN     int
	failCode  int
	dropN     int
	pushCalls int
}

func New() *Stub {
	return &Stub{codes: map[string]string{}, tokens: map[string]string{}, revoked: map[string]bool{}, byKey: map[string]*stored{}}
}

func (s *Stub) AddPairingCode(code, outlet string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = outlet
}

// Events returns the stored events in arrival order.
func (s *Stub) Events() []edgesync.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]edgesync.Event, len(s.order))
	for i, st := range s.order {
		out[i] = st.ev
	}
	return out
}

// FailNext makes the next n /events calls answer status without storing anything.
func (s *Stub) FailNext(n, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failN, s.failCode = n, status
}

// DropResponseNext makes the next n /events calls store the batch and then answer 503, as if the response was lost.
func (s *Stub) DropResponseNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropN = n
}

func (s *Stub) Revoke(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked[deviceID] = true
}

func (s *Stub) PushCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushCalls
}

func (s *Stub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+edgesync.BasePath+"/register", s.register)
	mux.HandleFunc("POST "+edgesync.BasePath+"/events", s.events)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	var eb edgesync.ErrorBody
	eb.Error.Code, eb.Error.Message = code, msg
	writeJSON(w, status, eb)
}

func checkVersion(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(edgesync.VersionHeader) != edgesync.Version {
		writeErr(w, 400, "unsupported_sync_version", "supported: v1")
		return false
	}
	return true
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func (s *Stub) register(w http.ResponseWriter, r *http.Request) {
	if !checkVersion(w, r) {
		return
	}
	var req edgesync.RegisterRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, 400, "pairing_code_invalid", "pairing code is not valid")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	outlet, ok := s.codes[req.PairingCode]
	if !ok {
		writeErr(w, 400, "pairing_code_invalid", "pairing code is not valid")
		return
	}
	delete(s.codes, req.PairingCode)
	id, tok := "dev_"+randHex(20), "rke_"+randHex(32)
	s.tokens[tok] = id
	writeJSON(w, 201, edgesync.RegisterResponse{DeviceID: id, Token: tok, Site: "stub-site", Org: "stub-org", Outlet: outlet,
		Sync: edgesync.SyncSettings{PushMaxItems: maxItems, PullIntervalS: 60}})
}

func (s *Stub) events(w http.ResponseWriter, r *http.Request) {
	if !checkVersion(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushCalls++
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if id, ok := s.tokens[tok]; !ok || s.revoked[id] {
		writeErr(w, 401, "device_revoked", "device token is revoked or unknown")
		return
	}
	if s.failN > 0 {
		s.failN--
		writeErr(w, s.failCode, "stub_failure", "injected failure")
		return
	}
	var req edgesync.PushRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, 400, "bad_request", "body is not valid JSON")
		return
	}
	if len(req.Events) > maxItems {
		writeErr(w, 400, "too_many_events", fmt.Sprintf("at most %d events per request", maxItems))
		return
	}
	results := make([]edgesync.Result, len(req.Events))
	for i, ev := range req.Events {
		results[i] = s.process(ev)
	}
	if s.dropN > 0 {
		s.dropN--
		writeErr(w, 503, "stub_failure", "response lost")
		return
	}
	writeJSON(w, 200, edgesync.PushResponse{Results: results})
}

func rejected(key, code, msg string) edgesync.Result {
	return edgesync.Result{Key: key, Status: edgesync.StatusRejected, Code: code, Message: msg}
}

func (s *Stub) process(ev edgesync.Event) edgesync.Result {
	known := false
	for _, k := range edgesync.Kinds {
		known = known || k == ev.Kind
	}
	if !known {
		return rejected(ev.Key, "unknown_kind", "unknown event kind")
	}
	if ev.Key == "" || len(ev.Key) > 200 {
		return rejected(ev.Key, "payload_invalid", "key must be 1 to 200 characters")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(ev.Payload, &obj) != nil || obj == nil {
		return rejected(ev.Key, "payload_invalid", "payload must be a JSON object")
	}
	var buf bytes.Buffer
	if json.Compact(&buf, ev.Payload) != nil {
		return rejected(ev.Key, "payload_invalid", "payload must be a JSON object")
	}
	sum := sha256.Sum256(append([]byte(ev.Kind+"\x00"), buf.Bytes()...))
	if old, ok := s.byKey[ev.Key]; ok {
		if old.sum == sum {
			return edgesync.Result{Key: ev.Key, Status: edgesync.StatusDuplicate, Ref: old.ref}
		}
		return rejected(ev.Key, "key_conflict", "key already used with a different event")
	}
	st := &stored{ev: ev, sum: sum, ref: fmt.Sprintf("edge_event/%d", len(s.order)+1)}
	s.byKey[ev.Key] = st
	s.order = append(s.order, st)
	return edgesync.Result{Key: ev.Key, Status: edgesync.StatusAccepted, Ref: st.ref}
}
