package proto_test

import (
	"context"
	"net"
	"testing"

	"github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type mockSpeculateServer struct {
	proto.UnimplementedSpeculateServiceServer
}

func (s *mockSpeculateServer) Enroll(ctx context.Context, req *proto.EnrollRequest) (*proto.EnrollResponse, error) {
	return &proto.EnrollResponse{
		Repository:          req.GetRepository(),
		AlignmentPercentage: 75,
		IssueUrl:            "https://github.com/brotherlogic/speculate-kv/issues/10",
		StatusMessage:       "Successfully enrolled repository",
	}, nil
}

func TestEnrollMessages(t *testing.T) {
	req := &proto.EnrollRequest{
		Repository: "brotherlogic/speculate-kv",
	}
	if req.GetRepository() != "brotherlogic/speculate-kv" {
		t.Fatalf("expected repository brotherlogic/speculate-kv, got %s", req.GetRepository())
	}

	resp := &proto.EnrollResponse{
		Repository:          "brotherlogic/speculate-kv",
		AlignmentPercentage: 80,
		IssueUrl:            "https://github.com/brotherlogic/speculate-kv/issues/1",
		StatusMessage:       "Ready for dispatch",
	}
	if resp.GetRepository() != "brotherlogic/speculate-kv" ||
		resp.GetAlignmentPercentage() != 80 ||
		resp.GetIssueUrl() != "https://github.com/brotherlogic/speculate-kv/issues/1" ||
		resp.GetStatusMessage() != "Ready for dispatch" {
		t.Fatalf("unexpected EnrollResponse fields: %+v", resp)
	}

	rec := &proto.EnrollmentRecord{
		Repository:          "brotherlogic/speculate-kv",
		AlignmentPercentage: 80,
		IssueUrl:            "https://github.com/brotherlogic/speculate-kv/issues/1",
		EnrolledAtUnix:      1700000000,
		LastEvaluatedAtUnix: 1700000500,
		StatusMessage:       "Enrolled",
	}
	if rec.GetRepository() != "brotherlogic/speculate-kv" ||
		rec.GetAlignmentPercentage() != 80 ||
		rec.GetIssueUrl() != "https://github.com/brotherlogic/speculate-kv/issues/1" ||
		rec.GetEnrolledAtUnix() != 1700000000 ||
		rec.GetLastEvaluatedAtUnix() != 1700000500 ||
		rec.GetStatusMessage() != "Enrolled" {
		t.Fatalf("unexpected EnrollmentRecord fields: %+v", rec)
	}
}

func TestSpeculateService_GRPC(t *testing.T) {
	bufferSize := 1024 * 1024
	lis := bufconn.Listen(bufferSize)

	server := grpc.NewServer()
	proto.RegisterSpeculateServiceServer(server, &mockSpeculateServer{})

	go func() {
		if err := server.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Logf("Server error: %v", err)
		}
	}()
	defer server.Stop()

	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("Failed to dial bufnet: %v", err)
	}
	defer conn.Close()

	client := proto.NewSpeculateServiceClient(conn)
	resp, err := client.Enroll(ctx, &proto.EnrollRequest{
		Repository: "brotherlogic/speculate-kv",
	})
	if err != nil {
		t.Fatalf("Enroll RPC failed: %v", err)
	}

	if resp.GetRepository() != "brotherlogic/speculate-kv" {
		t.Errorf("expected repository brotherlogic/speculate-kv, got %s", resp.GetRepository())
	}
	if resp.GetAlignmentPercentage() != 75 {
		t.Errorf("expected alignment percentage 75, got %d", resp.GetAlignmentPercentage())
	}
	if resp.GetIssueUrl() != "https://github.com/brotherlogic/speculate-kv/issues/10" {
		t.Errorf("expected issue URL https://github.com/brotherlogic/speculate-kv/issues/10, got %s", resp.GetIssueUrl())
	}
	if resp.GetStatusMessage() != "Successfully enrolled repository" {
		t.Errorf("expected status message 'Successfully enrolled repository', got %s", resp.GetStatusMessage())
	}
}
