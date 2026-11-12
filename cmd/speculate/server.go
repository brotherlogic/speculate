package main

import (
	"context"
	"strings"

	pb "github.com/brotherlogic/speculate/proto"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// EnrollmentPipeline specifies the interface required to enroll and list repositories in Speculate.
type EnrollmentPipeline interface {
	Enroll(ctx context.Context, repo string) (*pb.EnrollResponse, error)
	ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error)
}

// SpeculateServer implements pb.SpeculateServiceServer with singleflight deduplication.
type SpeculateServer struct {
	pb.UnimplementedSpeculateServiceServer
	singleflight.Group
	pipeline EnrollmentPipeline
}

// NewSpeculateServer creates a new SpeculateServer backed by an EnrollmentPipeline.
func NewSpeculateServer(pipeline EnrollmentPipeline) *SpeculateServer {
	return &SpeculateServer{
		pipeline: pipeline,
	}
}

// ListEnrollments returns all currently tracked repository enrollment records.
func (s *SpeculateServer) ListEnrollments(ctx context.Context, req *pb.ListEnrollmentsRequest) (*pb.ListEnrollmentsResponse, error) {
	if s == nil || s.pipeline == nil {
		return nil, status.Error(codes.Internal, "enrollment pipeline is not configured")
	}

	records, err := s.pipeline.ListEnrollments(ctx)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Errorf(codes.Internal, "failed to list enrollments: %v", err)
	}

	return &pb.ListEnrollmentsResponse{
		Records: records,
	}, nil
}

// Enroll orchestrates the enrollment of a target repository, deduplicating concurrent calls
// for the same repository using singleflight.Group.
func (s *SpeculateServer) Enroll(ctx context.Context, req *pb.EnrollRequest) (*pb.EnrollResponse, error) {
	if req == nil || strings.TrimSpace(req.GetRepository()) == "" {
		return nil, status.Error(codes.InvalidArgument, "repository is required")
	}

	repo := strings.TrimSpace(req.GetRepository())
	res, err, _ := s.Do(repo, func() (any, error) {
		return s.pipeline.Enroll(ctx, repo)
	})
	if err != nil {
		return nil, err
	}

	resp, ok := res.(*pb.EnrollResponse)
	if !ok {
		return nil, status.Error(codes.Internal, "unexpected response type from pipeline")
	}

	return resp, nil
}
