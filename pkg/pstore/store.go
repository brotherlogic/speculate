package pstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pb "github.com/brotherlogic/speculate/proto"
)

var (
	// ErrNotFound is returned when an enrollment record does not exist in storage.
	ErrNotFound = errors.New("enrollment record not found")
)

// Store defines persistence operations for repository enrollment records.
type Store interface {
	SaveEnrollment(ctx context.Context, record *pb.EnrollmentRecord) error
	GetEnrollment(ctx context.Context, repo string) (*pb.EnrollmentRecord, error)
}

// NormalizeRepo strips leading/trailing slashes and common prefixes.
func NormalizeRepo(repo string) string {
	r := strings.TrimSpace(repo)
	r = strings.Trim(r, "/")
	r = strings.TrimPrefix(r, "speculate/enrollment/")
	return strings.Trim(r, "/")
}

// EnrollmentKey constructs the canonical pstore storage key for a repository enrollment record.
// Format: speculate/enrollment/<owner>/<repo>
func EnrollmentKey(repo string) string {
	return fmt.Sprintf("speculate/enrollment/%s", NormalizeRepo(repo))
}
