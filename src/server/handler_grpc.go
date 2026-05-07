package server

import (
	"context"

	"kvgo/kvpb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type grpcServer struct {
	kvpb.UnimplementedKVServer
	s *Server
}

func (g *grpcServer) Put(ctx context.Context, req *kvpb.PutRequest) (*kvpb.PutResponse, error) {
	if err := g.s.proposePut(string(req.Key), req.Value); err != nil {
		return nil, status.Errorf(codes.Internal, "put failed: %v", err)
	}
	return &kvpb.PutResponse{}, nil
}

func (g *grpcServer) Get(ctx context.Context, req *kvpb.GetRequest) (*kvpb.GetResponse, error) {
	if err := g.s.proposeRead(); err != nil {
		return nil, status.Errorf(codes.Internal, "read failed: %v", err)
	}

	key := string(req.Key)
	val, ok := g.s.sm.Get(key)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "key not found: %s", key)
	}

	copyVal := append([]byte(nil), val...)
	return &kvpb.GetResponse{Value: copyVal}, nil
}
