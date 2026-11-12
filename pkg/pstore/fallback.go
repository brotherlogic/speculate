package pstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	pb "github.com/brotherlogic/speculate/proto"
)

// FallbackStore wraps a primary Store (such as *Client) with a thread-safe in-memory
// *MockStore so that reads, writes, lists, and deletes degrade gracefully in memory
// if the remote pstore service is unreachable at startup or mid-run.
type FallbackStore struct {
	primary  Store
	fallback *MockStore
}

// NewFallbackStore creates a FallbackStore backed by primary and fallback.
// If fallback is nil, a new MockStore is initialized automatically.
func NewFallbackStore(primary Store, fallback *MockStore) *FallbackStore {
	if fallback == nil {
		fallback = NewMockStore()
	}
	return &FallbackStore{
		primary:  primary,
		fallback: fallback,
	}
}

// SaveEnrollment validates and writes record to the in-memory fallback store first,
// then attempts to persist it to the primary store if configured.
func (f *FallbackStore) SaveEnrollment(ctx context.Context, record *pb.EnrollmentRecord) error {
	if err := f.fallback.SaveEnrollment(ctx, record); err != nil {
		return err
	}
	if f.primary != nil {
		if err := f.primary.SaveEnrollment(ctx, record); err != nil {
			log.Printf("Warning: primary pstore SaveEnrollment failed for %s; using in-memory fallback: %v", record.GetRepository(), err)
		}
	}
	return nil
}

// DeleteEnrollment validates repo and deletes the record from the in-memory fallback store first,
// then attempts to delete it from the primary store if configured.
func (f *FallbackStore) DeleteEnrollment(ctx context.Context, repo string) error {
	if err := f.fallback.DeleteEnrollment(ctx, repo); err != nil {
		return err
	}
	if f.primary != nil {
		if err := f.primary.DeleteEnrollment(ctx, repo); err != nil {
			log.Printf("Warning: primary pstore DeleteEnrollment failed for %s; using in-memory fallback: %v", repo, err)
		}
	}
	return nil
}

// GetEnrollment reads from the primary store when available, hydrating the in-memory
// fallback cache on success, propagating ErrNotFound, and serving from the fallback
// cache if the primary store returns a transport/RPC failure.
func (f *FallbackStore) GetEnrollment(ctx context.Context, repo string) (*pb.EnrollmentRecord, error) {
	if strings.TrimSpace(repo) == "" || NormalizeRepo(repo) == "" {
		return nil, fmt.Errorf("repository cannot be empty")
	}

	if f.primary != nil {
		rec, err := f.primary.GetEnrollment(ctx, repo)
		if err == nil {
			_ = f.fallback.SaveEnrollment(ctx, rec)
			return rec, nil
		}
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		log.Printf("Warning: primary pstore GetEnrollment failed for %s; serving from in-memory fallback: %v", repo, err)
	}

	return f.fallback.GetEnrollment(ctx, repo)
}

// ListEnrollments lists records from the primary store when available, hydrating the
// in-memory fallback cache on success, and serving from the fallback cache if the
// primary store returns a transport/RPC failure.
func (f *FallbackStore) ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
	if f.primary != nil {
		records, err := f.primary.ListEnrollments(ctx)
		if err == nil {
			for _, rec := range records {
				_ = f.fallback.SaveEnrollment(ctx, rec)
			}
			return records, nil
		}
		log.Printf("Warning: primary pstore ListEnrollments failed; serving from in-memory fallback: %v", err)
	}

	return f.fallback.ListEnrollments(ctx)
}
