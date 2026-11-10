package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Printer sends one print job (raw ESC/POS bytes) to a device.
type Printer interface {
	Print(ctx context.Context, job []byte) error
	String() string
}

// OpenPrinter parses a printer address:
//   - tcp://host[:port]  a network printer's raw port (default 9100, the port Epson, Xprinter and most LAN printers
//     listen on);
//   - file:/dev/usb/lp0, file:///dev/cu.usbserial-1  a USB or serial printer the OS exposes as a device file, or any
//     file (tests, archiving the bytes).
func OpenPrinter(addr string) (Printer, error) {
	u, err := url.Parse(addr)
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("printer %q: want tcp://host[:9100] or file:/path", addr)
	}
	switch u.Scheme {
	case "tcp":
		host := u.Host
		if host == "" {
			return nil, fmt.Errorf("printer %q: no host", addr)
		}
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, "9100")
		}
		return &tcpPrinter{addr: host, timeout: 10 * time.Second}, nil
	case "file":
		p := u.Path
		if p == "" {
			p = u.Opaque
		}
		if p == "" {
			return nil, fmt.Errorf("printer %q: no path", addr)
		}
		return &filePrinter{path: p}, nil
	}
	return nil, fmt.Errorf("printer %q: unknown scheme %q (tcp, file)", addr, u.Scheme)
}

type tcpPrinter struct {
	addr    string
	timeout time.Duration
}

func (p *tcpPrinter) String() string { return "tcp://" + p.addr }

// Print opens one connection per job, writes the whole job and closes: a printer that drops the link mid-job fails
// the job (the caller records print.failed and may retry the same job; a receipt printed twice says SALINAN only when
// the caller marks it a copy).
func (p *tcpPrinter) Print(ctx context.Context, job []byte) error {
	d := net.Dialer{Timeout: p.timeout}
	conn, err := d.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return fmt.Errorf("printer %s unreachable: %w", p.addr, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(p.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetWriteDeadline(deadline)
	for len(job) > 0 {
		n, err := conn.Write(job)
		if err != nil {
			return fmt.Errorf("printer %s: %w", p.addr, err)
		}
		job = job[n:]
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("printer %s: %w", p.addr, err)
		}
	}
	return nil
}

type filePrinter struct{ path string }

func (p *filePrinter) String() string { return "file:" + p.path }

// Print appends the job to the file (a device file takes it as a stream; a plain file collects jobs).
func (p *filePrinter) Print(_ context.Context, job []byte) error {
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("printer %s: %w", p.path, err)
	}
	if _, err := f.Write(job); err != nil {
		f.Close()
		return fmt.Errorf("printer %s: %w", p.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("printer %s: %w", p.path, err)
	}
	return nil
}

// IsAddr reports whether s looks like a printer address OpenPrinter takes.
func IsAddr(s string) bool { return strings.HasPrefix(s, "tcp://") || strings.HasPrefix(s, "file:") }
