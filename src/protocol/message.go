package protocol

import (
	"encoding/binary"
	"errors"
	"math"
)

var (
	ErrInvalidMessage = errors.New("protocol: invalid message")
	ErrUnknownOp      = errors.New("protocol: unknown op")
	ErrUnknownStatus  = errors.New("protocol: unknown status")
)

const (
	u8Size  = 1
	u32Size = 4
	u64Size = 8

	// Request payload: [op u8][klen u32][vlen u32][key][value]
	// For OpPut: [op u8][klen u32][vlen u32][seq u64][key][value]
	requestHeaderSize    = u8Size + u32Size + u32Size
	requestHeaderSizePut = requestHeaderSize + u64Size // includes seq
	requestOpOff         = 0
	requestKLenOff       = requestOpOff + u8Size
	requestVLenOff       = requestKLenOff + u32Size
	requestSeqOff        = requestVLenOff + u32Size // only for OpPut
	requestKeyOff        = requestHeaderSize
	requestKeyOffPut     = requestHeaderSizePut // key starts after seq for OpPut

	// Response payload: [status u8][vlen u32][value]
	responseHeaderSize = u8Size + u32Size
	responseStatusOff  = 0
	responseVLenOff    = responseStatusOff + u8Size
	responseValueOff   = responseHeaderSize
)

type Op uint8

const (
	OpGet       Op = 1
	OpPut       Op = 2
	OpReplicate Op = 3
	OpPing      Op = 4
)

type Status uint8

const (
	StatusOK       Status = 0
	StatusNotFound Status = 1
	StatusError    Status = 2
	StatusPong     Status = 4
)

type Request struct {
	Op    Op
	Key   []byte
	Value []byte // only for OpPut
	Seq   uint64 // sequence number for replication (only for OpPut)
}

type Response struct {
	Status Status
	Value  []byte
}

func lenFromU32(u uint32) (int, bool) {
	// On 32-bit platforms, converting a uint32 greater than MaxInt wraps and can go negative.
	// Keep this safe and explicit.
	if uint64(u) > uint64(math.MaxInt) {
		return 0, false
	}
	return int(u), true
}

func ensureU32Len(n int) error {
	if uint64(n) > uint64(^uint32(0)) {
		return ErrFrameTooLarge
	}
	return nil
}

func putU32LE(buf []byte, off int, v int) {
	binary.LittleEndian.PutUint32(buf[off:off+u32Size], uint32(v))
}

func putU64LE(buf []byte, off int, v uint64) {
	binary.LittleEndian.PutUint64(buf[off:off+u64Size], v)
}

// EncodeRequest encodes a Request into a payload (without the outer frameLen).
//
// Request payload format:
//
//	[op uint8][klen uint32 LE][vlen uint32 LE][key bytes][value bytes]
//
// For OpPut, includes seq for replication:
//
//	[op uint8][klen uint32 LE][vlen uint32 LE][seq uint64 LE][key bytes][value bytes]
//
// For OpGet, vlen must be 0 and value bytes must be omitted.
func EncodeRequest(req Request) ([]byte, error) {
	klen := len(req.Key)
	if err := ensureU32Len(klen); err != nil {
		return nil, err
	}
	vlen := len(req.Value)
	if err := ensureU32Len(vlen); err != nil {
		return nil, err
	}

	switch req.Op {
	case OpGet:
		if vlen != 0 {
			return nil, ErrInvalidMessage
		}
		buf := make([]byte, requestHeaderSize+klen)
		buf[requestOpOff] = byte(req.Op)
		putU32LE(buf, requestKLenOff, klen)
		putU32LE(buf, requestVLenOff, 0)
		copy(buf[requestKeyOff:], req.Key)
		return buf, nil

	case OpPut:
		buf := make([]byte, requestHeaderSizePut+klen+vlen)
		buf[requestOpOff] = byte(req.Op)
		putU32LE(buf, requestKLenOff, klen)
		putU32LE(buf, requestVLenOff, vlen)
		putU64LE(buf, requestSeqOff, req.Seq)
		copy(buf[requestKeyOffPut:requestKeyOffPut+klen], req.Key)
		copy(buf[requestKeyOffPut+klen:], req.Value)
		return buf, nil

	case OpReplicate:
		// Replicate carries seq (replica's last applied seq) but no key/value.
		if klen != 0 || vlen != 0 {
			return nil, ErrInvalidMessage
		}
		buf := make([]byte, requestHeaderSize+u64Size)
		buf[requestOpOff] = byte(req.Op)
		putU32LE(buf, requestKLenOff, 0)
		putU32LE(buf, requestVLenOff, 0)
		putU64LE(buf, requestKeyOff, req.Seq) // seq follows header since no key/value
		return buf, nil

	case OpPing:
		buf := make([]byte, requestHeaderSize)
		buf[requestOpOff] = byte(req.Op)
		putU32LE(buf, requestKLenOff, 0)
		putU32LE(buf, requestVLenOff, 0)
		return buf, nil

	default:
		return nil, ErrUnknownOp
	}
}

