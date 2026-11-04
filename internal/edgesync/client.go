package edgesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Retryable reports whether err means "outcome unknown, try the same events again".
func Retryable(err error) bool {
	if err == nil || errors.Is(err, ErrRevoked) || errors.Is(err, context.Canceled) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status >= 500 || he.Status == http.StatusTooManyRequests
	}
	var ue *url.Error
	var ne net.Error
	return errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded)
}

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client // nil: a client with a 30 s timeout
}

var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

func (c *Client) Register(ctx context.Context, code string, dev DeviceInfo) (RegisterResponse, error) {
	var out RegisterResponse
	err := c.do(ctx, "/register", RegisterRequest{PairingCode: code, Device: dev}, &out)
	return out, err
}

// Push sends events in order and returns one result per event, in the same order.
func (c *Client) Push(ctx context.Context, events []Event) ([]Result, error) {
	var out PushResponse
	if err := c.do(ctx, "/events", PushRequest{Events: events}, &out); err != nil {
		return nil, err
	}
	if len(out.Results) != len(events) {
		return nil, fmt.Errorf("edgesync: kernel answered %d results for %d events", len(out.Results), len(events))
	}
	for i, r := range out.Results {
		if r.Key != events[i].Key {
			return nil, fmt.Errorf("edgesync: result %d has key %q, want %q", i, r.Key, events[i].Key)
		}
	}
	return out.Results, nil
}

func (c *Client) do(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+BasePath+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(VersionHeader, Version)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		he := &HTTPError{Status: resp.StatusCode}
		var eb ErrorBody
		if json.Unmarshal(data, &eb) == nil {
			he.Code, he.Message = eb.Error.Code, eb.Error.Message
		}
		if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
			he.RetryAfter = time.Duration(n) * time.Second
		}
		if resp.StatusCode == http.StatusUnauthorized && he.Code == "device_revoked" {
			return fmt.Errorf("%w: %w", ErrRevoked, he)
		}
		return he
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("edgesync: bad kernel response: %w", err)
	}
	return nil
}
