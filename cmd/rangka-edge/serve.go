package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rangka-os/rangka-edge/internal/printsvc"
	"github.com/rangka-os/rangka-edge/internal/queue"
)

// listFlag is a repeatable flag (--printer kasir=tcp://... --printer dapur=tcp://...).
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// serve is the outlet's one long-running process (draft contract 0045 §1): it owns the queue, pushes it to the kernel
// like `run` when the device is registered, and takes print jobs from the cashier screen on the same device at
// http://127.0.0.1:9180/v1/print. A job is spooled durably before the answer (202) and printed by a worker; each try
// is queued as print.done or print.failed. An unregistered Edge (before pairing) still prints, without events.
// Printers and allowed origins come from <dir>/printers.json and the flags; the print token from <dir>/print-token
// (made on first start, mode 0600), which the screen sends as Authorization: Bearer.
func (c *cli) serve() func([]string) int {
	addr := c.fs.String("listen", printsvc.DefaultAddr, "listen address (loopback only)")
	var printers, routes, origins listFlag
	c.fs.Var(&printers, "printer", "name=address, e.g. kasir=tcp://192.168.1.50:9100 (repeat)")
	c.fs.Var(&routes, "route", "layout=printer, e.g. kitchen=dapur (repeat)")
	c.fs.Var(&origins, "origin", "site origin allowed to print, e.g. https://tahu-contoh.juragan.id (repeat)")
	cols := c.fs.Int("cols", 0, "characters per line: 48 for 80 mm (default), 32 for 58 mm")
	return func([]string) int {
		if err := loopbackOnly(*addr); err != nil {
			return c.usage("--listen: %v", err)
		}
		dir, err := dataDir(*c.dirFlag)
		if err != nil {
			return c.fail("%v", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return c.fail("%v", err)
		}
		cfg, err := loadPrintConfig(dir)
		if err != nil {
			return c.fail("%v", err)
		}
		if err := mergePrintFlags(&cfg, printers, routes, origins, *cols); err != nil {
			return c.usage("%v", err)
		}
		if len(cfg.Printers) == 0 {
			return c.usage("no printer: add --printer name=tcp://host:9100 or %s", filepath.Join(dir, "printers.json"))
		}
		token, err := printToken(dir)
		if err != nil {
			return c.fail("%v", err)
		}
		logf := func(f string, a ...any) { fmt.Fprintf(c.stderr, time.Now().Format(time.RFC3339)+" "+f+"\n", a...) }
		svc, err := printsvc.New(filepath.Join(dir, "spool"), cfg, token)
		if err != nil {
			return c.fail("%v", err)
		}
		svc.Logf = logf

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		deviceID := ""
		d, q, err := c.openRegistered(dir)
		switch {
		case err == nil:
			defer q.Close()
			deviceID = d.DeviceID
			svc.Events = func(ev printsvc.Event) {
				b, _ := json.Marshal(ev.Payload)
				if _, _, err := q.Enqueue(queue.NewItem{Key: d.DeviceID + ":" + ev.Key, Kind: ev.Kind, OccurredAt: ev.At, Payload: b}); err != nil {
					logf("print event not queued: %v", err)
				}
			}
			go func() {
				if err := newRunner(d, q, logf).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logf("sync stopped: %v", err)
				}
			}()
			fmt.Fprintf(c.stderr, "device %s: syncing with %s\n", d.DeviceID, d.Kernel)
		case errors.Is(err, errNotRegistered):
			fmt.Fprintln(c.stderr, "device not registered: printing without print events or sync")
		default:
			return c.fail("%v", err)
		}

		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return c.fail("%v", err)
		}
		srv := &http.Server{Handler: svc.Handler(Version, deviceID), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
		go svc.Run(ctx)
		go func() {
			<-ctx.Done()
			sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			srv.Shutdown(sc)
		}()
		fmt.Fprintf(c.stdout, "print endpoint on http://%s (token in %s)\n", ln.Addr(), filepath.Join(dir, "print-token"))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return c.fail("%v", err)
		}
		return 0
	}
}

// openRegistered loads the device and opens its queue (held by this process until it stops).
func (c *cli) openRegistered(dir string) (device, *queue.Queue, error) {
	d, err := loadDevice(dir)
	if err != nil {
		return d, nil, err
	}
	q, err := openQueue(dir, d)
	return d, q, err
}

// loopbackOnly refuses a listen address that is not 127.0.0.1, ::1 or localhost: the endpoint prints for the screen
// on this device, never for the network.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%q is not a loopback address", addr)
}

// loadPrintConfig reads <dir>/printers.json; a missing file is an empty config.
func loadPrintConfig(dir string) (printsvc.Config, error) {
	var cfg printsvc.Config
	b, err := os.ReadFile(filepath.Join(dir, "printers.json"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("printers.json: %v", err)
	}
	return cfg, nil
}

func mergePrintFlags(cfg *printsvc.Config, printers, routes, origins []string, cols int) error {
	if cfg.Printers == nil {
		cfg.Printers = map[string]string{}
	}
	if cfg.Layouts == nil {
		cfg.Layouts = map[string]string{}
	}
	for _, p := range printers {
		n, a, ok := strings.Cut(p, "=")
		if !ok || n == "" || a == "" {
			return fmt.Errorf("--printer %q: want name=address", p)
		}
		cfg.Printers[n] = a
	}
	for _, r := range routes {
		l, n, ok := strings.Cut(r, "=")
		if !ok || (l != "receipt" && l != "kitchen") || n == "" {
			return fmt.Errorf("--route %q: want receipt=<printer> or kitchen=<printer>", r)
		}
		cfg.Layouts[l] = n
	}
	for _, o := range origins {
		cfg.Origins = append(cfg.Origins, strings.TrimRight(o, "/"))
	}
	for _, o := range cfg.Origins {
		if err := checkOrigin(o); err != nil {
			return err
		}
	}
	for l, n := range cfg.Layouts {
		if _, ok := cfg.Printers[n]; !ok {
			return fmt.Errorf("layout %s goes to printer %q, which is not configured", l, n)
		}
	}
	if cols != 0 {
		cfg.Cols = cols
	}
	return nil
}

// printToken reads <dir>/print-token, making a random one (0600) on first start.
func printToken(dir string) (string, error) {
	p := filepath.Join(dir, "print-token")
	if b, err := os.ReadFile(p); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.WriteFile(p, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// checkOrigin accepts https://<host>[:port], or http on exactly localhost, 127.0.0.1 or [::1], with nothing after the
// host: a browser sends an Origin in that form, and a prefix match would let http://localhost.evil.example in.
func checkOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return fmt.Errorf("origin %q: want https://<site>", o)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
	}
	return fmt.Errorf("origin %q: want https://<site> (http only for localhost)", o)
}
