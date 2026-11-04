// Command rangka-edge is the Rangka agent that runs at the outlet: it executes device connectors
// (printer, barcode scanner, cash drawer, RFID), keeps a local queue while offline and syncs with the
// Rangka kernel. See rangka-os/docs/contracts/0003-edge-and-devices.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rangka-os/rangka-edge/internal/edgesync"
	"github.com/rangka-os/rangka-edge/internal/edgesync/kernelstub"
	"github.com/rangka-os/rangka-edge/internal/queue"
	"github.com/rangka-os/rangka-edge/internal/runner"
)

// Version is set at build time with -ldflags "-X main.Version=…".
var Version = "0.0.0-dev"

const usageText = `rangka-edge: Rangka agent for the outlet

Usage:
  rangka-edge register --kernel <url> --code <pairing-code> [--name <name>] [--force]
  rangka-edge enqueue --kind <kind> [--key <key>] [--actor <a>] [--occurred-at <RFC3339>] --payload <json|->
  rangka-edge status [--json]
  rangka-edge flush                 push the queue once
  rangka-edge run                   push the queue until stopped (SIGINT, SIGTERM)
  rangka-edge dev-kernel [--addr 127.0.0.1:8787] --code <code>   stub kernel for manual tests
  rangka-edge devices               not implemented yet
  rangka-edge version

Every command except version and devices takes --dir <path>
(default: $RANGKA_EDGE_DIR, else the user config dir + /rangka-edge).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes one command and returns the exit code: 0 ok, 1 runtime error, 2 usage error.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "devices":
		fmt.Fprintln(stderr, "rangka-edge devices: not implemented yet")
		return 1
	case "register", "enqueue", "status", "flush", "run", "dev-kernel":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usageText)
		return 2
	}

	fs := flag.NewFlagSet("rangka-edge "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dirFlag := fs.String("dir", "", "data directory")
	c := &cli{stdin: stdin, stdout: stdout, stderr: stderr, fs: fs, dirFlag: dirFlag}
	var do func(rest []string) int
	switch cmd {
	case "register":
		do = c.register()
	case "enqueue":
		do = c.enqueue()
	case "status":
		do = c.status()
	case "flush":
		do = c.flush()
	case "run":
		do = c.run()
	case "dev-kernel":
		do = c.devKernel()
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return do(fs.Args())
}

type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	fs             *flag.FlagSet
	dirFlag        *string
}

func (c *cli) fail(format string, a ...any) int {
	fmt.Fprintf(c.stderr, "rangka-edge %s: %s\n", c.fs.Name()[len("rangka-edge "):], fmt.Sprintf(format, a...))
	return 1
}

func (c *cli) usage(format string, a ...any) int {
	fmt.Fprintf(c.stderr, "rangka-edge %s: %s\n", c.fs.Name()[len("rangka-edge "):], fmt.Sprintf(format, a...))
	return 2
}

// open loads device.json and opens the queue. The caller closes the queue.
func (c *cli) open() (string, device, *queue.Queue, error) {
	dir, err := dataDir(*c.dirFlag)
	if err != nil {
		return "", device{}, nil, err
	}
	d, err := loadDevice(dir)
	if err != nil {
		return dir, d, nil, err
	}
	q, err := openQueue(dir, d)
	return dir, d, q, err
}

func (c *cli) register() func([]string) int {
	kernel := c.fs.String("kernel", "", "kernel base URL")
	code := c.fs.String("code", "", "pairing code")
	name := c.fs.String("name", "", "device name (default: hostname)")
	force := c.fs.Bool("force", false, "replace an existing registration")
	return func([]string) int {
		if *kernel == "" || *code == "" {
			return c.usage("--kernel and --code are required")
		}
		dir, err := dataDir(*c.dirFlag)
		if err != nil {
			return c.fail("%v", err)
		}
		if _, err := os.Stat(devicePath(dir)); err == nil && !*force {
			return c.fail("already registered (%s exists); use --force to replace it", devicePath(dir))
		}
		devName := *name
		if devName == "" {
			devName, _ = os.Hostname()
		}
		cl := &edgesync.Client{BaseURL: strings.TrimRight(*kernel, "/")}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		resp, err := cl.Register(ctx, *code, edgesync.DeviceInfo{Name: devName, Platform: runtime.GOOS + "/" + runtime.GOARCH, Version: Version})
		if err != nil {
			return c.fail("%v", err)
		}
		d := device{Kernel: cl.BaseURL, DeviceID: resp.DeviceID, Token: resp.Token, Site: resp.Site, Org: resp.Org,
			Outlet: resp.Outlet, PushMaxItems: resp.Sync.PushMaxItems}
		if err := saveDevice(dir, d); err != nil {
			return c.fail("%v", err)
		}
		fmt.Fprintf(c.stdout, "registered %s (outlet %s, site %s)\n", d.DeviceID, d.Outlet, d.Site)
		return 0
	}
}

func (c *cli) enqueue() func([]string) int {
	kind := c.fs.String("kind", "", "event kind")
	key := c.fs.String("key", "", "idempotency key (default: generated)")
	actor := c.fs.String("actor", "", "who did it")
	occ := c.fs.String("occurred-at", "", "RFC3339 time (default: now)")
	payload := c.fs.String("payload", "", "JSON object, or - to read stdin")
	return func([]string) int {
		if *kind == "" || *payload == "" {
			return c.usage("--kind and --payload are required")
		}
		if !slices.Contains(edgesync.Kinds, *kind) {
			return c.fail("unknown kind %q (known: %s)", *kind, strings.Join(edgesync.Kinds, ", "))
		}
		at := time.Now().UTC()
		if *occ != "" {
			t, err := time.Parse(time.RFC3339, *occ)
			if err != nil {
				return c.usage("--occurred-at: %v", err)
			}
			at = t
		}
		raw := []byte(*payload)
		if *payload == "-" {
			b, err := io.ReadAll(c.stdin)
			if err != nil {
				return c.fail("reading stdin: %v", err)
			}
			raw = b
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return c.fail("payload must be a JSON object")
		}
		_, _, q, err := c.open()
		if err != nil {
			return c.fail("%v", err)
		}
		defer q.Close()
		it, added, err := q.Enqueue(queue.NewItem{Key: *key, Kind: *kind, Actor: *actor, OccurredAt: at, Payload: raw})
		if err != nil {
			return c.fail("%v", err)
		}
		if added {
			fmt.Fprintf(c.stdout, "queued %s\n", it.Key)
		} else {
			fmt.Fprintf(c.stdout, "already queued %s\n", it.Key)
		}
		return 0
	}
}

type deadJSON struct {
	Key     string `json:"key"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type statusOut struct {
	DeviceID       string     `json:"device_id"`
	Kernel         string     `json:"kernel"`
	Outlet         string     `json:"outlet"`
	Pending        int        `json:"pending"`
	Dead           int        `json:"dead"`
	Done           int        `json:"done"`
	OldestPendingA string     `json:"oldest_pending_at,omitempty"`
	LastSeq        int64      `json:"last_seq"`
	DeadItems      []deadJSON `json:"dead_items"`
}

