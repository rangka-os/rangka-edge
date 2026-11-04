//go:build !unix

package queue

import "os"

const lockSupported = false

// lockFile is a no-op where flock is unavailable.
func lockFile(*os.File) error { return nil }
