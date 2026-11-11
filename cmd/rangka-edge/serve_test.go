package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeUsage(t *testing.T) {
	dir := t.TempDir()
	if c, _, e := exec(t, "", "serve", "--dir", dir, "--listen", "0.0.0.0:9180", "--printer", "kasir=tcp://127.0.0.1:1"); c != 2 || !strings.Contains(e, "loopback") {
		t.Fatalf("non-loopback: %d %s", c, e)
	}
	if c, _, e := exec(t, "", "serve", "--dir", dir, "--listen", "127.0.0.1:0"); c != 2 || !strings.Contains(e, "no printer") {
		t.Fatalf("no printer: %d %s", c, e)
	}
	if c, _, e := exec(t, "", "serve", "--dir", dir, "--listen", "127.0.0.1:0", "--printer", "kasir=tcp://127.0.0.1:1", "--route", "kitchen=dapur"); c != 2 || !strings.Contains(e, "dapur") {
		t.Fatalf("route to a missing printer: %d %s", c, e)
	}
	if c, _, e := exec(t, "", "serve", "--dir", dir, "--listen", "127.0.0.1:0", "--printer", "kasir=tcp://127.0.0.1:1", "--origin", "http://evil.example"); c != 2 || !strings.Contains(e, "origin") {
		t.Fatalf("http origin: %d %s", c, e)
	}
}

// TestServePrints runs `serve` (unregistered device) against a fake network printer and prints one receipt twice by
// the same job id: one paper.
func TestServePrints(t *testing.T) {
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	jobs := make(chan []byte, 4)
	go func() {
		for {
			conn, err := pl.Accept()
			if err != nil {
				return
			}
			b, _ := io.ReadAll(conn)
			conn.Close()
			jobs <- b
		}
	}()
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	dir := t.TempDir()
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() {
		done <- run([]string{"serve", "--dir", dir, "--listen", addr, "--printer", "kasir=tcp://" + pl.Addr().String()},
			strings.NewReader(""), io.Discard, &errb)
	}()
	var tok []byte
	for i := 0; i < 100; i++ {
		if tok, err = os.ReadFile(filepath.Join(dir, "print-token")); err == nil {
			if c, err := net.Dial("tcp", addr); err == nil {
				c.Close()
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st, err := os.Stat(filepath.Join(dir, "print-token")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("print token: %v %v", st, err)
	}
	body := fmt.Sprintf(`{"job_id": "ob-1:receipt", "layout": "receipt", "record": %s}`, strings.ReplaceAll(saleJob, "\n", ""))
	post := func() int {
		r, _ := http.NewRequest("POST", "http://"+addr+"/v1/print", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(); c != 202 {
		t.Fatalf("print: %d %s", c, errb.String())
	}
	select {
	case b := <-jobs:
		if !bytes.Contains(b, []byte("STRK-DGO-2611-00001")) {
			t.Fatalf("paper: %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("nothing printed: %s", errb.String())
	}
	if c := post(); c != 200 {
		t.Fatalf("resend: %d", c)
	}
	select {
	case b := <-jobs:
		t.Fatalf("printed twice: %q", b)
	case <-time.After(1500 * time.Millisecond):
	}
	// the serve goroutine ends with the test binary (it stops on SIGINT/SIGTERM only)
	_ = done
}
