package fpkgen

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadTestDataFiles tests that all testdata files can be loaded correctly
// This validates Requirements 1.1, 1.2, 1.3, 1.4, 1.5

// loadTestIcon is a helper to load icons from testdata directory using URLIconSource
func loadTestIcon(path string) (*URLIconSource, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &URLIconSource{URL: "file://" + absPath}, nil
}

func TestLoadTestData_PNG(t *testing.T) {
	path := filepath.Join("testdata", "test.png")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("testdata/test.png not found, run 'go run testdata/generate_testdata.go' to create")
	}

	source, err := loadTestIcon(path)
	if err != nil {
		t.Fatalf("Failed to create icon source: %v", err)
	}

	img, err := source.Load()
	if err != nil {
		t.Fatalf("Failed to load PNG: %v", err)
	}

	bounds := img.Bounds()
	if bounds.Dx() != 32 || bounds.Dy() != 32 {
		t.Errorf("Expected 32x32 image, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestLoadTestData_JPEG(t *testing.T) {
	path := filepath.Join("testdata", "test.jpg")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("testdata/test.jpg not found, run 'go run testdata/generate_testdata.go' to create")
	}

	source, err := loadTestIcon(path)
	if err != nil {
		t.Fatalf("Failed to create icon source: %v", err)
	}

	img, err := source.Load()
	if err != nil {
		t.Fatalf("Failed to load JPEG: %v", err)
	}

	bounds := img.Bounds()
	if bounds.Dx() != 32 || bounds.Dy() != 32 {
		t.Errorf("Expected 32x32 image, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestLoadTestData_BMP(t *testing.T) {
	path := filepath.Join("testdata", "test.bmp")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("testdata/test.bmp not found, run 'go run testdata/generate_testdata.go' to create")
	}

	source, err := loadTestIcon(path)
	if err != nil {
		t.Fatalf("Failed to create icon source: %v", err)
	}

	img, err := source.Load()
	if err != nil {
		t.Fatalf("Failed to load BMP: %v", err)
	}

	bounds := img.Bounds()
	if bounds.Dx() != 32 || bounds.Dy() != 32 {
		t.Errorf("Expected 32x32 image, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestLoadTestData_WebP(t *testing.T) {
	path := filepath.Join("testdata", "test.webp")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("testdata/test.webp not found, run 'go run testdata/generate_testdata.go' to create")
	}

	source, err := loadTestIcon(path)
	if err != nil {
		t.Fatalf("Failed to create icon source: %v", err)
	}

	img, err := source.Load()
	if err != nil {
		t.Fatalf("Failed to load WebP: %v", err)
	}

	// WebP test file is 1x1 (minimal valid WebP)
	bounds := img.Bounds()
	if bounds.Dx() < 1 || bounds.Dy() < 1 {
		t.Errorf("Expected valid image dimensions, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestLoadTestData_ICO(t *testing.T) {
	path := filepath.Join("testdata", "test.ico")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("testdata/test.ico not found, run 'go run testdata/generate_testdata.go' to create")
	}

	source, err := loadTestIcon(path)
	if err != nil {
		t.Fatalf("Failed to create icon source: %v", err)
	}

	img, err := source.Load()
	if err != nil {
		t.Fatalf("Failed to load ICO: %v", err)
	}

	bounds := img.Bounds()
	if bounds.Dx() != 32 || bounds.Dy() != 32 {
		t.Errorf("Expected 32x32 image, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestLoadTestData_ICOMulti(t *testing.T) {
	path := filepath.Join("testdata", "test_multi.ico")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("testdata/test_multi.ico not found, run 'go run testdata/generate_testdata.go' to create")
	}

	source, err := loadTestIcon(path)
	if err != nil {
		t.Fatalf("Failed to create icon source: %v", err)
	}

	img, err := source.Load()
	if err != nil {
		t.Fatalf("Failed to load multi-resolution ICO: %v", err)
	}

	// Should return the largest image (64x64)
	bounds := img.Bounds()
	if bounds.Dx() != 64 || bounds.Dy() != 64 {
		t.Errorf("Expected 64x64 (largest resolution), got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestLoadTestData_Invalid(t *testing.T) {
	path := filepath.Join("testdata", "invalid.bin")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("testdata/invalid.bin not found, run 'go run testdata/generate_testdata.go' to create")
	}

	source, err := loadTestIcon(path)
	if err != nil {
		t.Fatalf("Failed to create icon source: %v", err)
	}

	_, err = source.Load()
	if err == nil {
		t.Error("Expected error for invalid file, got nil")
	}
}

func TestDetectFormat_TestDataFiles(t *testing.T) {
	tests := []struct {
		filename string
		expected ImageFormat
	}{
		{"test.png", FormatPNG},
		{"test.jpg", FormatJPEG},
		{"test.bmp", FormatBMP},
		{"test.webp", FormatWebP},
		{"test.ico", FormatICO},
		{"test_multi.ico", FormatICO},
		{"invalid.bin", FormatUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			path := filepath.Join("testdata", tt.filename)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				t.Skipf("testdata/%s not found", tt.filename)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("Failed to read file: %v", err)
			}

			format := detectFormat(data)
			if format != tt.expected {
				t.Errorf("detectFormat(%s) = %v, want %v", tt.filename, format, tt.expected)
			}
		})
	}
}
