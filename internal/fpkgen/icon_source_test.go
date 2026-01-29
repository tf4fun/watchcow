package fpkgen

import (
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestURLIconSource_LoadFromFile(t *testing.T) {
	// Create a test PNG file
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.png")

	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	f, err := os.Create(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("Failed to encode test PNG: %v", err)
	}
	f.Close()

	source := &URLIconSource{URL: "file://" + tmpFile}
	loaded, err := source.Load()
	if err != nil {
		t.Fatalf("URLIconSource.Load() error = %v", err)
	}

	bounds := loaded.Bounds()
	if bounds.Dx() != 32 || bounds.Dy() != 32 {
		t.Errorf("Expected 32x32 image, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestURLIconSource_LoadFromFileRelative(t *testing.T) {
	// Create a test PNG file
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "icons")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}
	tmpFile := filepath.Join(subDir, "test.png")

	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	f, err := os.Create(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("Failed to encode test PNG: %v", err)
	}
	f.Close()

	source := &URLIconSource{
		URL:      "file://icons/test.png",
		BasePath: tmpDir,
	}
	loaded, err := source.Load()
	if err != nil {
		t.Fatalf("URLIconSource.Load() error = %v", err)
	}

	bounds := loaded.Bounds()
	if bounds.Dx() != 32 || bounds.Dy() != 32 {
		t.Errorf("Expected 32x32 image, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestURLIconSource_RelativePathRequiresBasePath(t *testing.T) {
	source := &URLIconSource{URL: "file://icon.png"}
	_, err := source.Load()
	if err == nil {
		t.Error("Expected error for relative path without basePath")
	}
	if !strings.Contains(err.Error(), "relative path requires base path") {
		t.Errorf("Expected 'relative path requires base path' error, got: %v", err)
	}
}

func TestURLIconSource_UnsupportedScheme(t *testing.T) {
	source := &URLIconSource{URL: "ftp://example.com/icon.png"}
	_, err := source.Load()
	if err == nil {
		t.Error("Expected error for unsupported scheme")
	}
	if !strings.Contains(err.Error(), "unsupported URL scheme") {
		t.Errorf("Expected 'unsupported URL scheme' error, got: %v", err)
	}
}

func TestURLIconSource_String(t *testing.T) {
	source := &URLIconSource{URL: "file:///path/to/icon.png"}
	got := source.String()
	if got != "URL(file:///path/to/icon.png)" {
		t.Errorf("URLIconSource.String() = %q, want %q", got, "URL(file:///path/to/icon.png)")
	}
}

func TestBase64IconSource_Load(t *testing.T) {
	// Create a test image and encode to base64
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	var buf strings.Builder
	encoder := base64.NewEncoder(base64.StdEncoding, &buf)
	if err := png.Encode(encoder, img); err != nil {
		t.Fatalf("Failed to encode test PNG: %v", err)
	}
	encoder.Close()

	source := &Base64IconSource{Data: buf.String()}
	loaded, err := source.Load()
	if err != nil {
		t.Fatalf("Base64IconSource.Load() error = %v", err)
	}

	bounds := loaded.Bounds()
	if bounds.Dx() != 32 || bounds.Dy() != 32 {
		t.Errorf("Expected 32x32 image, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestBase64IconSource_EmptyData(t *testing.T) {
	source := &Base64IconSource{Data: ""}
	_, err := source.Load()
	if err == nil {
		t.Error("Expected error for empty base64 data")
	}
	if !strings.Contains(err.Error(), "empty base64 data") {
		t.Errorf("Expected 'empty base64 data' error, got: %v", err)
	}
}

func TestBase64IconSource_InvalidBase64(t *testing.T) {
	source := &Base64IconSource{Data: "not-valid-base64!!!"}
	_, err := source.Load()
	if err == nil {
		t.Error("Expected error for invalid base64 data")
	}
}

func TestBase64IconSource_String(t *testing.T) {
	tests := []struct {
		data string
		want string
	}{
		{"short", "Base64(short)"},
		{"this is a longer string that exceeds twenty characters", "Base64(this is a longer str...)"},
	}

	for _, tt := range tests {
		source := &Base64IconSource{Data: tt.data}
		got := source.String()
		if got != tt.want {
			t.Errorf("Base64IconSource{%q}.String() = %q, want %q", tt.data, got, tt.want)
		}
	}
}

func TestParseIconSource_URL(t *testing.T) {
	tests := []struct {
		source   string
		wantType string
	}{
		{"file:///path/to/icon.png", "*fpkgen.URLIconSource"},
		{"http://example.com/icon.png", "*fpkgen.URLIconSource"},
		{"https://example.com/icon.png", "*fpkgen.URLIconSource"},
	}

	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			got, err := ParseIconSource(tt.source, "")
			if err != nil {
				t.Fatalf("ParseIconSource(%q) error = %v", tt.source, err)
			}
			gotType := strings.Split(strings.TrimPrefix(strings.TrimPrefix(got.String(), "URL("), "Base64("), ")")[0]
			_ = gotType // Just verify it parses without error

			switch got.(type) {
			case *URLIconSource:
				if tt.wantType != "*fpkgen.URLIconSource" {
					t.Errorf("ParseIconSource(%q) type = URLIconSource, want %s", tt.source, tt.wantType)
				}
			case *Base64IconSource:
				if tt.wantType != "*fpkgen.Base64IconSource" {
					t.Errorf("ParseIconSource(%q) type = Base64IconSource, want %s", tt.source, tt.wantType)
				}
			}
		})
	}
}

func TestParseIconSource_Base64(t *testing.T) {
	// Create valid base64 data (at least 100 chars)
	data := strings.Repeat("AAAA", 30) // 120 chars of valid base64

	got, err := ParseIconSource(data, "")
	if err != nil {
		t.Fatalf("ParseIconSource() error = %v", err)
	}

	if _, ok := got.(*Base64IconSource); !ok {
		t.Errorf("ParseIconSource() type = %T, want *Base64IconSource", got)
	}
}

func TestParseIconSource_Empty(t *testing.T) {
	got, err := ParseIconSource("", "")
	if err != nil {
		t.Fatalf("ParseIconSource(\"\") error = %v", err)
	}
	if got != nil {
		t.Errorf("ParseIconSource(\"\") = %v, want nil", got)
	}
}

func TestParseIconSource_Unrecognized(t *testing.T) {
	// Short string that's not a URL
	_, err := ParseIconSource("short", "")
	if err == nil {
		t.Error("Expected error for unrecognized format")
	}
	if !strings.Contains(err.Error(), "unrecognized icon source format") {
		t.Errorf("Expected 'unrecognized icon source format' error, got: %v", err)
	}
}

func TestIsValidBase64(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"short", false},                                  // Too short
		{strings.Repeat("AAAA", 30), true},                // Valid base64
		{strings.Repeat("!!!!", 30), false},               // Invalid characters
		{"", false},                                       // Empty
		{strings.Repeat("A", 99), false},                  // Just under 100
		{strings.Repeat("A", 100), true},                  // Exactly 100
		{strings.Repeat("A", 101) + "!", true},            // Only first 100 chars checked
		{strings.Repeat("AAAA", 25) + "====", true},       // With padding
	}

	for _, tt := range tests {
		t.Run(tt.input[:min(len(tt.input), 20)]+"...", func(t *testing.T) {
			got := isValidBase64(tt.input)
			if got != tt.want {
				t.Errorf("isValidBase64(%q...) = %v, want %v", tt.input[:min(len(tt.input), 20)], got, tt.want)
			}
		})
	}
}
