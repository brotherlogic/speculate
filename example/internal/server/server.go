package server

import (
	pb "github.com/brotherlogic/speculate/example/proto/kv/v1"
)

// Server implements the KV service. In its initial skeleton state,
// all methods return codes.Unimplemented via UnimplementedKVServer.
type Server struct {
	pb.UnimplementedKVServer
}

// New creates an initial skeleton KV server.
func New() *Server {
	return &Server{}
}
