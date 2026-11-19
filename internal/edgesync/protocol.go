package edgesync

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	Version       = "v1"
	VersionHeader = "Rangka-Edge-Sync"
	BasePath      = "/api/v1/edge"
)

const (
	StatusAccepted  = "accepted"
	StatusDuplicate = "duplicate"
	StatusRejected  = "rejected"
)

// Event kinds of v1 (contract 0058 §4, which amends 0006 §3). A sale, a void and a shift open or close are all
// screen.write events of the module's models (pos_sale, pos_shift, pos_shift_close); sale.recorded, sale.voided and
// stock.counted were dropped from v1 and the kernel answers them rejected / unknown_kind.
const (
	KindScreenWrite = "screen.write"
	KindPrintDone   = "print.done"
	KindPrintFailed = "print.failed"
	KindDeviceRead  = "device.read"
)

// Kinds are the event kinds the kernel accepts in v1.
var Kinds = []string{KindScreenWrite, KindPrintDone, KindPrintFailed, KindDeviceRead}

// Ops of a screen.write payload (contract 0058 §4).
var ScreenWriteOps = []string{"create", "submit", "cancel", "action"}

// ScreenWrite is the payload of a screen.write event (contract 0058 §4):
//   - create {screen, model, body?, name?, submit?}: submit true creates and submits in one unit (a paid sale); name is
//     the device name (STRK-{outlet}-{device}-{YY}{MM}-{#####}), taken at submit
//   - submit {model, name, draft, body?}: draft is the key of an earlier create event of the same device
//   - cancel {model, name}: name is the submitted document's device name
//   - action {model, name, action, args?}
type ScreenWrite struct {
	Screen string         `json:"screen,omitempty"`
	Model  string         `json:"model"`
	Op     string         `json:"op"`
	Name   string         `json:"name,omitempty"`
	Draft  string         `json:"draft,omitempty"`
	Action string         `json:"action,omitempty"`
	Submit bool           `json:"submit,omitempty"`
	Body   map[string]any `json:"body,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
}

// Validate checks the shape the kernel requires before it replays the write; the kernel still decides the rest.
func (w ScreenWrite) Validate() error {
	if w.Model == "" {
		return errors.New("screen.write: model is required")
	}
	switch w.Op {
	case "create":
		// a create without body is an empty record, as the kernel reads it
	case "submit":
		if w.Name == "" || w.Draft == "" {
			return errors.New("screen.write submit: name and draft are required")
		}
	case "cancel":
		if w.Name == "" {
			return errors.New("screen.write cancel: name is required")
		}
	case "action":
		if w.Name == "" || w.Action == "" {
			return errors.New("screen.write action: name and action are required")
		}
	default:
		return fmt.Errorf("screen.write: unknown op %q", w.Op)
	}
	return nil
}

type DeviceInfo struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

type RegisterRequest struct {
	PairingCode string     `json:"pairing_code"`
	Device      DeviceInfo `json:"device"`
}

type SyncSettings struct {
	PushMaxItems  int `json:"push_max_items"`
	PullIntervalS int `json:"pull_interval_s"`
}

type RegisterResponse struct {
	DeviceID string       `json:"device_id"`
	Token    string       `json:"token"`
	Site     string       `json:"site"`
	Org      string       `json:"org"`
	Outlet   string       `json:"outlet"`
	Sync     SyncSettings `json:"sync"`
}

type Event struct {
	Key        string          `json:"key"`
	Kind       string          `json:"kind"`
	Seq        int64           `json:"seq"`
	OccurredAt time.Time       `json:"occurred_at"`
	Actor      string          `json:"actor,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

type PushRequest struct {
	Events []Event `json:"events"`
}

type Result struct {
	Key     string `json:"key"`
	Status  string `json:"status"`
	Ref     string `json:"ref,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// Record is the written record as the REST API returns it (accepted screen.write only, contract 0058 §4).
	Record json.RawMessage `json:"record,omitempty"`
}

type PushResponse struct {
	Results []Result `json:"results"`
}

// ErrorBody is the error body shared with the rest of /api/v1.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// HTTPError is a non-2xx answer from the kernel.
type HTTPError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration // from Retry-After (integer seconds), 0 when absent
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("kernel answered %d %s: %s", e.Status, e.Code, e.Message)
}

// ErrRevoked means the kernel answered 401 device_revoked: stop syncing, keep the queue.
var ErrRevoked = errors.New("edgesync: device token revoked")
