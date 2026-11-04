# Rangka Edge

**The Rangka agent that runs at the outlet.** It runs the device connectors that the kernel can't run from a server:
receipt printers, barcode scanners, cash drawers, scales and RFID readers. It keeps working when the internet is down,
and syncs with the Rangka kernel when the connection comes back.

> **Status: first cut (November 2026).** The durable queue, `edge-sync/v1` push and the CLI work end to end
> against a stub kernel. There are no device connectors and no POS yet. The design is in
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
| `cmd/rangka-edge` | The binary: `register`, `enqueue`, `status`, `flush`, `run`, `dev-kernel`, `devices`, `version` |
| `internal/transport` | Generic hardware transports (USB HID, serial, Bluetooth, network) |
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
