package proto_test

import (
	"context"
	"net"
	"testing"

	"github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	goproto "google.golang.org/protobuf/proto"
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
		Stage:               proto.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW,
		LastError:           "",
	}, nil
}

func (s *mockSpeculateServer) ListEnrollments(ctx context.Context, req *proto.ListEnrollmentsRequest) (*proto.ListEnrollmentsResponse, error) {
	return &proto.ListEnrollmentsResponse{
		Records: []*proto.EnrollmentRecord{
			{
				Repository:          "brotherlogic/speculate-alpha",
				AlignmentPercentage: 0,
				IssueUrl:            "https://github.com/brotherlogic/speculate-alpha/issues/1",
				EnrolledAtUnix:      1700000000,
				LastEvaluatedAtUnix: 1700000100,
				StatusMessage:       "Awaiting seed proto and spec files",
				Stage:               proto.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED,
			},
			{
				Repository:          "brotherlogic/speculate-kv",
				AlignmentPercentage: 100,
				IssueUrl:            "",
				EnrolledAtUnix:      1700000200,
				LastEvaluatedAtUnix: 1700000900,
				StatusMessage:       "Aligned",
				Stage:               proto.LifecycleStage_LIFECYCLE_STAGE_ALIGNED,
				LastError:           "transient rate limit warning",
			},
		},
	}, nil
}

