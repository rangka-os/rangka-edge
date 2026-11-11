# Rangka Edge

**The Rangka agent that runs at the outlet.** It runs the device connectors that the kernel can't run from a server:
receipt printers, barcode scanners, cash drawers, scales and RFID readers. It keeps working when the internet is down,
and syncs with the Rangka kernel when the connection comes back.

> **Status: first cut (November 2026).** The durable queue, `edge-sync/v1` push and the CLI work end to end
> against a stub kernel. Printing works without the kernel: `print` lays out a cashier receipt or a kitchen ticket as
> ESC/POS and sends it to a network (raw TCP 9100) or USB/serial printer, and `printer-sim` stands in for a printer.
> Device connectors (`via: device`) and printing driven by the kernel wait for edge-sync in the kernel. The design is in
> [rangka-os/docs/contracts/0003-edge-and-devices.md](https://github.com/rangka-os/rangka-os/blob/main/docs/contracts/0003-edge-and-devices.md).

Part of the Rangka family: [rangka-os](https://github.com/rangka-os/rangka-os) (kernel) ·
[rangka-apps](https://github.com/rangka-os/rangka-apps) · [rangka-connectors](https://github.com/rangka-os/rangka-connectors) ·
**rangka-edge**

## How it fits

```
 rangka-connectors   escpos_printer/connector.rk   (via: device)   ← what the device does
        │ installed on a site
 rangka-os kernel    sends the site's device connectors to each paired Edge
        │ edge-sync/v1
 rangka-edge         runs them against local hardware, queues events offline, syncs back
        │ USB · serial · Bluetooth · network
 printer · scanner · cash drawer · RFID
```

Modules in rangka-apps decide what a device event means. The same `rfid.tag_read` can be attendance in HR, stock
movement in a warehouse, or check-in at a clinic.

## Layout

| Path | What |
| --- | --- |
| `cmd/rangka-edge` | The binary: `register`, `enqueue`, `status`, `flush`, `run`, `serve`, `dev-kernel`, `print`, `printer-sim`, `devices`, `version` |
| `internal/printsvc` | The local print endpoint of `serve`: durable spool, print worker, `/v1/print` with token and CORS |
| `internal/escpos` | ESC/POS builder (80 mm = 48 columns, 58 mm = 32) and a decoder that shows a job as the paper reads |
| `internal/printdoc` | Layouts: `receipt` (same order and words as module pos's `pos_receipt` print format) and `kitchen` (no prices, big item lines) |
| `internal/transport` | Generic hardware transports; printers today: `tcp://host[:9100]` and `file:/dev/...` |
| `internal/runner` | Pushes the queue to the kernel with backoff (device connectors come later) |
| `internal/queue` | Durable local queue with idempotency keys |
| `internal/edgesync` | The `edge-sync/v1` client and types, plus `kernelstub`, a fake kernel for tests |

Shared types (the `.rk` reader, connector schema, CEL) come from `github.com/rangka-os/rangka-os/kernel/pkg/...` at a
pinned `rangka-os` release. They are moved out of `kernel/internal` as the first step of 0003.

## Commands

A local walkthrough with the stub kernel (use any data dir with `--dir`, or set `RANGKA_EDGE_DIR`):

```bash
rangka-edge dev-kernel --code ABCD2345 &                       # stub kernel on 127.0.0.1:8787
rangka-edge register --kernel http://127.0.0.1:8787 --code ABCD2345
rangka-edge enqueue --kind sale.recorded --key s-1 --payload '{"total":25000}'
rangka-edge status                                             # pending 1
rangka-edge flush                                              # pushed 1, duplicates 0, rejected 0
```

`rangka-edge run` keeps pushing until SIGINT or SIGTERM. `status --json` prints one JSON object for scripts.
`status` works while `run` is active; `enqueue` and `flush` are refused then ("queue is in use by another rangka-edge process (stop `rangka-edge run` first)").

### Printing

```bash
rangka-edge printer-sim --listen 127.0.0.1:9100 &              # shows each job as text
curl -s -H "Authorization: Bearer $TOKEN" $KERNEL/api/v1/r/pos_sale/STRK-DGO-2611-00001 > sale.json
rangka-edge print --printer tcp://127.0.0.1:9100 --outlet-name "Outlet Dago" --drawer sale.json
rangka-edge print --printer tcp://127.0.0.1:9100 --layout kitchen --station Dapur sale.json
rangka-edge print --preview sale.json                           # no printer: the text on stdout
```

A job file is the pos_sale record (bare, or the kernel's `{"record": ...}`), or `{"doc": record, "header": {name,
address, phone, npwp, footer}, "outlet_name", "layout", "station", "copy", "drawer"}`. Text goes out as plain ASCII
(accents folded), because printers differ in their default code page. On a registered Edge every print queues
`print.done` or `print.failed` (key `<device>:print:<job>:done|failed`, payload: record, layout, printer, bytes, error), so head
office sees it once the kernel speaks edge-sync; `--job` makes a retry the same event. While `run` holds the queue the
print still goes out and the event is not queued (a note on stderr); `serve` owns both.

### Local print endpoint (`serve`)

`rangka-edge serve` is the outlet's one long-running process (draft contract 0045 §1 in Juragan's proposals, for
rangka-os): it holds the queue and pushes it like `run` when the device is registered, and takes print jobs from the
cashier screen on the same device.

```bash
rangka-edge serve --printer kasir=tcp://192.168.1.50:9100 --printer dapur=tcp://192.168.1.51:9100 \
  --route receipt=kasir --route kitchen=dapur --origin https://tahu-contoh.juragan.id
curl -s -H "Authorization: Bearer $(cat "$RANGKA_EDGE_DIR/print-token")" -d @job.json http://127.0.0.1:9180/v1/print
```

- `POST /v1/print {job_id, model, name, layout, printer?, record, copy?, drawer?}`: `202 {state: queued}` once the job
  is written to `<dir>/spool/queued/` (fsync, rename); a worker prints it on the printer of its layout (or the one it
  names, or the only one). `job_id` is the screen's outbox key + layout: a resend answers with the job's state
  (`200 done`, `202 queued`) and prints nothing. A failed try waits (2 s, 4 s, ... 64 s) without holding up the other
  printer's jobs; after 5 tries the job moves to `spool/failed/`. Every try queues `print.done` or `print.failed`
  (key `<device>:print:<job_id>:<try>:done|failed`) on a registered device.
- A job that gave up is tried again when the screen sends the same `job_id` again (5 more tries). Printing is at
  least once: a stop after the paper came out but before the job is marked done prints it again on restart. `done`
  means the bytes reached the printer's socket (raw 9100 has no acknowledgement), not that the paper came out.
- The cash drawer opens only for a paid receipt (`docstatus` 1) that is not a copy, on its first try; the event's
  `drawer` says whether it opened.
- An Edge that is not registered yet (before pairing) prints without events and never opens the cash drawer: nothing
  would show the kernel who opened it.
- `GET /v1/health` (printers, layouts, device) and `GET /v1/jobs/<job_id>` (state, tries, error).
- Safety: loopback only (`--listen` refuses other addresses), `Authorization: Bearer <print-token>` (made on first
  start in `<dir>/print-token`, mode 0600), and a browser `Origin` must be exactly one of `--origin` (https, or http
  on localhost, 127.0.0.1 or [::1]); preflights answer CORS and Chrome's private network access. The printer map lives on the Edge
  (`<dir>/printers.json` with `printers`, `layouts`, `origins`, `cols`, plus the flags), never in `.rk` files.
- Open for Dede (0045): the screen talking to localhost versus the kernel relaying print jobs through edge-sync.

The queue is an append-only `queue.log` in `<dir>/queue/`: one CRC-checked JSON record per line, fsync per write.
A torn last line (power cut) is truncated on open. The protocol types live in `internal/edgesync` until rangka-os
ships `kernel/pkg/edgesync` at a tagged release.

## Build

```bash
go build -o bin/rangka-edge ./cmd/rangka-edge
./bin/rangka-edge version
```

## Licence

Elastic License 2.0 (ELv2), see `LICENSE`. Copyright 2026 Saga Tekno Studio.
