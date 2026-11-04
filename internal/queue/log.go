package queue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// syncFile is a variable so tests can skip fsync when they write thousands of records.
var syncFile = func(f *os.File) error { return f.Sync() }

const (
	opPut  = "put"
	opAck  = "ack"
	opDead = "dead"
	opDone = "done"

	logName = "queue.log"
	tmpName = "queue.log.tmp"

	// A line is "<crc32 hex, 8 chars> <json>\n"; the CRC covers the JSON bytes.
	crcLen = 8

	compactMinDone = 1000
	doneRetention  = 400 * 24 * time.Hour
)

// record is one line of the log.
type record struct {
	Op      string    `json:"op"`
	Item    *Item     `json:"item,omitempty"`
	Key     string    `json:"key,omitempty"`
	Ref     string    `json:"ref,omitempty"`
	Code    string    `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
	Seq     int64     `json:"seq,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	Hash    string    `json:"hash,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

func encodeRecord(r record) ([]byte, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	line := make([]byte, 0, crcLen+1+len(body)+1)
	line = fmt.Appendf(line, "%08x ", crc32.ChecksumIEEE(body))
	line = append(line, body...)
	line = append(line, '\n')
	return line, nil
}

// decodeRecord parses one line without its trailing newline.
func decodeRecord(line []byte) (record, error) {
	var r record
	if len(line) < crcLen+2 || line[crcLen] != ' ' {
		return r, fmt.Errorf("malformed line")
	}
	sum, err := strconv.ParseUint(string(line[:crcLen]), 16, 32)
	if err != nil {
		return r, fmt.Errorf("bad checksum field: %w", err)
	}
	body := line[crcLen+1:]
	if uint32(sum) != crc32.ChecksumIEEE(body) {
		return r, fmt.Errorf("checksum mismatch")
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return r, err
	}
	return r, nil
}

// replay reads the log into q. A bad tail (torn write) is truncated; a bad line
// followed by good lines is ErrCorrupt. It returns the number of valid records.
func (q *Queue) replay(path string) (int, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	type line struct {
		off int
		rec record
		err error
	}
	var lines []line
	torn := -1 // offset of an unterminated last line
	for off := 0; off < len(data); {
		end := bytes.IndexByte(data[off:], '\n')
		if end < 0 {
			torn = off
			break
		}
		rec, derr := decodeRecord(data[off : off+end])
		lines = append(lines, line{off: off, rec: rec, err: derr})
		off += end + 1
	}

	valid := len(lines)
	cut := -1
	for i, l := range lines {
		if l.err == nil {
			continue
		}
		for _, later := range lines[i+1:] {
			if later.err == nil {
				return 0, fmt.Errorf("%w: line at offset %d: %v", ErrCorrupt, l.off, l.err)
			}
		}
		valid, cut = i, l.off
		break
	}
	if cut < 0 {
		cut = torn
	}

	count := 0
	for _, l := range lines[:valid] {
		if err := q.apply(l.rec); err != nil {
			return 0, fmt.Errorf("%w: line at offset %d: %v", ErrCorrupt, l.off, err)
		}
		count++
	}
	if cut >= 0 {
		if err := os.Truncate(path, int64(cut)); err != nil {
			return 0, err
		}
	}
	return count, nil
}

// shouldCompact reports whether done records are at least compactMinDone and
// more than half of all records.
func (q *Queue) shouldCompact(totalRecords int) bool {
	done, doneRecords := 0, 0
	for _, e := range q.entries {
		if e.item.State == StateDone {
			done++
			doneRecords += e.records
		}
	}
	return done >= compactMinDone && doneRecords*2 > totalRecords
}

// compact rewrites the log with pending and dead items in full and done items as
// key-only "done" records, then atomically replaces queue.log.
func (q *Queue) compact(dir string) error {
	cutoff := q.now().Add(-doneRetention)
	list := make([]*entry, 0, len(q.entries))
	for _, e := range q.entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].item.Seq < list[j].item.Seq })

	var buf bytes.Buffer
	kept := map[string]*entry{}
	for _, e := range list {
		it := e.item
		var recs []record
		switch it.State {
		case StatePending:
			recs = []record{{Op: opPut, Item: &it}}
		case StateDead:
			put := it
			put.State, put.Code, put.Message = StatePending, "", ""
			recs = []record{{Op: opPut, Item: &put}, {Op: opDead, Key: it.Key, Code: it.Code, Message: it.Message, At: e.at}}
		case StateDone:
			// The newest seq is always kept so a restart never reuses a seq.
			if e.at.Before(cutoff) && it.Seq != q.lastSeq {
				continue
			}
			recs = []record{{Op: opDone, Key: it.Key, Seq: it.Seq, Kind: it.Kind, Ref: it.Ref, Hash: e.hash, At: e.at}}
		}
		for _, r := range recs {
			line, err := encodeRecord(r)
			if err != nil {
				return err
			}
			buf.Write(line)
		}
		e.records = len(recs)
		kept[it.Key] = e
	}

	tmp := filepath.Join(dir, tmpName)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, logName)); err != nil {
		return err
	}
	q.entries = kept
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Some platforms refuse to fsync a directory; the rename already happened.
	_ = d.Sync()
	return nil
}
