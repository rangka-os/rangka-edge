package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rangka-os/rangka-edge/internal/edgesync/kernelstub"
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
	if c, o := enq("--kind", "sale.recorded", "--key", "s-1", "--payload", `{"total":25000}`); c != 0 || strings.TrimSpace(o) != "queued s-1" {
		t.Fatalf("enqueue 1: %d %q", c, o)
	}
	if c, o := enq("--kind", "sale.recorded", "--key", "s-1", "--payload", `{"total":25000}`); c != 0 || strings.TrimSpace(o) != "already queued s-1" {
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
