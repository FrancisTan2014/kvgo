package server

const SeqNotFound = -1

func (s *Server) writeBacklog(payload []byte) {
	s.replBacklog = append(s.replBacklog, payload)
	s.backlogSeqs = append(s.backlogSeqs, s.seq.Load())
}

func (s *Server) getSeqIndex(seq uint64) int {
	for i, v := range s.backlogSeqs {
		if v == seq {
			return i
		}
	}
	return SeqNotFound
}
