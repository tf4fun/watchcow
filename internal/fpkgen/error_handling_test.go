package fpkgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
)

// **Feature: icon-format-support, Property 5: Invalid Input Error Handling**
// **Validates: Requirements 1.6, 2.5**
//
// For any byte slice that does not represent a valid image of a supported format,
// the image decoding functions SHALL return a non-nil error.

// **Feature: icon-format-support, Property 6: Non-existent File Error**
// **Validates: Requirements 3.7**
//
// For any file path that does not exist on the filesystem,
// the loadIcon function SHALL return an error indicating the file was not found.

// TestProperty_InvalidInputErrorHandling tests that invalid image data returns an error
// Property 5: For any byte slice that does not represent a valid image of a supported format,
// the decoding functions SHALL return a non-nil error.
func TestProperty_InvalidInputErrorHandling(t *testing.T) {
	f := func(data []byte) bool {
		// Skip if data happens to have valid magic bytes for a supported format
		// We want to test truly invalid data
		format := detectFormat(data)
		if format != FormatUnknown {
			// If it has valid magic bytes, it might be a valid image
			// We only want to test data that is definitely not a valid image
			return true
		}

		// Create a temp file with the invalid data
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "invalid.bin")
		if err := os.WriteFile(tmpFile, data, 0644); err != nil {
			t.Logf("Failed to write temp file: %v", err)
			return true // Skip this case
		}

		// URLIconSource should return an error for unknown format
		source := &URLIconSource{URL: "file://" + tmpFile}
		_, err := source.Load()
		if err == nil {
			t.Logf("Expected error for invalid data (len=%d), got nil", len(data))
			return false
		}

		// Error should indicate unsupported format
		return strings.Contains(err.Error(), "unsupported image format")
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("Property test failed: %v", err)
	}
}

// TestProperty_NonExistentFileError tests that non-existent files return an error
// Property 6: For any file path that does not exist on the filesystem,
// the loadIcon function SHALL return an error indicating the file was not found.
func TestProperty_NonExistentFileError(t *testing.T) {
	f := func(filename string) bool {
		// Skip empty filenames
		if filename == "" {
			return true
		}

		// Skip filenames with path separators to avoid creating complex paths
		if strings.ContainsAny(filename, "/\\") {
			return true
		}

		// Create a path that definitely doesn't exist
		tmpDir := t.TempDir()
		nonExistentPath := filepath.Join(tmpDir, "nonexistent_subdir", filename)

		// URLIconSource should return an error for non-existent file
		source := &URLIconSource{URL: "file://" + nonExistentPath}
		_, err := source.Load()
		if err == nil {
			t.Logf("Expected error for non-existent file %q, got nil", nonExistentPath)
			return false
		}

		// Error should indicate file read failure
		return strings.Contains(err.Error(), "failed to read file")
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("Property test failed: %v", err)
	}
}

// TestLoadLocalIcon_NonExistentFile tests URLIconSource with a non-existent file
func TestLoadLocalIcon_NonExistentFile(t *testing.T) {
	source := &URLIconSource{URL: "file:///nonexistent/path/to/icon.png"}
	_, err := source.Load()
	if err == nil {
		t.Error("Expected error for non-existent file, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read file") {
		t.Errorf("Error should contain 'failed to read file', got: %v", err)
	}
}

// TestLoadLocalIcon_InvalidFormat tests URLIconSource with invalid image data
func TestLoadLocalIcon_InvalidFormat(t *testing.T) {
	// Create a temp file with invalid data
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "invalid.bin")
	invalidData := []byte{0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, 0xDE, 0xF0}
	if err := os.WriteFile(tmpFile, invalidData, 0644); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}

	source := &URLIconSource{URL: "file://" + tmpFile}
	_, err := source.Load()
	if err == nil {
		t.Error("Expected error for invalid format, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported image format") {
		t.Errorf("Error should contain 'unsupported image format', got: %v", err)
	}
}

// TestLoadLocalIcon_EmptyFile tests URLIconSource with an empty file
func TestLoadLocalIcon_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "empty.bin")
	if err := os.WriteFile(tmpFile, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}

	source := &URLIconSource{URL: "file://" + tmpFile}
	_, err := source.Load()
	if err == nil {
		t.Error("Expected error for empty file, got nil")
	}
}

// TestLoadLocalIcon_CorruptedICO tests URLIconSource with corrupted ICO data
func TestLoadLocalIcon_CorruptedICO(t *testing.T) {
	// Create a temp file with ICO magic bytes but corrupted content
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "corrupted.ico")
	// ICO magic bytes followed by invalid data
	corruptedData := []byte{0x00, 0x00, 0x01, 0x00, 0xFF, 0xFF}
	if err := os.WriteFile(tmpFile, corruptedData, 0644); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}

	source := &URLIconSource{URL: "file://" + tmpFile}
	_, err := source.Load()
	if err == nil {
		t.Error("Expected error for corrupted ICO, got nil")
	}
}

// TestLoadIconFromSource_EmptySource tests loadIcon with empty source
func TestLoadIconFromSource_EmptySource(t *testing.T) {
	_, err := loadIcon("", "")
	if err == nil {
		t.Error("Expected error for empty source, got nil")
	}
	if !strings.Contains(err.Error(), "empty icon source") {
		t.Errorf("Error should contain 'empty icon source', got: %v", err)
	}
}

// TestLoadIconFromSource_UnsupportedScheme tests loadIcon with unsupported scheme
func TestLoadIconFromSource_UnsupportedScheme(t *testing.T) {
	_, err := loadIcon("ftp://example.com/icon.png", "")
	if err == nil {
		t.Error("Expected error for unsupported scheme, got nil")
	}
	if !strings.Contains(err.Error(), "unrecognized icon source format") {
		t.Errorf("Error should contain 'unrecognized icon source format', got: %v", err)
	}
}

// TestLoadIconFromSource_RelativePathNoBasePath tests loadIcon with relative path but no basePath
func TestLoadIconFromSource_RelativePathNoBasePath(t *testing.T) {
	_, err := loadIcon("file://icon.png", "")
	if err == nil {
		t.Error("Expected error for relative path without basePath, got nil")
	}
	if !strings.Contains(err.Error(), "relative path requires base path") {
		t.Errorf("Error should contain 'relative path requires base path', got: %v", err)
	}
}
