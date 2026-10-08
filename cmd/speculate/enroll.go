package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	pb "github.com/brotherlogic/speculate/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// runEnroll executes the CLI command `speculate enroll <owner/repo>` to dial the daemon
// and enroll the repository.
func runEnroll(ctx context.Context, args []string, stdout, stderr io.Writer, dialOpts ...grpc.DialOption) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(stdout)
	daemonAddr := fs.String("daemon-addr", "localhost:50051", "Speculate daemon gRPC address")

	fs.Usage = func() {
		fmt.Fprintln(stdout, "Usage: speculate enroll [options] <owner/repo>")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Enroll a target repository into Speculate, perform initial spec evaluation,")
		fmt.Fprintln(stdout, "update the README alignment badge, and dispatch the first scenario card.")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Options:")
		fs.PrintDefaults()
	}

	// Normalize arguments so flags can appear before or after positional repository
	var flagArgs, posArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if (arg == "--daemon-addr" || arg == "-daemon-addr") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			posArgs = append(posArgs, arg)
		}
	}

	if err := fs.Parse(append(flagArgs, posArgs...)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	repoArgs := fs.Args()
	if len(repoArgs) == 0 {
		fmt.Fprintln(stderr, "Error: repository in \"owner/repo\" format is required")
		fs.Usage()
		return errors.New("repository argument missing")
	}

	targetRepo := repoArgs[0]

	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, dialOpts...)

	conn, err := grpc.NewClient(*daemonAddr, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "Error: failed to dial daemon at %s: %v\n", *daemonAddr, err)
		return err
	}
	defer conn.Close()

	client := pb.NewSpeculateServiceClient(conn)
	resp, err := client.Enroll(ctx, &pb.EnrollRequest{
		Repository: targetRepo,
	})
	if err != nil {
		st, ok := status.FromError(err)
		if ok {
			switch st.Code() {
			case codes.InvalidArgument:
				fmt.Fprintf(stderr, "Error: Invalid argument: %s\n", st.Message())
			case codes.Unavailable:
				fmt.Fprintf(stderr, "Error: Speculate daemon is unavailable at %s: %s\n", *daemonAddr, st.Message())
			case codes.DeadlineExceeded:
				fmt.Fprintf(stderr, "Error: Request timed out: %s\n", st.Message())
			case codes.NotFound:
				fmt.Fprintf(stderr, "Error: Not found: %s\n", st.Message())
			default:
				fmt.Fprintf(stderr, "Error: %s (code: %s)\n", st.Message(), st.Code())
			}
		} else {
			fmt.Fprintf(stderr, "Error: %v\n", err)
		}
		return err
	}

	activeIssue := resp.GetIssueUrl()
	if activeIssue == "" {
		activeIssue = "None"
	}

	fmt.Fprintln(stdout, "Enrollment Status:")
	fmt.Fprintf(stdout, "  Repository:   %s\n", resp.GetRepository())
	fmt.Fprintf(stdout, "  Alignment:    %d%%\n", resp.GetAlignmentPercentage())
	fmt.Fprintf(stdout, "  Active Issue: %s\n", activeIssue)
	fmt.Fprintf(stdout, "  Status:       %s\n", resp.GetStatusMessage())

	return nil
}
