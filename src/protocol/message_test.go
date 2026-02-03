package protocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestRequest_Get_RoundTrip(t *testing.T) {
	req := Request{Cmd: CmdGet, Key: []byte("k")}
	payload, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	got, err := DecodeRequest(payload)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if got.Cmd != CmdGet || !bytes.Equal(got.Key, req.Key) || len(got.Value) != 0 {
		t.Fatalf("decoded mismatch: %+v", got)
	}
}

func TestRequest_Put_RoundTrip(t *testing.T) {
	req := Request{Cmd: CmdPut, Key: []byte("k"), Value: []byte("v")}
	payload, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	got, err := DecodeRequest(payload)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if got.Cmd != CmdPut || !bytes.Equal(got.Key, req.Key) || !bytes.Equal(got.Value, req.Value) {
		t.Fatalf("decoded mismatch: %+v", got)
	}
}

func TestRequest_GetRejectsValue(t *testing.T) {
	_, err := EncodeRequest(Request{Cmd: CmdGet, Key: []byte("k"), Value: []byte("v")})
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("expected ErrInvalidMessage, got %v", err)
	}
}

func TestResponse_RoundTrip(t *testing.T) {
	resp := Response{Status: StatusOK, Value: []byte("hello")}
	payload, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	got, err := DecodeResponse(payload)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.Status != resp.Status || !bytes.Equal(got.Value, resp.Value) {
		t.Fatalf("decoded mismatch: %+v", got)
	}
}

func TestResponse_NotFoundHasNoValue(t *testing.T) {
	_, err := EncodeResponse(Response{Status: StatusNotFound, Value: []byte("no")})
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("expected ErrInvalidMessage, got %v", err)
	}
}

func TestResponse_ErrorHasNoValue(t *testing.T) {
	_, err := EncodeResponse(Response{Status: StatusError, Value: []byte("boom")})
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("expected ErrInvalidMessage, got %v", err)
	}
}
