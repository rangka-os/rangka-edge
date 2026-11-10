package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rangka-os/rangka-edge/internal/escpos"
	"github.com/rangka-os/rangka-edge/internal/printdoc"
	"github.com/rangka-os/rangka-edge/internal/queue"
	"github.com/rangka-os/rangka-edge/internal/transport"
)

// printPayload is the payload of the print.done / print.failed event (contract 0006 §3) a print leaves in the queue.
type printPayload struct {
	Model   string `json:"model"`
	Doc     string `json:"doc"`
	Layout  string `json:"layout"`
	Printer string `json:"printer"`
	Copy    bool   `json:"copy,omitempty"`
	Bytes   int    `json:"bytes"`
	Error   string `json:"error,omitempty"`
}

// print lays out a record (receipt or kitchen ticket) as ESC/POS and sends it to a printer. On a registered device the
// result is queued as print.done or print.failed, keyed <device>:print:<job>, so head office sees what was printed
// once edge-sync reaches the kernel. Printing never waits for the network.
func (c *cli) print() func([]string) int {
	printer := c.fs.String("printer", os.Getenv("RANGKA_EDGE_PRINTER"), "tcp://host[:9100] or file:/path (default $RANGKA_EDGE_PRINTER)")
	layout := c.fs.String("layout", "", "receipt or kitchen (default: the job's layout, else receipt)")
	cols := c.fs.Int("cols", escpos.Cols80, "characters per line: 48 for 80 mm, 32 for 58 mm")
	station := c.fs.String("station", "", "kitchen: station name printed on top (default DAPUR)")
	outlet := c.fs.String("outlet-name", "", "outlet name printed under the header")
	copyFlag := c.fs.Bool("copy", false, "a reprint: prints SALINAN")
	drawer := c.fs.Bool("drawer", false, "receipt: open the cash drawer after printing")
	jobID := c.fs.String("job", "", "job id for the event key (default: layout, record and print time)")
	preview := c.fs.Bool("preview", false, "print the text the paper would show to stdout instead of sending it")
	return func(args []string) int {
		if len(args) != 1 {
			return c.usage("give one job file, or - for stdin")
		}
		raw, err := readArg(args[0], c.stdin)
		if err != nil {
			return c.fail("%v", err)
		}
		job, err := printdoc.ParseJob(raw)
		if err != nil {
			return c.fail("%v", err)
		}
		if *layout != "" {
			job.Layout = *layout
		}
		if job.Layout == "" {
			job.Layout = "receipt"
		}
		if *station != "" {
			job.Station = *station
		}
		if *outlet != "" {
			job.OutletName = *outlet
		}
		job.Copy = job.Copy || *copyFlag
		job.Drawer = job.Drawer || *drawer
		now := time.Now()
		if job.PrintedAt == "" {
			job.PrintedAt = now.Format(time.RFC3339)
		}
		out, err := printdoc.Render(job, *cols)
		if err != nil {
			return c.usage("%v", err)
		}
		if *preview {
			fmt.Fprint(c.stdout, escpos.Decode(out))
			return 0
		}
		if *printer == "" {
			return c.usage("--printer is required (or set RANGKA_EDGE_PRINTER), or use --preview")
		}
		p, err := transport.OpenPrinter(*printer)
		if err != nil {
			return c.usage("%v", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		perr := p.Print(ctx, out)

		doc := fmt.Sprint(job.Doc["name"])
		id := *jobID
		if id == "" {
			id = job.Layout + ":" + doc + ":" + now.UTC().Format("20060102T150405.000000000Z")
		}
		pl := printPayload{Model: "pos_sale", Doc: doc, Layout: job.Layout, Printer: p.String(), Copy: job.Copy, Bytes: len(out)}
		kind := "print.done"
		if perr != nil {
			kind, pl.Error = "print.failed", perr.Error()
		}
		c.queuePrint(kind, "print:"+id, pl, now)
		if perr != nil {
			return c.fail("%v", perr)
		}
		fmt.Fprintf(c.stdout, "printed %s %s to %s (%d bytes)\n", job.Layout, doc, p, len(out))
		return 0
	}
}

// queuePrint records the print event when this device is registered; an unregistered Edge (a printer test before
// pairing) or a queue held by `rangka-edge run` only gets a note on stderr, the paper is what counts.
func (c *cli) queuePrint(kind, key string, pl printPayload, at time.Time) {
	_, d, q, err := c.open()
	if err != nil {
		if errors.Is(err, errNotRegistered) {
			fmt.Fprintln(c.stderr, "note: device not registered, print event not queued")
		} else {
			fmt.Fprintf(c.stderr, "note: print event not queued: %v\n", err)
		}
		return
	}
	defer q.Close()
	b, _ := json.Marshal(pl)
	it, _, err := q.Enqueue(queue.NewItem{Key: d.DeviceID + ":" + key, Kind: kind, OccurredAt: at.UTC(), Payload: b})
	if err != nil {
		fmt.Fprintf(c.stderr, "note: print event not queued: %v\n", err)
		return
	}
	fmt.Fprintf(c.stderr, "queued %s %s\n", kind, it.Key)
}

func readArg(arg string, stdin io.Reader) ([]byte, error) {
	if arg == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(arg)
}

// printerSim listens like a network printer (raw TCP 9100) and shows each job as the paper would read. For demos and
// tests without hardware; --out keeps every job's bytes (.bin) and text (.txt).
func (c *cli) printerSim() func([]string) int {
	addr := c.fs.String("listen", "127.0.0.1:9100", "listen address")
	outDir := c.fs.String("out", "", "directory to keep each job (.bin and .txt)")
	once := c.fs.Bool("once", false, "exit after the first job")
	return func([]string) int {
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return c.fail("%v", err)
		}
		if *outDir != "" {
			if err := os.MkdirAll(*outDir, 0o755); err != nil {
				return c.fail("%v", err)
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		go func() { <-ctx.Done(); ln.Close() }()
		fmt.Fprintf(c.stdout, "printer simulator on tcp://%s\n", ln.Addr())
		var mu sync.Mutex
		n := 0
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return 0
				}
				return c.fail("%v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			b, err := io.ReadAll(conn)
			conn.Close()
			if err != nil && len(b) == 0 {
				fmt.Fprintf(c.stderr, "job read failed: %v\n", err)
				continue
			}
			mu.Lock()
			n++
			text := escpos.Decode(b)
			fmt.Fprintf(c.stdout, "--- job %d (%d bytes) ---\n%s", n, len(b), text)
			if !strings.HasSuffix(text, "\n") {
				fmt.Fprintln(c.stdout)
			}
			if *outDir != "" {
				base := filepath.Join(*outDir, fmt.Sprintf("job-%03d", n))
				_ = os.WriteFile(base+".bin", b, 0o644)
				_ = os.WriteFile(base+".txt", []byte(text), 0o644)
			}
			mu.Unlock()
			if *once {
				return 0
			}
		}
	}
}
