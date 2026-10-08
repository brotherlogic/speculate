package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/brotherlogic/speculate/proto"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRunEnrollProberDryRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	targetRepo := "brotherlogic/speculate-kv"
	err := runEnrollProber(ctx, targetRepo, true, "", "")
	if err != nil {
		t.Fatalf("runEnrollProber in dry-run mode failed: %v", err)
	}

	// Verify alignment score metric was updated
	score := testutil.ToFloat64(specAlignmentScore.WithLabelValues(targetRepo))
	if score < 0 {
		t.Errorf("expected non-negative alignment score metric, got %f", score)
	}
}

func TestRunEnrollProber_InvalidTargetRepo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runEnrollProber(ctx, "invalid-repo", true, "", "")
	if err == nil {
		t.Fatal("expected error for invalid repository format, got nil")
	}
}

func TestEnrollServer_Validation(t *testing.T) {
	ctx := context.Background()
	server := &enrollServer{pipeline: nil}

	// Nil request
	_, err := server.Enroll(ctx, nil)
	if err == nil {
		t.Fatal("expected error on nil request, got nil")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument code, got %v", err)
	}

	// Empty repository
	_, err = server.Enroll(ctx, &pb.EnrollRequest{Repository: "   "})
	if err == nil {
		t.Fatal("expected error on empty repository, got nil")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument code, got %v", err)
	}
}

func TestCopyDir(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	subDir := filepath.Join(src, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create sub dir: %v", err)
	}
	file1 := filepath.Join(src, "file1.txt")
	if err := os.WriteFile(file1, []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to write file1: %v", err)
	}
	file2 := filepath.Join(subDir, "file2.txt")
	if err := os.WriteFile(file2, []byte("world"), 0644); err != nil {
		t.Fatalf("failed to write file2: %v", err)
	}

	target := filepath.Join(dst, "copied")
	if err := copyDir(src, target); err != nil {
		t.Fatalf("copyDir failed: %v", err)
	}

	data1, err := os.ReadFile(filepath.Join(target, "file1.txt"))
	if err != nil || string(data1) != "hello" {
		t.Errorf("expected file1.txt content 'hello', got %q (err: %v)", string(data1), err)
	}
	data2, err := os.ReadFile(filepath.Join(target, "sub", "file2.txt"))
	if err != nil || string(data2) != "world" {
		t.Errorf("expected file2.txt content 'world', got %q (err: %v)", string(data2), err)
	}
}
