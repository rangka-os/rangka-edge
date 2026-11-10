package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rangka-os/rangka-edge/internal/escpos"
)

const saleJob = `{"doc": {"name": "STRK-DGO-2611-00001", "date": "2026-11-11", "docstatus": 1,
 "items": [{"description": "Tahu goreng original", "qty": 2, "rate": 15000, "amount": 30000}],
 "grand_total": 30000, "payments": [{"method": "cash", "amount": 50000}], "change_amount": 20000},
 "header": {"name": "Tahu Contoh"}, "outlet_name": "Outlet Dago"}`

func TestCLIPrintPreview(t *testing.T) {
	c, out, e := exec(t, saleJob, "print", "--preview", "-")
	if c != 0 || !strings.Contains(out, "Kembalian                              Rp 20.000") || !strings.Contains(out, "[POTONG]") {
		t.Fatalf("preview: %d %q %s", c, out, e)
	}
	c, out, _ = exec(t, saleJob, "print", "--preview", "--layout", "kitchen", "--cols", "32", "-")
	if c != 0 || !strings.Contains(out, "DAPUR") || strings.Contains(out, "Rp") {
		t.Fatalf("kitchen preview: %d %q", c, out)
	}
}

func TestCLIPrintUsage(t *testing.T) {
	if c, _, _ := exec(t, saleJob, "print", "-"); c != 2 {
		t.Fatalf("no printer: exit %d", c)
	}
	if c, _, _ := exec(t, saleJob, "print", "--printer", "lpt1", "-"); c != 2 {
		t.Fatalf("bad printer: exit %d", c)
	}
	if c, _, _ := exec(t, saleJob, "print", "--preview", "--layout", "label", "-"); c != 2 {
		t.Fatalf("bad layout: exit %d", c)
	}
	if c, _, _ := exec(t, `{"doc":{}}`, "print", "--preview", "-"); c != 1 {
		t.Fatalf("no record name: exit %d", c)
	}
}

// A registered Edge queues print.done for a printed job and print.failed (exit 1) when the printer is unreachable.
func TestCLIPrintQueuesEvents(t *testing.T) {
	dir, srv := registerStub(t)
	defer srv.Close()
	paper := filepath.Join(t.TempDir(), "lp0")
	c, out, e := exec(t, saleJob, "print", "--dir", dir, "--printer", "file:"+paper, "--job", "j1", "--drawer", "-")
	if c != 0 || !strings.Contains(out, "printed receipt STRK-DGO-2611-00001") || !strings.Contains(e, "queued print.done") {
		t.Fatalf("print: %d %q %q", c, out, e)
	}
	b, _ := os.ReadFile(paper)
	if txt := escpos.Decode(b); !strings.Contains(txt, "Total                                  Rp 30.000") || !strings.HasSuffix(txt, "[LACI]\n") {
		t.Fatalf("paper: %q", txt)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c, _, e = exec(t, saleJob, "print", "--dir", dir, "--printer", "tcp://"+addr, "--job", "j2", "-")
	if c != 1 || !strings.Contains(e, "queued print.failed") {
		t.Fatalf("unreachable printer: %d %q", c, e)
	}
	_, out, _ = exec(t, "", "status", "--dir", dir, "--json")
	var s statusJSON
	if err := json.Unmarshal([]byte(out), &s); err != nil || s.Pending != 2 {
		t.Fatalf("status: %v %q", err, out)
	}
	// the same job id again is the same event: still two pending
	exec(t, saleJob, "print", "--dir", dir, "--printer", "file:"+paper, "--job", "j1", "-")
	_, out, _ = exec(t, "", "status", "--dir", dir, "--json")
	_ = json.Unmarshal([]byte(out), &s)
	if s.Pending != 2 {
		t.Fatalf("a repeated job id queued twice: %q", out)
	}
}

func TestCLIPrintUnregistered(t *testing.T) {
	paper := filepath.Join(t.TempDir(), "lp0")
	c, _, e := exec(t, saleJob, "print", "--dir", t.TempDir(), "--printer", "file:"+paper, "-")
	if c != 0 || !strings.Contains(e, "not registered") {
		t.Fatalf("%d %q", c, e)
	}
}
