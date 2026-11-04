package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rangka-os/rangka-edge/internal/edgesync/kernelstub"
	"github.com/rangka-os/rangka-edge/internal/queue"
)

func exec(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

type statusJSON struct {
	DeviceID string `json:"device_id"`
	Pending  int    `json:"pending"`
	Dead     int    `json:"dead"`
	Done     int    `json:"done"`
}

func TestCLIEndToEnd(t *testing.T) {
	stub := kernelstub.New()
	stub.AddPairingCode("K7Q2M9XD", "outlet-1")
	srv := httptest.NewServer(stub.Handler())
	defer srv.Close()
	dir := t.TempDir()

	if c, _, e := exec(t, "", "register", "--dir", dir, "--kernel", srv.URL, "--code", "K7Q2M9XD", "--name", "kasir-1"); c != 0 {
		t.Fatalf("register: %d %s", c, e)
	}
	if c, _, _ := exec(t, "", "register", "--dir", dir, "--kernel", srv.URL, "--code", "K7Q2M9XD"); c != 1 {
		t.Fatalf("second register without --force should exit 1, got %d", c)
	}
	enq := func(args ...string) (int, string) {
		c, o, e := exec(t, "", append([]string{"enqueue", "--dir", dir}, args...)...)
		return c, o + e
	}
	if c, o := enq("--kind", "sale.recorded", "--key", "s-1", "--payload", `{"total":25000}`); c != 0 || !strings.HasPrefix(o, "queued dev_") || !strings.HasSuffix(strings.TrimSpace(o), ":s-1") {
		t.Fatalf("enqueue 1: %d %q", c, o)
	}
	if c, o := enq("--kind", "sale.recorded", "--key", "s-1", "--payload", `{"total":25000}`); c != 0 || !strings.HasPrefix(o, "already queued dev_") || !strings.HasSuffix(strings.TrimSpace(o), ":s-1") {
		t.Fatalf("enqueue 2: %d %q", c, o)
	}
	c, o := enq("--kind", "sale.recorded", "--payload", `{"total":1}`)
	if c != 0 || !strings.HasPrefix(o, "queued dev_") || !strings.HasSuffix(strings.TrimSpace(o), ":000000000002") {
		t.Fatalf("enqueue 3: %d %q", c, o)
	}
	var st statusJSON
	_, so, _ := exec(t, "", "status", "--dir", dir, "--json")
	if err := json.Unmarshal([]byte(so), &st); err != nil || st.Pending != 2 || !strings.HasPrefix(st.DeviceID, "dev_") {
		t.Fatalf("status: %v %s", err, so)
	}
	if c, o, e := exec(t, "", "flush", "--dir", dir); c != 0 || strings.TrimSpace(o) != "pushed 2, duplicates 0, rejected 0" {
		t.Fatalf("flush: %d %q %s", c, o, e)
	}
	if n := len(stub.Events()); n != 2 {
		t.Fatalf("stub events = %d", n)
	}
	_, so, _ = exec(t, "", "status", "--dir", dir, "--json")
	st = statusJSON{}
	if err := json.Unmarshal([]byte(so), &st); err != nil || st.Pending != 0 || st.Done != 2 {
		t.Fatalf("status after flush: %v %s", err, so)
	}
	if c, _ := enq("--kind", "foo", "--payload", `{}`); c != 1 {
		t.Fatalf("unknown kind exit %d", c)
	}
	if c, _ := enq("--kind", "sale.recorded", "--key", "s-1", "--payload", `{"total":9}`); c != 1 {
		t.Fatalf("key conflict exit %d", c)
	}
	if c, _ := enq("--kind", "sale.recorded", "--payload", `[1]`); c != 1 {
		t.Fatalf("non-object payload exit %d", c)
	}
	if c, o, _ := exec(t, `{"x":1}`, "enqueue", "--dir", dir, "--kind", "sale.recorded", "--payload", "-"); c != 0 || !strings.HasPrefix(o, "queued ") {
		t.Fatalf("stdin payload: %d %q", c, o)
	}
}

func TestCLIEnqueueBeforeRegister(t *testing.T) {
	c, _, e := exec(t, "", "enqueue", "--dir", t.TempDir(), "--kind", "sale.recorded", "--payload", "{}")
	if c != 1 || !strings.Contains(e, "not registered") {
		t.Fatalf("got %d %q", c, e)
	}
}

func TestCLIUsage(t *testing.T) {
	if c, _, _ := exec(t, ""); c != 2 {
		t.Fatalf("no args: %d", c)
	}
	if c, _, _ := exec(t, "", "bogus"); c != 2 {
		t.Fatalf("bogus: %d", c)
	}
	if c, _, _ := exec(t, "", "enqueue", "--nope"); c != 2 {
		t.Fatalf("bad flag: %d", c)
	}
	if c, o, _ := exec(t, "", "version"); c != 0 || strings.TrimSpace(o) != Version {
		t.Fatalf("version: %d %q", c, o)
	}
	if c, _, _ := exec(t, "", "devices"); c != 1 {
		t.Fatalf("devices: %d", c)
	}
}

func registerStub(t *testing.T) (dir string, srv *httptest.Server) {
	t.Helper()
	stub := kernelstub.New()
	stub.AddPairingCode("K7Q2M9XD", "outlet-1")
	srv = httptest.NewServer(stub.Handler())
	t.Cleanup(srv.Close)
	dir = t.TempDir()
	if c, _, e := exec(t, "", "register", "--dir", dir, "--kernel", srv.URL, "--code", "K7Q2M9XD"); c != 0 {
		t.Fatalf("register: %d %s", c, e)
	}
	return dir, srv
}

func TestCLIStatusWhileQueueHeld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no directory lock on windows")
	}
	dir, _ := registerStub(t)
	if c, _, e := exec(t, "", "enqueue", "--dir", dir, "--kind", "sale.recorded", "--payload", `{"a":1}`); c != 0 {
		t.Fatalf("enqueue: %d %s", c, e)
	}
	d, err := loadDevice(dir)
	if err != nil {
		t.Fatal(err)
	}
	q, err := queue.Open(filepath.Join(dir, "queue"), queue.Options{DeviceID: d.DeviceID})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	c, so, e := exec(t, "", "status", "--dir", dir, "--json")
	var st statusJSON
	if c != 0 || json.Unmarshal([]byte(so), &st) != nil || st.Pending != 1 {
		t.Fatalf("status while held: %d %q %s", c, so, e)
	}
	c, _, e = exec(t, "", "enqueue", "--dir", dir, "--kind", "sale.recorded", "--payload", `{"a":2}`)
	if c != 1 || !strings.Contains(e, "stop `rangka-edge run` first") {
		t.Fatalf("enqueue while held: %d %s", c, e)
	}
}

func TestCLIRegisterRefusesPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{"http://kernel.example.com", "ftp://x", "kernel.example.com", "http://10.0.0.5:8080"} {
		c, _, e := exec(t, "", "register", "--dir", dir, "--kernel", k, "--code", "ABCD2345")
		if c != 2 || !strings.Contains(e, "--kernel") {
			t.Fatalf("%s: %d %s", k, c, e)
		}
	}
	for _, k := range []string{"https://a.example", "http://localhost:1", "http://127.0.0.1:1", "http://[::1]:1"} {
		if err := checkKernelURL(k); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

func TestCLIRegisterForceRefusedWithPending(t *testing.T) {
	dir, srv := registerStub(t)
	if c, _, e := exec(t, "", "enqueue", "--dir", dir, "--kind", "sale.recorded", "--payload", `{"a":1}`); c != 0 {
		t.Fatalf("enqueue: %d %s", c, e)
	}
	before, _ := loadDevice(dir)
	c, _, e := exec(t, "", "register", "--dir", dir, "--kernel", srv.URL, "--code", "K7Q2M9XD", "--force")
	if c != 1 || !strings.Contains(e, "flush") {
		t.Fatalf("force with pending: %d %s", c, e)
	}
	if after, _ := loadDevice(dir); after != before {
		t.Fatal("device.json changed")
	}
	if c, _, e := exec(t, "", "flush", "--dir", dir); c != 0 {
		t.Fatalf("flush: %d %s", c, e)
	}
}
