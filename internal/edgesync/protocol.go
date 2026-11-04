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

// Kinds are the event kinds the kernel accepts in v1.
var Kinds = []string{"sale.recorded", "sale.voided", "stock.counted", "print.done", "print.failed", "device.read"}

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
