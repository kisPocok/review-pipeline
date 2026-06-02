package marker

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleHash = "3a2b9f4c8d1e5f6789abcdef0123456789abcdef"

func TestConsume_MarkerPresent_ReturnsTrueAndDeletes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	markerPath := filepath.Join(dir, sampleHash)
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	consumed, err := Consume(dir, sampleHash)
	if err != nil {
		t.Fatalf("Consume returned err: %v", err)
	}
	if !consumed {
		t.Fatalf("Consume returned false; want true")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("marker file still exists after Consume; want removed")
	}
}

func TestConsume_NoMarker_ReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	consumed, err := Consume(dir, sampleHash)
	if err != nil {
		t.Fatalf("Consume returned err: %v", err)
	}
	if consumed {
		t.Errorf("Consume returned true on empty dir; want false")
	}
}

func TestConsume_DirDoesNotExist_ReturnsFalse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")

	consumed, err := Consume(dir, sampleHash)
	if err != nil {
		t.Fatalf("Consume on missing dir returned err: %v", err)
	}
	if consumed {
		t.Errorf("Consume returned true on missing dir; want false")
	}
}

func TestConsume_IsSingleUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	markerPath := filepath.Join(dir, sampleHash)
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	first, err := Consume(dir, sampleHash)
	if err != nil {
		t.Fatalf("first Consume err: %v", err)
	}
	if !first {
		t.Fatalf("first Consume = false; want true")
	}

	second, err := Consume(dir, sampleHash)
	if err != nil {
		t.Fatalf("second Consume err: %v", err)
	}
	if second {
		t.Errorf("second Consume = true; want false (marker should be single-use)")
	}
}

func TestConsume_DirWithGroupWorldBits_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	markerPath := filepath.Join(dir, sampleHash)
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	consumed, err := Consume(dir, sampleHash)
	if err == nil {
		t.Fatalf("Consume returned nil err on mode 0o755 dir; want unsafe error")
	}
	if consumed {
		t.Errorf("Consume returned true on unsafe dir; want false")
	}
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Errorf("marker file should not have been removed on unsafe-dir error: %v", statErr)
	}
}

func TestEnsureDir_CreatesWithMode0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "markers")

	if err := EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("EnsureDir did not create a directory")
	}
	gotMode := info.Mode().Perm()
	if gotMode != 0o700 {
		t.Errorf("EnsureDir created dir with mode %o; want 0700", gotMode)
	}
}