func (c *cli) status() func([]string) int {
	asJSON := c.fs.Bool("json", false, "print one JSON object")
	return func([]string) int {
		_, d, q, err := c.open()
		if err != nil {
			return c.fail("%v", err)
		}
		defer q.Close()
		s := q.Stats()
		out := statusOut{DeviceID: d.DeviceID, Kernel: d.Kernel, Outlet: d.Outlet, Pending: s.Pending, Dead: s.Dead,
			Done: s.Done, LastSeq: s.LastSeq, DeadItems: []deadJSON{}}
		if !s.OldestPendingAt.IsZero() {
			out.OldestPendingA = s.OldestPendingAt.UTC().Format(time.RFC3339)
		}
		for _, it := range q.DeadItems() {
			out.DeadItems = append(out.DeadItems, deadJSON{it.Key, it.Code, it.Message})
		}
		if *asJSON {
			b, _ := json.Marshal(out)
			fmt.Fprintln(c.stdout, string(b))
			return 0
		}
		oldest := "none"
		if out.OldestPendingA != "" {
			oldest = out.OldestPendingA
		}
		fmt.Fprintf(c.stdout, "device:  %s\nkernel:  %s\noutlet:  %s\npending: %d\ndead:    %d\ndone:    %d\noldest pending: %s\nlast seq: %d\n",
			out.DeviceID, out.Kernel, out.Outlet, out.Pending, out.Dead, out.Done, oldest, out.LastSeq)
		for _, di := range out.DeadItems {
			fmt.Fprintf(c.stdout, "dead: %s %s %s\n", di.Key, di.Code, di.Message)
		}
		return 0
	}
}

func newRunner(d device, q *queue.Queue, logf func(string, ...any)) *runner.Runner {
	return &runner.Runner{
		Queue:     q,
		Pusher:    &edgesync.Client{BaseURL: d.Kernel, Token: d.Token},
		BatchSize: d.PushMaxItems,
		Logf:      logf,
	}
}

func (c *cli) flush() func([]string) int {
	return func([]string) int {
		_, d, q, err := c.open()
		if err != nil {
			return c.fail("%v", err)
		}
		defer q.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		rep, err := newRunner(d, q, nil).Flush(ctx)
		if err != nil {
			return c.fail("%v", err)
		}
		fmt.Fprintf(c.stdout, "pushed %d, duplicates %d, rejected %d\n", rep.Pushed, rep.Duplicates, rep.Rejected)
		return 0
	}
}

func (c *cli) run() func([]string) int {
	return func([]string) int {
		_, d, q, err := c.open()
		if err != nil {
			return c.fail("%v", err)
		}
		defer q.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		logf := func(f string, a ...any) { fmt.Fprintf(c.stderr, time.Now().Format(time.RFC3339)+" "+f+"\n", a...) }
		fmt.Fprintf(c.stderr, "running as %s against %s\n", d.DeviceID, d.Kernel)
		err = newRunner(d, q, logf).Run(ctx)
		if err == nil || errors.Is(err, context.Canceled) {
			return 0
		}
		return c.fail("%v", err)
	}
}

func (c *cli) devKernel() func([]string) int {
	addr := c.fs.String("addr", "127.0.0.1:8787", "listen address")
	code := c.fs.String("code", "", "pairing code to accept")
	return func([]string) int {
		if *code == "" {
			return c.usage("--code is required")
		}
		stub := kernelstub.New()
		stub.AddPairingCode(*code, "dev-outlet")
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return c.fail("%v", err)
		}
		srv := &http.Server{Handler: stub.Handler(), ReadHeaderTimeout: 10 * time.Second}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		fmt.Fprintf(c.stdout, "dev kernel on http://%s (pairing code %s)\n", ln.Addr(), *code)
		go func() {
			<-ctx.Done()
			sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			srv.Shutdown(sc)
		}()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return c.fail("%v", err)
		}
		return 0
	}
}
