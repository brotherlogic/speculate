package pstore

import (
	"context"
	"fmt"
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
