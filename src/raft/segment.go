package raft

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	segmentPrefix       = "wal-"
	segmentSuffix       = ".log"
	defaultSegmentLimit = 64 * 1000 * 1000 // 64MB
)

type segmentedWAL struct {
	dir          string
	segmentLimit int64
	segments     []string // sorted segment filenames
	current      *wal     // the active (last) segment
}

func newSegmentedWAL(dir string, segmentLimit int64) (*segmentedWAL, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	if segmentLimit <= 0 {
		segmentLimit = defaultSegmentLimit
	}

	sw := &segmentedWAL{
		dir:          dir,
		segmentLimit: segmentLimit,
	}

	segments, err := sw.listSegments()
	if err != nil {
		return nil, err
	}
	sw.segments = segments

	if len(sw.segments) == 0 {
		name := segmentName(0)
		w, err := newWAL(dir, name)
		if err != nil {
			return nil, err
		}
		sw.segments = []string{name}
		sw.current = w
		return sw, nil
	}

	last := sw.segments[len(sw.segments)-1]
	w, err := newWAL(dir, last)
	if err != nil {
		return nil, err
	}
	sw.current = w
	return sw, nil
}

func segmentName(seq uint32) string {
	return fmt.Sprintf("%s%08d%s", segmentPrefix, seq, segmentSuffix)
}

func parseSegmentSeq(name string) (uint32, bool) {
	if !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentSuffix) {
		return 0, false
	}
	numStr := name[len(segmentPrefix) : len(name)-len(segmentSuffix)]
	var seq uint32
	if _, err := fmt.Sscanf(numStr, "%08d", &seq); err != nil {
		return 0, false
	}
	return seq, true
}

func (sw *segmentedWAL) listSegments() ([]string, error) {
	entries, err := os.ReadDir(sw.dir)
	if err != nil {
		return nil, err
	}
	var segments []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := parseSegmentSeq(e.Name()); ok {
			segments = append(segments, e.Name())
		}
	}
	sort.Strings(segments)
	return segments, nil
}

func (sw *segmentedWAL) write(batch []byte) error {
	return sw.current.write(batch)
}

func (sw *segmentedWAL) sync() error {
	return sw.current.sync()
}

func (sw *segmentedWAL) cut() error {
	if err := sw.current.sync(); err != nil {
		return err
	}
	if err := sw.current.close(); err != nil {
		return err
	}

	lastSeq, _ := parseSegmentSeq(sw.segments[len(sw.segments)-1])
	name := segmentName(lastSeq + 1)
	w, err := newWAL(sw.dir, name)
	if err != nil {
		return err
	}
	sw.segments = append(sw.segments, name)
	sw.current = w
	return nil
}

func (sw *segmentedWAL) maybeCut() error {
	if sw.current.size >= sw.segmentLimit {
		return sw.cut()
	}
	return nil
}

type tailCorruptionError struct {
	goodPos int64
}

func (e *tailCorruptionError) Error() string {
	return "corrupted tail"
}

// readAll reads all frames across all segments sequentially. For each frame,
// fn is called with the segment name and the decoded batch bytes.
// Tail corruption in the last segment is recovered by truncation.
// Corruption in sealed (non-last) segments is a hard error.
func (sw *segmentedWAL) readAll(fn func(seg string, batch []byte) error) error {
	for i, name := range sw.segments {
		isLast := i == len(sw.segments)-1

		var r *wal
		var err error
		if isLast {
			if _, err := sw.current.file.Seek(0, io.SeekStart); err != nil {
				return err
			}
			r = sw.current
		} else {
			r, err = openWALReadOnly(sw.dir, name)
			if err != nil {
				return err
			}
		}

		readErr := readSegmentFrames(r, name, fn)

		if !isLast {
			_ = r.close()
		}

		if readErr != nil {
			var tc *tailCorruptionError
			if errors.As(readErr, &tc) {
				if !isLast {
					return fmt.Errorf("corruption in sealed segment %s", name)
				}
				if truncErr := r.truncate(tc.goodPos); truncErr != nil {
					return truncErr
				}
				continue
			}
			return readErr
		}
	}
	return nil
}

func readSegmentFrames(r *wal, name string, fn func(seg string, batch []byte) error) error {
	var goodPos int64
	for {
		batch, err := r.read()
		if err == io.EOF {
			return nil
		}
		if err == ErrTailCorruption {
			return &tailCorruptionError{goodPos: goodPos}
		}
		if err != nil {
			return err
		}
		if err := fn(name, batch); err != nil {
			return err
		}
		goodPos += int64(frameHeaderSize + len(batch))
	}
}

func (sw *segmentedWAL) deleteSegments(shouldDelete func(name string) bool) error {
	var kept []string
	for _, name := range sw.segments {
		if name == sw.current.filename || !shouldDelete(name) {
			kept = append(kept, name)
			continue
		}
		path := filepath.Join(sw.dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	sw.segments = kept
	return nil
}

func (sw *segmentedWAL) close() error {
	if sw.current != nil {
		return sw.current.close()
	}
	return nil
}

func (sw *segmentedWAL) segmentCount() int {
	return len(sw.segments)
}

func (sw *segmentedWAL) currentSegment() string {
	return sw.current.filename
}
