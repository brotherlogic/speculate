package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDetermineTargetPackages_LocalExample(t *testing.T) {
	serverPkg, protoPkg := determineTargetPackages("example")
	expectedServer := "github.com/brotherlogic/speculate/example/internal/server"
	expectedProto := "github.com/brotherlogic/speculate/example/proto/kv/v1"

	if serverPkg != expectedServer {
		t.Errorf("expected server package %q, got %q", expectedServer, serverPkg)
	}
	if protoPkg != expectedProto {
		t.Errorf("expected proto package %q, got %q", expectedProto, protoPkg)
	}
}

func TestDetermineTargetPackages_WithGoMod(t *testing.T) {
	tmpDir := t.TempDir()
	goModContent := "module github.com/testorg/testrepo\n\ngo 1.25.0\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	serverPkg, protoPkg := determineTargetPackages(tmpDir)
	expectedServer := "github.com/testorg/testrepo/internal/server"
	expectedProto := "github.com/testorg/testrepo/proto/kv/v1"

	if serverPkg != expectedServer {
		t.Errorf("expected server package %q, got %q", expectedServer, serverPkg)
	}
	if protoPkg != expectedProto {
		t.Errorf("expected proto package %q, got %q", expectedProto, protoPkg)
	}
}

func TestResolveTargetDir_ExplicitDir(t *testing.T) {
	tmpDir := t.TempDir()
	resolved, cleanup, err := resolveTargetDir(context.Background(), tmpDir, "", false)
	defer cleanup()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved != tmpDir {
		t.Errorf("expected %q, got %q", tmpDir, resolved)
	}
}

func TestResolveTargetDir_LocalExampleDefault(t *testing.T) {
	resolved, cleanup, err := resolveTargetDir(context.Background(), "", "", false)
	defer cleanup()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved != "example" && resolved != "../example" && resolved != "../../example" {
		t.Errorf("expected 'example' (or relative), got %q", resolved)
	}
}
