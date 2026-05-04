package raft

import (
	"fmt"
	"kvgo/raftpb"
	"log/slog"
)

type raftLog struct {
	storage   Storage
	unstable  unstable
	committed uint64
	applied   uint64

	logger *slog.Logger
}

func newRaftLog(storage Storage, logger *slog.Logger) *raftLog {
	firstIndex := storage.FirstIndex()
	lastIndex := storage.LastIndex()

	var committed, applied uint64
	if firstIndex > 0 {
		committed = firstIndex - 1
		applied = firstIndex - 1
	}

	return &raftLog{
		storage: storage,
		unstable: unstable{
			offset: lastIndex + 1,
		},
		committed: committed,
		applied:   applied,
		logger:    logger,
	}
}

func (l *raftLog) term(i uint64) (uint64, error) {
	if i == 0 {
		return 0, nil
	}
	if t, ok := l.unstable.maybeTerm(i); ok {
		return t, nil
	}
	return l.storage.Term(i)
}

func (l *raftLog) matchTerm(i uint64, term uint64) bool {
	t, err := l.term(i)
	return err == nil && t == term
}

func (l *raftLog) nextUnstableEnts() []*raftpb.Entry {
	return l.unstable.entries
}

func (l *raftLog) nextCommittedEnts() []*raftpb.Entry {
	if l.applied >= l.committed {
		return nil
	}
	return l.slice(l.applied+1, l.committed+1)
}

func (l *raftLog) lastIndex() uint64 {
	if i, ok := l.unstable.maybeLastIndex(); ok {
		return i
	}
	return l.storage.LastIndex()
}

func (l *raftLog) stableTo(i uint64, term uint64) {
	l.unstable.stableTo(i, term)
}

func (l *raftLog) appliedTo(i uint64) {
	l.applied = i
}

func (l *raftLog) commitTo(i uint64) {
	if i <= l.committed {
		return
	}
	if i > l.lastIndex() {
		l.logger.Error("commitTo: index is out of range",
			"tocommit", i, "lastIndex", l.lastIndex())
		panic("commitTo: index is out of range")
	}
	l.committed = i
}

func (l *raftLog) appendEntries(ents []*raftpb.Entry) uint64 {
	if len(ents) == 0 {
		return l.lastIndex()
	}
	if after := ents[0].Index - 1; after < l.committed {
		l.logger.Error("appendEntries: after is before committed",
			"after", after, "committed", l.committed)
		panic("appendEntries: after is before committed")
	}
	l.unstable.truncateAndAppend(ents)
	return l.lastIndex()
}

func (l *raftLog) hasNextUnstableEnts() bool {
	return len(l.unstable.entries) > 0
}

func (l *raftLog) hasNextCommittedEnts() bool {
	return l.applied < l.committed
}

// slice returns entries in [lo, hi). Reads from storage first, then unstable.
func (l *raftLog) slice(lo, hi uint64) []*raftpb.Entry {
	if lo >= hi {
		return nil
	}
	var ents []*raftpb.Entry
	// Read from storage up to the unstable boundary.
	if lo < l.unstable.offset {
		storageHi := min(hi, l.unstable.offset)
		stored, err := l.storage.Entries(lo, storageHi)
		if err != nil {
			return nil
		}
		ents = stored
	}
	// Read from unstable for the remainder.
	if hi > l.unstable.offset {
		unstableLo := max(lo, l.unstable.offset) - l.unstable.offset
		unstableHi := hi - l.unstable.offset
		if unstableHi > uint64(len(l.unstable.entries)) {
			unstableHi = uint64(len(l.unstable.entries))
		}
		ents = append(ents, l.unstable.entries[unstableLo:unstableHi]...)
	}
	return ents
}

func (l *raftLog) lastEntryID() (uint64, uint64) {
	index := l.lastIndex()
	if index == 0 {
		return 0, 0
	}
	t, err := l.term(index)
	if err != nil {
		panic(fmt.Sprintf("unexpected error getting term for last entry at %d: %v", index, err))
	}
	return index, t
}

func (l *raftLog) isUpToDate(index, term uint64) bool {
	lastIndex, lastTerm := l.lastEntryID()
	return term > lastTerm || (term == lastTerm && index >= lastIndex)
}

func (l *raftLog) restore(snapIndex uint64) {
	l.unstable.offset = snapIndex + 1
	l.unstable.entries = nil
	if l.committed < snapIndex {
		l.committed = snapIndex
	}
	if l.applied < snapIndex {
		l.applied = snapIndex
	}
}
