// Command rangka-edge is the Rangka agent that runs at the outlet: it executes device connectors
// (printer, barcode scanner, cash drawer, RFID), keeps a local queue while offline and syncs with the
// Rangka kernel. See rangka-os/docs/contracts/0003-edge-and-devices.md.
package main

import (
	"fmt"
	"os"
)

// Version is set at build time with -ldflags "-X main.Version=…".
var Version = "0.0.0-dev"

func usage() {
	fmt.Fprint(os.Stderr, `rangka-edge — Rangka agent for the outlet

Usage:
  rangka-edge register --kernel <url> --code <pairing-code>   pair this device with a site
  rangka-edge run                                              run device connectors and sync
  rangka-edge devices                                          list attached devices
  rangka-edge status                                           show queue and sync state
  rangka-edge version

Status: scaffold only. Nothing below "version" is implemented yet.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(Version)
	case "help", "-h", "--help":
		usage()
	case "register", "run", "devices", "status":
		fmt.Fprintf(os.Stderr, "rangka-edge %s: not implemented yet\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}
