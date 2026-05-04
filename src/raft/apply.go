package raft

import "kvgo/raftpb"

// ApplyTarget is the smallest seam needed for 036d.
// It only needs to answer one question: can committed entries become visible?
// Even after Raft is integrated into the real server, this boundary still
// exists conceptually: something outside pure Raft must take committed entries
// and make them visible to the state machine.
type ApplyTarget interface {
	Apply(entries []*raftpb.Entry) error
}

// Applier consumes Ready from Raft and advances progress only after committed
// entries have been applied successfully.
//
// The type is intentionally small because 036d is proving one invariant, not
// designing the final server shape. Later, this logic may be absorbed into a
// higher-level coordinator, but the sequencing it represents must survive:
// committed entries become visible before progress advances.
type Applier struct {
	r      *Raft
	target ApplyTarget
}

func NewApplier(r *Raft, target ApplyTarget) *Applier {
	return &Applier{r: r, target: target}
}

// ConsumeReady handles the current Ready if one exists.
// Uncommitted entries are not applied. Committed entries must apply
// successfully before progress advances.
//
// That rule remains valuable even when this episode's temporary artifact is no
// longer visible as a standalone type. The final system may rename or relocate
// this code, but it still needs this gate between "committed" and "applied".
func (a *Applier) ConsumeReady() (Ready, error) {
	if !a.r.HasReady() {
		return Ready{}, nil
	}

	rd := a.r.Ready()

	// Persist unstable entries to storage before advancing.
	if len(rd.Entries) > 0 {
		if err := a.r.raftLog.storage.Save(rd.Entries, nil); err != nil {
			return rd, err
		}
	}

	if len(rd.CommittedEntries) > 0 {
		if err := a.target.Apply(rd.CommittedEntries); err != nil {
			return rd, err
		}
	}

	a.r.msgsAfterAppend = nil
	a.r.Advance(rd)
	return rd, nil
}
