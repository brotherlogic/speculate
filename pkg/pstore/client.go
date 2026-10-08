package pstore

import (
	"context"
	"fmt"
	"strings"

	pstorepb "github.com/brotherlogic/pstore/proto"
	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Client interacts with brotherlogic/pstore to persist and retrieve enrollment records.
type Client struct {
	conn         *grpc.ClientConn
	pstoreClient pstorepb.PStoreServiceClient
}

// NewClient creates a new Client using an existing PStoreServiceClient.
func NewClient(pstoreClient pstorepb.PStoreServiceClient) *Client {
	return &Client{
		pstoreClient: pstoreClient,
	}
}

// NewFromClientConn creates a new Client using an existing grpc.ClientConn.
func NewFromClientConn(conn *grpc.ClientConn) *Client {
	return &Client{
		conn:         conn,
		pstoreClient: pstorepb.NewPStoreServiceClient(conn),
	}
}

// Dial establishes a new gRPC connection to the pstore service at target.
func Dial(ctx context.Context, target string, opts ...grpc.DialOption) (*Client, error) {
	if len(opts) == 0 {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial pstore service at %s: %w", target, err)
	}

	return NewFromClientConn(conn), nil
}

// Close closes the underlying gRPC connection, if opened by Dial.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// SaveEnrollment serializes and saves an EnrollmentRecord to pstore.
func (c *Client) SaveEnrollment(ctx context.Context, record *pb.EnrollmentRecord) error {
	if record == nil {
		return fmt.Errorf("record cannot be nil")
	}
	if strings.TrimSpace(record.GetRepository()) == "" {
		return fmt.Errorf("record repository cannot be empty")
	}

	anyVal, err := anypb.New(record)
	if err != nil {
		return fmt.Errorf("failed to marshal EnrollmentRecord into Any: %w", err)
	}

	key := EnrollmentKey(record.GetRepository())
	req := &pstorepb.WriteRequest{
		Key:   key,
		Value: anyVal,
	}

	if _, err := c.pstoreClient.Write(ctx, req); err != nil {
		return fmt.Errorf("failed to write enrollment record for %s: %w", record.GetRepository(), err)
	}

	return nil
}

// GetEnrollment retrieves and deserializes an EnrollmentRecord from pstore.
func (c *Client) GetEnrollment(ctx context.Context, repo string) (*pb.EnrollmentRecord, error) {
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("repository cannot be empty")
	}

	key := EnrollmentKey(repo)
	req := &pstorepb.ReadRequest{
		Key: key,
	}

	resp, err := c.pstoreClient.Read(ctx, req)
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
			return nil, ErrNotFound
		}
		if strings.Contains(strings.ToLower(err.Error()), "not found") ||
			strings.Contains(strings.ToLower(err.Error()), "unable to locate") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to read enrollment record for %s: %w", repo, err)
	}

	if resp == nil || resp.GetValue() == nil || len(resp.GetValue().GetValue()) == 0 {
		return nil, ErrNotFound
	}

	record := &pb.EnrollmentRecord{}
	if err := resp.GetValue().UnmarshalTo(record); err != nil {
		// Fallback to direct proto unmarshaling in case Any TypeUrl is empty or unregistered
		if unmarshalErr := proto.Unmarshal(resp.GetValue().GetValue(), record); unmarshalErr != nil {
			return nil, fmt.Errorf("failed to unmarshal EnrollmentRecord: %w", err)
		}
	}

	return record, nil
}
