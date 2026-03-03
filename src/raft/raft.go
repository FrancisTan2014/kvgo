// Invariant #1: Only committed entries may be applied to state machine.
package raft

type State uint8

const (
	Follower State = iota
	Candidate
	Leader
)

type Entry struct {
	Index uint64
	Term  uint64
	Data  []byte
}

type Ready struct {
	Entries          []Entry
	CommittedEntries []Entry
}

type Raft struct {
	id           uint64
	term         uint64
	state        State
	log          []Entry
	commitIndex  uint64
	appliedIndex uint64

	// volatile local states
	lastLogIndex uint64
	stableIndex  uint64
}

func New(id uint64) *Raft {
	return &Raft{
		id:    id,
		state: Follower,
		log:   make([]Entry, 0),
	}
}

func (r *Raft) Propose(data []byte) Entry {
	e := Entry{
		Index: r.lastLogIndex + 1,
		Term:  r.term,
		Data:  data,
	}
	r.log = append(r.log, e)
	r.lastLogIndex++
	return e
}

func (r *Raft) Ready() Ready {
	if r.stableIndex >= r.lastLogIndex {
		return Ready{}
	}

	start := int(r.stableIndex)
	end := int(r.lastLogIndex)
	return Ready{
		Entries:          r.log[start:end],
		CommittedEntries: make([]Entry, 0),
	}
}

func (r *Raft) Advance() {
	if r.stableIndex >= r.lastLogIndex {
		return
	}
	r.stableIndex = r.lastLogIndex
}
