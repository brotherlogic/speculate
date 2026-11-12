package pstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/protobuf/proto"
)

// MockStore provides a thread-safe in-memory implementation of Store.
type MockStore struct {
	mu      sync.RWMutex
	records map[string]*pb.EnrollmentRecord
}

// NewMockStore creates a new MockStore.
func NewMockStore() *MockStore {
	return &MockStore{
		records: make(map[string]*pb.EnrollmentRecord),
	}
}

// SaveEnrollment stores a deep copy of the EnrollmentRecord.
func (m *MockStore) SaveEnrollment(ctx context.Context, record *pb.EnrollmentRecord) error {
	if record == nil {
		return fmt.Errorf("record cannot be nil")
	}
	if strings.TrimSpace(record.GetRepository()) == "" {
		return fmt.Errorf("record repository cannot be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	norm := NormalizeRepo(record.GetRepository())
	m.records[norm] = proto.Clone(record).(*pb.EnrollmentRecord)
	return nil
}

// GetEnrollment retrieves a deep copy of the EnrollmentRecord for repo.
func (m *MockStore) GetEnrollment(ctx context.Context, repo string) (*pb.EnrollmentRecord, error) {
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("repository cannot be empty")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	norm := NormalizeRepo(repo)
	rec, ok := m.records[norm]
	if !ok {
		return nil, ErrNotFound
	}

	return proto.Clone(rec).(*pb.EnrollmentRecord), nil
}

// ListEnrollments returns deep copies of all stored EnrollmentRecords sorted ascending by repository.
func (m *MockStore) ListEnrollments(ctx context.Context) ([]*pb.EnrollmentRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	records := make([]*pb.EnrollmentRecord, 0, len(m.records))
	for _, rec := range m.records {
		records = append(records, proto.Clone(rec).(*pb.EnrollmentRecord))
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].GetRepository() < records[j].GetRepository()
	})

	return records, nil
}

// DeleteEnrollment removes the EnrollmentRecord for repo from memory, idempotently returning nil if absent.
func (m *MockStore) DeleteEnrollment(ctx context.Context, repo string) error {
	if strings.TrimSpace(repo) == "" || NormalizeRepo(repo) == "" {
		return fmt.Errorf("repository cannot be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.records, NormalizeRepo(repo))
	return nil
}
