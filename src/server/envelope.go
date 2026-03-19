package server

import (
	"encoding/binary"
	"errors"
)

const envelopeHeaderSize = 8

var errEnvelopeTooShort = errors.New("envelope too short: need at least 8 bytes")

// marshalEnvelope prepends an 8-byte big-endian request ID to the payload.
// Format: [8-byte ID][payload]
func marshalEnvelope(id uint64, payload []byte) []byte {
	buf := make([]byte, envelopeHeaderSize+len(payload))
	binary.BigEndian.PutUint64(buf, id)
	copy(buf[envelopeHeaderSize:], payload)
	return buf
}

// unmarshalEnvelope extracts the request ID and payload from an envelope.
// Returns an error if the data is shorter than the 8-byte header.
func unmarshalEnvelope(data []byte) (uint64, []byte, error) {
	if len(data) < envelopeHeaderSize {
		return 0, nil, errEnvelopeTooShort
	}
	id := binary.BigEndian.Uint64(data[:envelopeHeaderSize])
	return id, data[envelopeHeaderSize:], nil
}
