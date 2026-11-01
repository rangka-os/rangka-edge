# Rangka Edge

**The Rangka agent that runs at the outlet.** It runs the device connectors that the kernel can't run from a server:
receipt printers, barcode scanners, cash drawers, scales and RFID readers. It keeps working when the internet is down,
and syncs with the Rangka kernel when the connection comes back.

> **Status: scaffold (November 2026).** Nothing runs yet except `rangka-edge version`. The design is in
> [rangka-os/docs/contracts/0003-edge-and-devices.md](https://github.com/rangka-os/rangka-os/blob/main/docs/contracts/0003-edge-and-devices.md).
> Work starts with POS.

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
| `cmd/rangka-edge` | The binary: `register`, `run`, `devices`, `status`, `version` |
| `internal/transport` | Generic hardware transports (USB HID, serial, Bluetooth, network) |
| `internal/runner` | Runs device connectors: device reads → triggers, actions → device |
| `internal/queue` | Durable local queue with idempotency keys |
| `internal/sync` | The `edge-sync` protocol with the kernel |

Shared types (the `.rk` reader, connector schema, CEL) come from `github.com/rangka-os/rangka-os/kernel/pkg/...` at a
pinned `rangka-os` release. They are moved out of `kernel/internal` as the first step of 0003.

## Build

```bash
go build -o bin/rangka-edge ./cmd/rangka-edge
./bin/rangka-edge version
```

## Licence

Elastic License 2.0 (ELv2), see `LICENSE`. Copyright 2026 Saga Tekno Studio.
