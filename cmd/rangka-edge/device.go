package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rangka-os/rangka-edge/internal/queue"
)

// device is what `register` stores in <dir>/device.json.
type device struct {
	Kernel       string `json:"kernel"`
	DeviceID     string `json:"device_id"`
	Token        string `json:"token"`
	Site         string `json:"site"`
	Org          string `json:"org"`
	Outlet       string `json:"outlet"`
	PushMaxItems int    `json:"push_max_items"`
}

var errNotRegistered = errors.New("not registered: run `rangka-edge register` first")

// dataDir resolves the data directory: flag, else $RANGKA_EDGE_DIR, else <user config dir>/rangka-edge.
func dataDir(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if v := os.Getenv("RANGKA_EDGE_DIR"); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot find a data directory (use --dir): %w", err)
	}
	return filepath.Join(base, "rangka-edge"), nil
}

func devicePath(dir string) string { return filepath.Join(dir, "device.json") }

func loadDevice(dir string) (device, error) {
	var d device
	b, err := os.ReadFile(devicePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return d, errNotRegistered
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("%s: %w", devicePath(dir), err)
	}
	return d, nil
}

func saveDevice(dir string, d device) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := devicePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, devicePath(dir))
}

func openQueue(dir string, d device) (*queue.Queue, error) {
	qdir := filepath.Join(dir, "queue")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		return nil, err
	}
	q, err := queue.Open(qdir, queue.Options{DeviceID: d.DeviceID})
	if errors.Is(err, queue.ErrLocked) {
		return nil, fmt.Errorf("queue is in use by another rangka-edge process (is `run` active?): %w", err)
	}
	return q, err
}
