// Package queue is the local, durable queue that keeps events and jobs while the outlet is offline.
// Every item carries an idempotency key so a replay after reconnecting never duplicates work.
package queue
