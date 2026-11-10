package transport

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTCPPrinterSendsWholeJob(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		b, _ := io.ReadAll(c)
		c.Close()
		got <- b
	}()
	p, err := OpenPrinter("tcp://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	job := make([]byte, 200000) // bigger than one socket buffer
	for i := range job {
		job[i] = byte(i)
	}
	if err := p.Print(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if len(b) != len(job) || b[12345] != job[12345] {
			t.Fatalf("got %d bytes", len(b))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("printer got nothing")
	}
}

func TestTCPPrinterUnreachable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	p, _ := OpenPrinter("tcp://" + addr)
	if err := p.Print(context.Background(), []byte("x")); err == nil {
		t.Fatal("a closed port printed")
	}
}

func TestFilePrinterAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lp0")
	p, err := OpenPrinter("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Print(context.Background(), []byte("a"))
	_ = p.Print(context.Background(), []byte("b"))
	b, _ := os.ReadFile(path)
	if string(b) != "ab" {
		t.Fatalf("%q", b)
	}
}

func TestOpenPrinterAddresses(t *testing.T) {
	p, err := OpenPrinter("tcp://192.168.1.50")
	if err != nil || p.String() != "tcp://192.168.1.50:9100" {
		t.Fatalf("default port: %v %v", p, err)
	}
	if p, err := OpenPrinter("file:///dev/usb/lp0"); err != nil || p.String() != "file:/dev/usb/lp0" {
		t.Fatalf("file url: %v %v", p, err)
	}
	for _, bad := range []string{"", "192.168.1.50", "usb://x", "tcp://", "file:"} {
		if _, err := OpenPrinter(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