// DecodeRequest decodes a payload (without the outer frameLen) into a Request.
// It copies key/value bytes so the returned Request does not alias the input.
func DecodeRequest(payload []byte) (Request, error) {
	if len(payload) < requestHeaderSize {
		return Request{}, ErrInvalidMessage
	}

	op := Op(payload[requestOpOff])
	kU32 := binary.LittleEndian.Uint32(payload[requestKLenOff : requestKLenOff+u32Size])
	vU32 := binary.LittleEndian.Uint32(payload[requestVLenOff : requestVLenOff+u32Size])
	klen, ok := lenFromU32(kU32)
	if !ok {
		return Request{}, ErrInvalidMessage
	}
	vlen, ok := lenFromU32(vU32)
	if !ok {
		return Request{}, ErrInvalidMessage
	}

	switch op {
	case OpGet:
		if vlen != 0 {
			return Request{}, ErrInvalidMessage
		}
		need := requestHeaderSize + klen
		if len(payload) != need {
			return Request{}, ErrInvalidMessage
		}
		key := append([]byte(nil), payload[requestKeyOff:requestKeyOff+klen]...)
		return Request{Op: op, Key: key}, nil
	case OpPut:
		need := requestHeaderSizePut + klen + vlen
		if len(payload) != need {
			return Request{}, ErrInvalidMessage
		}
		seq := binary.LittleEndian.Uint64(payload[requestSeqOff : requestSeqOff+u64Size])
		key := append([]byte(nil), payload[requestKeyOffPut:requestKeyOffPut+klen]...)
		val := append([]byte(nil), payload[requestKeyOffPut+klen:need]...)
		return Request{Op: op, Key: key, Value: val, Seq: seq}, nil
	case OpReplicate:
		// Replicate carries seq (replica's last applied seq) but no key/value.
		if klen != 0 || vlen != 0 {
			return Request{}, ErrInvalidMessage
		}
		need := requestHeaderSize + u64Size
		if len(payload) != need {
			return Request{}, ErrInvalidMessage
		}
		seq := binary.LittleEndian.Uint64(payload[requestKeyOff : requestKeyOff+u64Size])
		return Request{Op: op, Seq: seq}, nil
	case OpPing:
		// Ping carries no data.
		if klen != 0 || vlen != 0 {
			return Request{}, ErrInvalidMessage
		}
		if len(payload) != requestHeaderSize {
			return Request{}, ErrInvalidMessage
		}
		return Request{Op: op}, nil
	default:
		return Request{}, ErrUnknownOp
	}
}

// EncodeResponse encodes a Response into a payload (without the outer frameLen).
//
// Response payload format (minimal):
//
//	[status uint8][vlen uint32 LE][value bytes]
func EncodeResponse(resp Response) ([]byte, error) {
	vlen := len(resp.Value)
	if err := ensureU32Len(vlen); err != nil {
		return nil, err
	}

	switch resp.Status {
	case StatusOK, StatusNotFound, StatusError, StatusPong:
		// ok
	default:
		return nil, ErrUnknownStatus
	}

	// Minimal format from 007: only OK responses may carry a value.
	if resp.Status != StatusOK && vlen != 0 {
		return nil, ErrInvalidMessage
	}

	buf := make([]byte, responseHeaderSize+vlen)
	buf[responseStatusOff] = byte(resp.Status)
	putU32LE(buf, responseVLenOff, vlen)
	copy(buf[responseValueOff:], resp.Value)
	return buf, nil
}

// DecodeResponse decodes a payload (without the outer frameLen) into a Response.
// It copies value bytes so the returned Response does not alias the input.
func DecodeResponse(payload []byte) (Response, error) {
	if len(payload) < responseHeaderSize {
		return Response{}, ErrInvalidMessage
	}
	st := Status(payload[responseStatusOff])
	vU32 := binary.LittleEndian.Uint32(payload[responseVLenOff : responseVLenOff+u32Size])
	vlen, ok := lenFromU32(vU32)
	if !ok {
		return Response{}, ErrInvalidMessage
	}
	need := responseHeaderSize + vlen
	if len(payload) != need {
		return Response{}, ErrInvalidMessage
	}

	switch st {
	case StatusOK, StatusNotFound, StatusError, StatusPong:
		// ok
	default:
		return Response{}, ErrUnknownStatus
	}

	// Minimal format from 007: only OK responses may carry a value.
	if st != StatusOK && vlen != 0 {
		return Response{}, ErrInvalidMessage
	}

	val := append([]byte(nil), payload[responseValueOff:need]...)
	return Response{Status: st, Value: val}, nil
}
