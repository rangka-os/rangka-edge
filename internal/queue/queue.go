package queue

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	// ErrKeyConflict means the key is already used by an item with a different kind or payload.
	ErrKeyConflict = errors.New("queue: key already used with a different payload")
	// ErrLocked means another process has the queue directory open.
	ErrLocked = errors.New("queue: directory is locked by another process")
	// ErrCorrupt means the log has a damaged record that is not just a torn tail.
	ErrCorrupt = errors.New("queue: log is corrupt")
	// ErrNotFound means no item has the key.
	ErrNotFound = errors.New("queue: item not found")
)

// MaxKeyLen is the longest idempotency key accepted.
const MaxKeyLen = 200

// State is where an item is in its life.
type State string

const (
	StatePending State = "pending"
	StateDone    State = "done"
	StateDead    State = "dead"
)

// Item is one queued event or job. Payload is kept only while the item is pending or dead.
type Item struct {
	Key        string          `json:"key"`
	Kind       string          `json:"kind"`
	Actor      string          `json:"actor,omitempty"`
	Seq        int64           `json:"seq"`
	OccurredAt time.Time       `json:"occurred_at"`
	EnqueuedAt time.Time       `json:"enqueued_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	State      State           `json:"state"`
	Ref        string          `json:"ref,omitempty"`     // set when done
	Code       string          `json:"code,omitempty"`    // set when dead
	Message    string          `json:"message,omitempty"` // set when dead
}

// Options configures Open.
type Options struct {
	DeviceID string
	Now      func() time.Time // defaults to time.Now
}

// NewItem is what a caller hands to Enqueue. An empty Key becomes "<DeviceID>:<seq %012d>".
type NewItem struct {
	Key, Kind, Actor string
	OccurredAt       time.Time
	Payload          json.RawMessage
}

// Stats summarises the queue.
type Stats struct {
	Pending, Dead, Done int
	OldestPendingAt     time.Time
	LastSeq             int64
}

type entry struct {
	item    Item
	hash    string    // sha256 of the compacted payload
	at      time.Time // when it was acked or declared dead
	records int       // log records that belong to this item
}

// Queue is a durable, append-only log of items with idempotency keys.
type Queue struct {
	mu       sync.Mutex
	deviceID string
	now      func() time.Time
	dir      string
	log      *os.File
	lock     *os.File
	entries  map[string]*entry
	lastSeq  int64
	closed   bool
}

// Open opens (creating if needed) the queue in dir, replays the log and compacts it when worthwhile.
func Open(dir string, opt Options) (*Queue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	q := &Queue{deviceID: opt.DeviceID, now: opt.Now, dir: dir, entries: map[string]*entry{}}
	if q.now == nil {
		q.now = time.Now
	}

	lock, err := os.OpenFile(filepath.Join(dir, "queue.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		return nil, err
	}
	q.lock = lock

	if err := q.load(); err != nil {
		lock.Close()
		return nil, err
	}
	return q, nil
}

func (q *Queue) load() error {
	path := filepath.Join(q.dir, logName)
	// A tmp file is the leftover of a crashed compaction; the real log is still whole.
	_ = os.Remove(filepath.Join(q.dir, tmpName))
	total, err := q.replay(path)
	if err != nil {
		return err
	}
	if q.shouldCompact(total) {
		if err := q.compact(q.dir); err != nil {
			return fmt.Errorf("queue: compaction: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	q.log = f
	return nil
}

// apply updates the in-memory state with one record. Live operations and replay share it.
func (q *Queue) apply(r record) error {
	switch r.Op {
	case opPut:
		if r.Item == nil || r.Item.Key == "" {
			return errors.New("put without item")
		}
		if _, dup := q.entries[r.Item.Key]; dup {
			return fmt.Errorf("duplicate put for %q", r.Item.Key)
		}
		it := *r.Item
		it.State = StatePending
		q.entries[it.Key] = &entry{item: it, hash: payloadHash(it.Payload), records: 1}
		q.bumpSeq(it.Seq)
	case opAck:
		e, err := q.pendingEntry(r.Key)
		if err != nil {
			return err
		}
		e.item.State, e.item.Ref, e.item.Payload = StateDone, r.Ref, nil
		e.at = r.At
		e.records++
	case opDead:
		e, err := q.pendingEntry(r.Key)
		if err != nil {
			return err
		}
		e.item.State, e.item.Code, e.item.Message = StateDead, r.Code, r.Message
		e.at = r.At
		e.records++
	case opDone:
		if r.Key == "" {
			return errors.New("done without key")
		}
		if _, dup := q.entries[r.Key]; dup {
			return fmt.Errorf("duplicate done for %q", r.Key)
		}
		q.entries[r.Key] = &entry{
			item:    Item{Key: r.Key, Kind: r.Kind, Seq: r.Seq, State: StateDone, Ref: r.Ref},
			hash:    r.Hash,
			at:      r.At,
			records: 1,
		}
		q.bumpSeq(r.Seq)
	default:
		return fmt.Errorf("unknown op %q", r.Op)
	}
	return nil
}

func (q *Queue) pendingEntry(key string) (*entry, error) {
	e, ok := q.entries[key]
	if !ok {
		return nil, fmt.Errorf("record for unknown key %q", key)
	}
	if e.item.State != StatePending {
		return nil, fmt.Errorf("record for %q which is %s", key, e.item.State)
	}
	return e, nil
}

func (q *Queue) bumpSeq(seq int64) {
	if seq > q.lastSeq {
		q.lastSeq = seq
	}
}

// append writes one record with a single Write and an fsync, then applies it.
func (q *Queue) append(r record) error {
	line, err := encodeRecord(r)
	if err != nil {
		return err
	}
	st, err := q.log.Stat()
	if err != nil {
		return err
	}
	if _, err := q.log.Write(line); err != nil {
		_ = os.Truncate(q.log.Name(), st.Size()) // drop a partial line; replay would do it anyway
		return err
	}
	if err := syncFile(q.log); err != nil {
		return err
	}
	return q.apply(r)
}

func payloadHash(p json.RawMessage) string {
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

func compactJSON(p json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(p)) == 0 {
		return json.RawMessage("null"), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, p); err != nil {
		return nil, fmt.Errorf("queue: payload is not valid JSON: %w", err)
	}
	return buf.Bytes(), nil
}

func (q *Queue) errIfClosed() error {
	if q.closed {
		return errors.New("queue: closed")
	}
	return nil
}

// Enqueue appends a new item. A key that already exists with the same kind and payload
// returns the stored item with added=false and writes nothing; with different content it
// returns ErrKeyConflict.
func (q *Queue) Enqueue(n NewItem) (it Item, added bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.errIfClosed(); err != nil {
		return Item{}, false, err
	}
	if n.Kind == "" {
		return Item{}, false, errors.New("queue: kind is required")
	}
	payload, err := compactJSON(n.Payload)
	if err != nil {
		return Item{}, false, err
	}
	if len(n.Key) > MaxKeyLen {
		return Item{}, false, fmt.Errorf("queue: key longer than %d chars", MaxKeyLen)
	}

	if n.Key != "" {
		if e, ok := q.entries[n.Key]; ok {
			if e.item.Kind != n.Kind || e.hash != payloadHash(payload) {
				return Item{}, false, fmt.Errorf("%w: %s", ErrKeyConflict, n.Key)
			}
			return e.item, false, nil
		}
	}

	seq := q.lastSeq + 1
	key := n.Key
	if key == "" {
		key = fmt.Sprintf("%s:%012d", q.deviceID, seq)
		if len(key) > MaxKeyLen {
			return Item{}, false, fmt.Errorf("queue: key longer than %d chars", MaxKeyLen)
		}
		if _, ok := q.entries[key]; ok {
			return Item{}, false, fmt.Errorf("%w: %s", ErrKeyConflict, key)
		}
	}
	now := q.now()
	occurred := n.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	item := Item{
		Key: key, Kind: n.Kind, Actor: n.Actor, Seq: seq,
		OccurredAt: occurred, EnqueuedAt: now,
		Payload: payload, State: StatePending,
	}
	if err := q.append(record{Op: opPut, Item: &item}); err != nil {
		return Item{}, false, err
	}
	return q.entries[key].item, true, nil
}

func (q *Queue) sorted(state State, limit int) []Item {
	var out []Item
	for _, e := range q.entries {
		if e.item.State == state {
			out = append(out, e.item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Pending returns up to limit pending items in seq order; limit <= 0 means all.
func (q *Queue) Pending(limit int) []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.sorted(StatePending, limit)
}

// DeadItems returns every dead item in seq order.
func (q *Queue) DeadItems() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.sorted(StateDead, 0)
}

// Get returns the item with the key, whatever its state. Done items have no payload.
func (q *Queue) Get(key string) (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.entries[key]
	if !ok {
		return Item{}, false
	}
	return e.item, true
}

// Ack marks a pending item done. Acking a done item is a no-op; an unknown key is ErrNotFound.
func (q *Queue) Ack(key, ref string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.errIfClosed(); err != nil {
		return err
	}
	e, ok := q.entries[key]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	switch e.item.State {
	case StateDone:
		return nil
	case StateDead:
		return fmt.Errorf("queue: %s is dead, cannot ack", key)
	}
	return q.append(record{Op: opAck, Key: key, Ref: ref, At: q.now()})
}

// Dead moves a pending item to the dead set. Marking a dead item dead again is a no-op.
func (q *Queue) Dead(key, code, message string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.errIfClosed(); err != nil {
		return err
	}
	e, ok := q.entries[key]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	switch e.item.State {
	case StateDead:
		return nil
	case StateDone:
		return fmt.Errorf("queue: %s is done, cannot mark dead", key)
	}
	return q.append(record{Op: opDead, Key: key, Code: code, Message: message, At: q.now()})
}

// Stats counts items by state.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := Stats{LastSeq: q.lastSeq}
	for _, e := range q.entries {
		switch e.item.State {
		case StatePending:
			s.Pending++
			if s.OldestPendingAt.IsZero() || e.item.EnqueuedAt.Before(s.OldestPendingAt) {
				s.OldestPendingAt = e.item.EnqueuedAt
			}
		case StateDead:
			s.Dead++
		case StateDone:
			s.Done++
		}
	}
	return s
}

// Close flushes and closes the log and releases the directory lock. It is safe to call twice.
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	err := q.log.Close()
	if lerr := q.lock.Close(); err == nil {
		err = lerr
	}
	return err
}
