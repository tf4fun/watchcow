package fpkgen

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IconSource represents an abstract icon source that can be loaded into an image.
//
// Two modes correspond to two configuration sources:
//   - URLIconSource: for label-based configuration (file:// or http(s):// URLs)
//   - Base64IconSource: for dashboard-based configuration (in-memory base64 data from upload)
type IconSource interface {
	// Load loads the icon and returns the decoded image.
	Load() (image.Image, error)

	// String returns a description of the icon source for logging.
	String() string
}

// URLIconSource loads an icon from a URL (file:// or http(s)://).
// Used by label-based configuration where the icon is specified as a URL
// in Docker labels (e.g., watchcow.icon=http://... or watchcow.icon=file://...).
type URLIconSource struct {
	URL      string
	BasePath string // Base directory for resolving relative file:// paths
}

// Load implements IconSource.Load for URL-based icons.
func (s *URLIconSource) Load() (image.Image, error) {
	if s.URL == "" {
		return nil, fmt.Errorf("empty URL")
	}

	if strings.HasPrefix(s.URL, "file://") {
		return s.loadFromFile()
	} else if strings.HasPrefix(s.URL, "http://") || strings.HasPrefix(s.URL, "https://") {
		return s.loadFromHTTP()
	}

	return nil, fmt.Errorf("unsupported URL scheme: %s", s.URL)
}

// String implements IconSource.String.
func (s *URLIconSource) String() string {
	return fmt.Sprintf("URL(%s)", s.URL)
}

// loadFromFile loads an icon from a file:// URL.
func (s *URLIconSource) loadFromFile() (image.Image, error) {
	path, err := resolveFilePath(s.URL, s.BasePath)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", path, err)
	}

	return decodeImageData(data)
}

// resolveFilePath resolves a file:// URL to an absolute filesystem path.
// Supports:
//   - Absolute paths: file:///path/to/file -> /path/to/file
//   - Relative paths: file://./icon.png -> basePath/./icon.png (requires basePath)
func resolveFilePath(fileURL string, basePath string) (string, error) {
	path := strings.TrimPrefix(fileURL, "file://")

	// Absolute path: starts with /
	if filepath.IsAbs(path) {
		return path, nil
	}

	// Relative path: requires basePath
	if basePath == "" {
		return "", fmt.Errorf("relative path requires base path: %s", path)
	}

	return filepath.Join(basePath, path), nil
}

// loadFromHTTP loads an icon from an HTTP(S) URL.
func (s *URLIconSource) loadFromHTTP() (image.Image, error) {
	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	resp, err := client.Get(s.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return decodeImageData(data)
}

// Base64IconSource loads an icon from base64 encoded image data.
// Used by dashboard-based configuration where the icon is uploaded via the web UI
// and stored as base64 in StoredConfig.IconBase64.
type Base64IconSource struct {
	Data string // Base64 encoded image data (raw, without data URI prefix)
}

// Load implements IconSource.Load for base64-based icons.
func (s *Base64IconSource) Load() (image.Image, error) {
	if s.Data == "" {
		return nil, fmt.Errorf("empty base64 data")
	}

	data, err := base64.StdEncoding.DecodeString(s.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}

	return decodeImageData(data)
}

// String implements IconSource.String.
func (s *Base64IconSource) String() string {
	if len(s.Data) > 20 {
		return fmt.Sprintf("Base64(%s...)", s.Data[:20])
	}
	return fmt.Sprintf("Base64(%s)", s.Data)
}

// ParseIconSource parses an icon source string and returns the appropriate IconSource.
//
// The source format depends on the configuration origin:
//   - Label config: URL string (file:// or http(s)://) → returns URLIconSource
//   - Dashboard config: raw base64 string (from icon upload) → returns Base64IconSource
//
// Returns nil if the source is empty.
func ParseIconSource(source string, basePath string) (IconSource, error) {
	if source == "" {
		return nil, nil
	}

	// URL-based source (from label config)
	if strings.HasPrefix(source, "file://") ||
		strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "https://") {
		return &URLIconSource{
			URL:      source,
			BasePath: basePath,
		}, nil
	}

	// Base64 encoded data (from dashboard upload)
	if isValidBase64(source) {
		return &Base64IconSource{
			Data: source,
		}, nil
	}

	return nil, fmt.Errorf("unrecognized icon source format")
}

// isValidBase64 checks if the string appears to be valid base64 encoded image data.
func isValidBase64(s string) bool {
	// Base64 encoded images are typically long
	if len(s) < 100 {
		return false
	}

	// Try to decode a portion to verify it's valid base64
	testLen := 100
	if len(s) < testLen {
		testLen = len(s)
	}

	// Ensure test length is multiple of 4 for base64
	testLen = (testLen / 4) * 4
	if testLen == 0 {
		return false
	}

	_, err := base64.StdEncoding.DecodeString(s[:testLen])
	return err == nil
}

// decodeImageData decodes raw image bytes into an image.Image.
// Supports PNG, JPEG, WebP, BMP, and ICO formats.
func decodeImageData(data []byte) (image.Image, error) {
	format := detectFormat(data)

	// Handle ICO format specially
	if format == FormatICO {
		return decodeICO(data)
	}

	// Check for unsupported formats
	if format == FormatUnknown {
		return nil, fmt.Errorf("unsupported image format")
	}

	// Use standard image.Decode for PNG, JPEG, WebP, BMP
	// (decoders registered via imports in icons.go)
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decode %s: %w", format, err)
	}

	return img, nil
}