func TestLifecycleStageEnum(t *testing.T) {
	cases := []struct {
		stage proto.LifecycleStage
		num   int32
		name  string
	}{
		{proto.LifecycleStage_LIFECYCLE_STAGE_UNSPECIFIED, 0, "LIFECYCLE_STAGE_UNSPECIFIED"},
		{proto.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED, 1, "LIFECYCLE_STAGE_AWAITING_SEED"},
		{proto.LifecycleStage_LIFECYCLE_STAGE_EVALUATING, 2, "LIFECYCLE_STAGE_EVALUATING"},
		{proto.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW, 3, "LIFECYCLE_STAGE_AWAITING_TEST_REVIEW"},
		{proto.LifecycleStage_LIFECYCLE_STAGE_ALIGNING, 4, "LIFECYCLE_STAGE_ALIGNING"},
		{proto.LifecycleStage_LIFECYCLE_STAGE_STALLED, 5, "LIFECYCLE_STAGE_STALLED"},
		{proto.LifecycleStage_LIFECYCLE_STAGE_ALIGNED, 6, "LIFECYCLE_STAGE_ALIGNED"},
	}

	for _, tc := range cases {
		if int32(tc.stage) != tc.num {
			t.Errorf("stage %s: expected number %d, got %d", tc.name, tc.num, int32(tc.stage))
		}
		if tc.stage.String() != tc.name {
			t.Errorf("stage %d: expected String() %q, got %q", tc.num, tc.name, tc.stage.String())
		}

		orig := &proto.EnrollmentRecord{
			Repository: "brotherlogic/speculate-kv",
			Stage:      tc.stage,
		}
		raw, err := goproto.Marshal(orig)
		if err != nil {
			t.Fatalf("Marshal failed for stage %s: %v", tc.name, err)
		}
		var decoded proto.EnrollmentRecord
		if err := goproto.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("Unmarshal failed for stage %s: %v", tc.name, err)
		}
		if decoded.GetStage() != tc.stage {
			t.Errorf("round-trip stage mismatch: expected %v, got %v", tc.stage, decoded.GetStage())
		}
	}
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
		Stage:               proto.LifecycleStage_LIFECYCLE_STAGE_ALIGNING,
		LastError:           "temporary github timeout",
	}
	respBytes, err := goproto.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal EnrollResponse: %v", err)
	}
	var decodedResp proto.EnrollResponse
	if err := goproto.Unmarshal(respBytes, &decodedResp); err != nil {
		t.Fatalf("failed to unmarshal EnrollResponse: %v", err)
	}
	if decodedResp.GetRepository() != "brotherlogic/speculate-kv" ||
		decodedResp.GetAlignmentPercentage() != 80 ||
		decodedResp.GetIssueUrl() != "https://github.com/brotherlogic/speculate-kv/issues/1" ||
		decodedResp.GetStatusMessage() != "Ready for dispatch" ||
		decodedResp.GetStage() != proto.LifecycleStage_LIFECYCLE_STAGE_ALIGNING ||
		decodedResp.GetLastError() != "temporary github timeout" {
		t.Fatalf("unexpected EnrollResponse fields after round-trip: %+v", &decodedResp)
	}

	rec := &proto.EnrollmentRecord{
		Repository:          "brotherlogic/speculate-kv",
		AlignmentPercentage: 80,
		IssueUrl:            "https://github.com/brotherlogic/speculate-kv/issues/1",
		EnrolledAtUnix:      1700000000,
		LastEvaluatedAtUnix: 1700000500,
		StatusMessage:       "Enrolled",
		Stage:               proto.LifecycleStage_LIFECYCLE_STAGE_STALLED,
		LastError:           "ollama unreachable",
	}
	recBytes, err := goproto.Marshal(rec)
	if err != nil {
		t.Fatalf("failed to marshal EnrollmentRecord: %v", err)
	}
	var decodedRec proto.EnrollmentRecord
	if err := goproto.Unmarshal(recBytes, &decodedRec); err != nil {
		t.Fatalf("failed to unmarshal EnrollmentRecord: %v", err)
	}
	if decodedRec.GetRepository() != "brotherlogic/speculate-kv" ||
		decodedRec.GetAlignmentPercentage() != 80 ||
		decodedRec.GetIssueUrl() != "https://github.com/brotherlogic/speculate-kv/issues/1" ||
		decodedRec.GetEnrolledAtUnix() != 1700000000 ||
		decodedRec.GetLastEvaluatedAtUnix() != 1700000500 ||
		decodedRec.GetStatusMessage() != "Enrolled" ||
		decodedRec.GetStage() != proto.LifecycleStage_LIFECYCLE_STAGE_STALLED ||
		decodedRec.GetLastError() != "ollama unreachable" {
		t.Fatalf("unexpected EnrollmentRecord fields after round-trip: %+v", &decodedRec)
	}

	listResp := &proto.ListEnrollmentsResponse{
		Records: []*proto.EnrollmentRecord{rec},
	}
	listBytes, err := goproto.Marshal(listResp)
	if err != nil {
		t.Fatalf("failed to marshal ListEnrollmentsResponse: %v", err)
	}
	var decodedList proto.ListEnrollmentsResponse
	if err := goproto.Unmarshal(listBytes, &decodedList); err != nil {
		t.Fatalf("failed to unmarshal ListEnrollmentsResponse: %v", err)
	}
	if len(decodedList.GetRecords()) != 1 {
		t.Fatalf("expected 1 record in ListEnrollmentsResponse, got %d", len(decodedList.GetRecords()))
	}
	if decodedList.GetRecords()[0].GetStage() != proto.LifecycleStage_LIFECYCLE_STAGE_STALLED ||
		decodedList.GetRecords()[0].GetLastError() != "ollama unreachable" {
		t.Fatalf("unexpected record in decoded ListEnrollmentsResponse: %+v", decodedList.GetRecords()[0])
	}

	// Verify wire compatibility when stage and last_error are omitted (legacy pstore records).
	legacyRec := &proto.EnrollmentRecord{
		Repository:          "brotherlogic/legacy-repo",
		AlignmentPercentage: 50,
		IssueUrl:            "https://github.com/brotherlogic/legacy-repo/issues/2",
		EnrolledAtUnix:      1690000000,
		LastEvaluatedAtUnix: 1690000100,
		StatusMessage:       "Legacy record",
	}
	legacyBytes, err := goproto.Marshal(legacyRec)
	if err != nil {
		t.Fatalf("failed to marshal legacy EnrollmentRecord: %v", err)
	}
	var decodedLegacy proto.EnrollmentRecord
	if err := goproto.Unmarshal(legacyBytes, &decodedLegacy); err != nil {
		t.Fatalf("failed to unmarshal legacy EnrollmentRecord: %v", err)
	}
	if decodedLegacy.GetStage() != proto.LifecycleStage_LIFECYCLE_STAGE_UNSPECIFIED ||
		decodedLegacy.GetLastError() != "" ||
		decodedLegacy.GetRepository() != "brotherlogic/legacy-repo" ||
		decodedLegacy.GetStatusMessage() != "Legacy record" {
		t.Fatalf("unexpected decoded legacy EnrollmentRecord: %+v", &decodedLegacy)
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
	if resp.GetStage() != proto.LifecycleStage_LIFECYCLE_STAGE_AWAITING_TEST_REVIEW {
		t.Errorf("expected stage LIFECYCLE_STAGE_AWAITING_TEST_REVIEW, got %v", resp.GetStage())
	}
	if resp.GetLastError() != "" {
		t.Errorf("expected empty last_error, got %q", resp.GetLastError())
	}

	listResp, err := client.ListEnrollments(ctx, &proto.ListEnrollmentsRequest{})
	if err != nil {
		t.Fatalf("ListEnrollments RPC failed: %v", err)
	}
	records := listResp.GetRecords()
	if len(records) != 2 {
		t.Fatalf("expected 2 enrollment records, got %d", len(records))
	}
	if records[0].GetRepository() != "brotherlogic/speculate-alpha" ||
		records[0].GetStage() != proto.LifecycleStage_LIFECYCLE_STAGE_AWAITING_SEED {
		t.Errorf("unexpected first record: %+v", records[0])
	}
	if records[1].GetRepository() != "brotherlogic/speculate-kv" ||
		records[1].GetAlignmentPercentage() != 100 ||
		records[1].GetStage() != proto.LifecycleStage_LIFECYCLE_STAGE_ALIGNED ||
		records[1].GetLastError() != "transient rate limit warning" {
		t.Errorf("unexpected second record: %+v", records[1])
	}
}
