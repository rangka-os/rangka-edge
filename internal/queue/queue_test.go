package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testClock() func() time.Time {
	t := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func openQ(t *testing.T, dir string) *Queue {
	t.Helper()
	q, err := Open(dir, Options{DeviceID: "dev_ab", Now: testClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return q
}

func newItem(kind, payload string) NewItem {
	return NewItem{Kind: kind, Actor: "kasir1", Payload: json.RawMessage(payload)}
}

func logSize(t *testing.T, dir string) int64 {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, "queue.log"))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func mustEnqueue(t *testing.T, q *Queue, n NewItem) Item {
	t.Helper()
	it, _, err := q.Enqueue(n)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return it
}

func TestEnqueueAssignsKeyAndSeq(t *testing.T) {
	q := openQ(t, t.TempDir())
	defer q.Close()
	a, added, err := q.Enqueue(newItem("sale", `{"total":1000}`))
	if err != nil || !added {
		t.Fatalf("first enqueue: added=%v err=%v", added, err)
	}
	b := mustEnqueue(t, q, newItem("sale", `{"total":2000}`))
	if a.Key != "dev_ab:000000000001" || a.Seq != 1 {
		t.Fatalf("first item: %+v", a)
	}
	if b.Key != "dev_ab:000000000002" || b.Seq != 2 {
		t.Fatalf("second item: %+v", b)
	}
	if a.State != StatePending || a.EnqueuedAt.IsZero() || a.OccurredAt.IsZero() {
		t.Fatalf("defaults not set: %+v", a)
	}
	if _, _, err := q.Enqueue(NewItem{Payload: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("empty kind must fail")
	}
	if _, _, err := q.Enqueue(NewItem{Kind: "x", Key: strings.Repeat("k", 201), Payload: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("key over 200 chars must fail")
	}
	if _, _, err := q.Enqueue(NewItem{Kind: "x", Key: strings.Repeat("k", 200), Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("key of 200 chars must work: %v", err)
	}
}

func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	q := openQ(t, dir)
	k1 := mustEnqueue(t, q, newItem("sale", `{"n":1}`)).Key
	k2 := mustEnqueue(t, q, newItem("sale", `{"n":2}`)).Key
	mustEnqueue(t, q, newItem("sale", `{"n":3}`))
	if err := q.Ack(k1, "srv-1"); err != nil {
		t.Fatal(err)
	}
	if err := q.Dead(k2, "bad_request", "rejected"); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	q = openQ(t, dir)
	defer q.Close()
	st := q.Stats()
	if st.Pending != 1 || st.Dead != 1 || st.Done != 1 || st.LastSeq != 3 {
		t.Fatalf("stats: %+v", st)
	}
	if st.OldestPendingAt.IsZero() {
		t.Fatal("OldestPendingAt not set")
	}
	dead := q.DeadItems()
	if len(dead) != 1 || dead[0].Key != k2 || dead[0].Code != "bad_request" || dead[0].Message != "rejected" {
		t.Fatalf("dead: %+v", dead)
	}
	done, ok := q.Get(k1)
	if !ok || done.State != StateDone || done.Ref != "srv-1" {
		t.Fatalf("done item: %+v ok=%v", done, ok)
	}
	if got := mustEnqueue(t, q, newItem("sale", `{"n":4}`)); got.Seq != 4 || got.Key != "dev_ab:000000000004" {
		t.Fatalf("next item: %+v", got)
	}
	p := q.Pending(10)
	if len(p) != 2 || p[0].Seq != 3 || p[1].Seq != 4 {
		t.Fatalf("pending order: %+v", p)
	}
	if len(q.Pending(1)) != 1 {
		t.Fatal("limit not applied")
	}
}

func TestAckDeadErrors(t *testing.T) {
	q := openQ(t, t.TempDir())
	defer q.Close()
	if err := q.Ack("nope", "r"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Ack unknown: %v", err)
	}
	if err := q.Dead("nope", "c", "m"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Dead unknown: %v", err)
	}
	k := mustEnqueue(t, q, newItem("sale", `{}`)).Key
	if err := q.Ack(k, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(k, "r2"); err != nil {
		t.Fatalf("second Ack must be nil: %v", err)
	}
	if got, _ := q.Get(k); got.Ref != "r1" {
		t.Fatalf("second Ack changed ref: %+v", got)
	}
	if err := q.Dead(k, "c", "m"); err == nil {
		t.Fatal("Dead on a done item must fail")
	}
}

func TestReplaySameKeyIsNoop(t *testing.T) {
	dir := t.TempDir()
	q := openQ(t, dir)
	n := NewItem{Key: "sale-42", Kind: "sale", Payload: json.RawMessage(`{"a": 1, "b": [1, 2]}`)}
	first, added, err := q.Enqueue(n)
	if err != nil || !added {
		t.Fatalf("first: added=%v err=%v", added, err)
	}
	size := logSize(t, dir)

	same := n
	same.Payload = json.RawMessage(`{"a":1,"b":[1,2]}`) // same JSON, different whitespace
	check := func(stage string, q *Queue) {
		t.Helper()
		it, added, err := q.Enqueue(same)
		if err != nil || added {
			t.Fatalf("%s: added=%v err=%v", stage, added, err)
		}
		if it.Key != first.Key || it.Seq != first.Seq {
			t.Fatalf("%s: item %+v", stage, it)
		}
		if got := logSize(t, dir); got != size {
			t.Fatalf("%s: log grew from %d to %d", stage, size, got)
		}
	}
	check("same session", q)
	q.Close()
	q = openQ(t, dir)
	check("after reopen", q)
	if err := q.Ack("sale-42", "ref"); err != nil {
		t.Fatal(err)
	}
	size = logSize(t, dir)
	check("after ack", q)
	q.Close()
	q = openQ(t, dir)
	check("after ack and reopen", q)
	q.Close()
}

func TestEnqueueSameKeyDifferentPayload(t *testing.T) {
	q := openQ(t, t.TempDir())
	defer q.Close()
	orig, _, err := q.Enqueue(NewItem{Key: "k1", Kind: "sale", Payload: json.RawMessage(`{"total":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Enqueue(NewItem{Key: "k1", Kind: "sale", Payload: json.RawMessage(`{"total":2}`)}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("different payload: %v", err)
	}
	if _, _, err := q.Enqueue(NewItem{Key: "k1", Kind: "refund", Payload: json.RawMessage(`{"total":1}`)}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("different kind: %v", err)
	}
	got, ok := q.Get("k1")
	if !ok || string(got.Payload) != string(orig.Payload) || got.Kind != "sale" {
		t.Fatalf("Get after conflict: %+v", got)
	}
}

func TestOpenTruncatesTornTail(t *testing.T) {
	dir := t.TempDir()
	q := openQ(t, dir)
	mustEnqueue(t, q, newItem("sale", `{"n":1}`))
	mustEnqueue(t, q, newItem("sale", `{"n":2}`))
	q.Close()
	size := logSize(t, dir)

	f, err := os.OpenFile(filepath.Join(dir, "queue.log"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`1a2b3c4d {"op":"put","item":{"key":"dev_ab:0000000`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	q = openQ(t, dir)
	if st := q.Stats(); st.Pending != 2 || st.LastSeq != 2 {
		t.Fatalf("stats after torn tail: %+v", st)
	}
	if got := logSize(t, dir); got != size {
		t.Fatalf("file not truncated: %d, want %d", got, size)
	}
	if it := mustEnqueue(t, q, newItem("sale", `{"n":3}`)); it.Seq != 3 {
		t.Fatalf("next item: %+v", it)
	}
	q.Close()
	q = openQ(t, dir)
	defer q.Close()
	if st := q.Stats(); st.Pending != 3 {
		t.Fatalf("stats after reopen: %+v", st)
	}
}

func TestOpenTruncatesBadChecksumTail(t *testing.T) {
	dir := t.TempDir()
	q := openQ(t, dir)
	mustEnqueue(t, q, newItem("sale", `{"n":1}`))
	mustEnqueue(t, q, newItem("sale", `{"n":2}`))
	q.Close()
	path := filepath.Join(dir, "queue.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	bad := strings.Replace(lines[1], `"n":2`, `"n":9`, 1)
	if err := os.WriteFile(path, []byte(lines[0]+bad), 0o600); err != nil {
		t.Fatal(err)
	}
	q = openQ(t, dir)
	defer q.Close()
	if st := q.Stats(); st.Pending != 1 || st.LastSeq != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if got := logSize(t, dir); got != int64(len(lines[0])) {
		t.Fatalf("file size %d, want %d", got, len(lines[0]))
	}
}

func TestCorruptMiddleLine(t *testing.T) {
	dir := t.TempDir()
	q := openQ(t, dir)
	for i := 1; i <= 3; i++ {
		mustEnqueue(t, q, newItem("sale", fmt.Sprintf(`{"n":%d}`, i)))
	}
	q.Close()
	path := filepath.Join(dir, "queue.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(data), `"n":2`, `"n":7`, 1)
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{DeviceID: "dev_ab"}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open: %v, want ErrCorrupt", err)
	}
	// A failed Open must not leave the directory locked.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	q = openQ(t, dir)
	q.Close()
}

func TestCompactionKeepsDoneKeys(t *testing.T) {
	// Fsync on every append would make 3000 appends slow; the thresholds stay real.
	orig := syncFile
	syncFile = func(*os.File) error { return nil }
	defer func() { syncFile = orig }()

	dir := t.TempDir()
	q := openQ(t, dir)
	const n = 1500
	for i := 1; i <= n; i++ {
		k := fmt.Sprintf("sale-%04d", i)
		if _, _, err := q.Enqueue(NewItem{Key: k, Kind: "sale", Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))}); err != nil {
			t.Fatal(err)
		}
		if err := q.Ack(k, "ref-"+k); err != nil {
			t.Fatal(err)
		}
	}
	mustEnqueue(t, q, newItem("sale", `{"p":1}`))
	mustEnqueue(t, q, newItem("sale", `{"p":2}`))
	q.Close()
	before := logSize(t, dir)

	q = openQ(t, dir)
	defer q.Close()
	after := logSize(t, dir)
	if after >= before {
		t.Fatalf("log not compacted: %d -> %d", before, after)
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.log.tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind: %v", err)
	}
	st := q.Stats()
	if st.Pending != 2 || st.Done != n || st.LastSeq != n+2 {
		t.Fatalf("stats: %+v", st)
	}
	// Replay of an acked key is still a no-op, with a different payload still a conflict.
	it, added, err := q.Enqueue(NewItem{Key: "sale-0007", Kind: "sale", Payload: json.RawMessage(`{"i":7}`)})
	if err != nil || added || it.State != StateDone || it.Ref != "ref-sale-0007" {
		t.Fatalf("replay: %+v added=%v err=%v", it, added, err)
	}
	if got := logSize(t, dir); got != after {
		t.Fatalf("replay wrote to the log: %d -> %d", after, got)
	}
	if _, _, err := q.Enqueue(NewItem{Key: "sale-0007", Kind: "sale", Payload: json.RawMessage(`{"i":8}`)}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("different payload after compaction: %v", err)
	}
	if len(q.Pending(10)) != 2 {
		t.Fatal("pending items lost")
	}
	if got := mustEnqueue(t, q, newItem("sale", `{"p":3}`)); got.Seq != n+3 {
		t.Fatalf("seq after compaction: %d", got.Seq)
	}
}

func TestNoCompactionBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	q := openQ(t, dir)
	for i := 0; i < 5; i++ {
		k := mustEnqueue(t, q, newItem("sale", fmt.Sprintf(`{"i":%d}`, i))).Key
		if err := q.Ack(k, "r"); err != nil {
			t.Fatal(err)
		}
	}
	q.Close()
	size := logSize(t, dir)
	q = openQ(t, dir)
	defer q.Close()
	if got := logSize(t, dir); got != size {
		t.Fatalf("log rewritten below threshold: %d -> %d", size, got)
	}
}

func TestOpenTwiceLocked(t *testing.T) {
	if !lockSupported {
		t.Skip("file locking not supported on this platform")
	}
	dir := t.TempDir()
	q := openQ(t, dir)
	if _, err := Open(dir, Options{DeviceID: "dev_ab"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: %v, want ErrLocked", err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q = openQ(t, dir)
	q.Close()
}

func TestReplayAfterRestartWithHTMLChars(t *testing.T) {
	dir := t.TempDir()
	q := openQ(t, dir)
	n := NewItem{Key: "k1", Kind: "sale", Payload: json.RawMessage(`{"name": "Kopi & Roti <1>"}`)}
	first, _, err := q.Enqueue(n)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"Kopi & Roti <1>"}`
	if string(first.Payload) != want {
		t.Fatalf("payload %s", first.Payload)
	}
	q.Close()
	q = openQ(t, dir)
	defer q.Close()
	size := logSize(t, dir)
	it, added, err := q.Enqueue(n)
	if err != nil || added {
		t.Fatalf("replay: added=%v err=%v", added, err)
	}
	if string(it.Payload) != want {
		t.Fatalf("payload after reopen %s", it.Payload)
	}
	if got, _ := q.Get("k1"); string(got.Payload) != want {
		t.Fatalf("Get payload %s", got.Payload)
	}
	if logSize(t, dir) != size {
		t.Fatal("log grew")
	}
}

func TestSyncFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	q := openQ(t, dir)
	orig := syncFile
	defer func() { syncFile = orig }()
	syncFile = func(*os.File) error { return errors.New("disk full") }
	if _, _, err := q.Enqueue(newItem("sale", `{"n":1}`)); err == nil {
		t.Fatal("enqueue must fail when fsync fails")
	}
	if logSize(t, dir) != 0 {
		t.Fatal("failed line not rolled back")
	}
	syncFile = orig
	it, _, err := q.Enqueue(newItem("sale", `{"n":1}`))
	if err != nil || it.Seq != 1 {
		t.Fatalf("next enqueue: %+v err=%v", it, err)
	}
	q.Close()
	q = openQ(t, dir)
	defer q.Close()
	if st := q.Stats(); st.Pending != 1 || st.LastSeq != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

// fillDone acks n items, then adds two pending ones, with fsync stubbed for speed.
func fillDone(t *testing.T, dir string, n int) {
	t.Helper()
	orig := syncFile
	syncFile = func(*os.File) error { return nil }
	defer func() { syncFile = orig }()
	q := openQ(t, dir)
	for i := 1; i <= n; i++ {
		k := fmt.Sprintf("sale-%04d", i)
		mustEnqueue(t, q, NewItem{Key: k, Kind: "sale", Payload: json.RawMessage(`{}`)})
		if err := q.Ack(k, "r"); err != nil {
			t.Fatal(err)
		}
	}
	mustEnqueue(t, q, newItem("sale", `{"p":1}`))
	q.Close()
}

func TestCompactionNotRepeated(t *testing.T) {
	dir := t.TempDir()
	fillDone(t, dir, 1500)
	q := openQ(t, dir) // compacts
	q.Close()
	path := filepath.Join(dir, "queue.log")
	before, _ := os.Stat(path)
	renames := 0
	orig := renameFile
	renameFile = func(a, b string) error { renames++; return orig(a, b) }
	defer func() { renameFile = orig }()
	q = openQ(t, dir)
	defer q.Close()
	after, _ := os.Stat(path)
	if renames != 0 || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("second Open rewrote the log (renames=%d)", renames)
	}
	if st := q.Stats(); st.Done != 1500 || st.Pending != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestCompactionFailureIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	fillDone(t, dir, 1500)
	before := logSize(t, dir)
	orig := renameFile
	renameFile = func(string, string) error { return errors.New("rename failed") }
	defer func() { renameFile = orig }()
	q, err := Open(dir, Options{DeviceID: "dev_ab", Now: testClock()})
	if err != nil {
		t.Fatalf("Open must succeed: %v", err)
	}
	defer q.Close()
	if st := q.Stats(); st.Done != 1500 || st.Pending != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if logSize(t, dir) != before {
		t.Fatal("log changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.log.tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp left behind: %v", err)
	}
	if _, _, err := q.Enqueue(newItem("sale", `{"p":2}`)); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReadOnlyWhileLocked(t *testing.T) {
	if !lockSupported {
		t.Skip("no directory lock on this platform")
	}
	dir := t.TempDir()
	q := openQ(t, dir)
	defer q.Close()
	mustEnqueue(t, q, newItem("sale.recorded", `{"a":1}`))
	mustEnqueue(t, q, newItem("sale.recorded", `{"a":2}`))
	if err := q.Ack("dev_ab:000000000001", "r1"); err != nil {
		t.Fatal(err)
	}
	// A torn tail, as left by a power cut while the writer is mid-append.
	f, err := os.OpenFile(filepath.Join(dir, "queue.log"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("deadbeef {\"op\":\"pu")
	f.Close()
	before := logSize(t, dir)

	if _, err := Open(dir, Options{DeviceID: "dev_ab"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: %v", err)
	}
	ro, err := OpenReadOnly(dir, Options{DeviceID: "dev_ab"})
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if got, want := ro.Stats(), q.Stats(); got != want {
		t.Fatalf("stats %+v != %+v", got, want)
	}
	if _, _, err := ro.Enqueue(newItem("sale.recorded", `{}`)); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := ro.Ack("dev_ab:000000000002", "r"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Ack: %v", err)
	}
	if err := ro.Dead("dev_ab:000000000002", "c", "m"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Dead: %v", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if after := logSize(t, dir); after != before {
		t.Fatalf("log size changed %d -> %d", before, after)
	}
}

func TestExplicitKeyReservedForm(t *testing.T) {
	q := openQ(t, t.TempDir())
	defer q.Close()
	for _, k := range []string{"dev_ab:000000000007", "dev_ab:123456789012"} {
		if _, _, err := q.Enqueue(NewItem{Key: k, Kind: "k", Payload: json.RawMessage(`{}`)}); !errors.Is(err, ErrReservedKey) {
			t.Fatalf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"dev_ab:s-1", "dev_ab:00000000007", "dev_ab:0000000000070", "dev_cd:000000000007", "s-1"} {
		if _, _, err := q.Enqueue(NewItem{Key: k, Kind: "k", Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	if it := mustEnqueue(t, q, newItem("k", `{}`)); !strings.HasSuffix(it.Key, ":000000000006") {
		t.Fatalf("generated key %s", it.Key)
	}
}
