package fpkgen

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"

	// Register image format decoders for multi-format support
	_ "image/jpeg"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

//go:embed defaults/ICON.PNG defaults/ICON_256.PNG
var defaultIcons embed.FS

// handleIcons downloads/generates and saves all required icon files for all entries
func (g *Generator) handleIcons(appDir string, config *AppConfig) error {
	var defaultIcon image.Image

	// Get base path from container labels for resolving relative file:// paths
	basePath := getBasePath(config.Labels)

	// Process each entry's icon
	for _, entry := range config.Entries {
		entryIcon, err := loadIcon(entry.Icon, basePath)
		if err != nil && entry.Icon != "" {
			fmt.Printf("Warning: Failed to load icon for entry '%s': %v\n", entry.Name, err)
		}

		// Use default icon if loading failed
		if entryIcon == nil {
			if defaultIcon == nil {
				defaultIcon, err = loadDefaultIcon()
				if err != nil {
					return fmt.Errorf("failed to load default icon: %w", err)
				}
			}
			entryIcon = defaultIcon
		}

		// Pad to square and resize to required sizes
		icon64, icon256 := prepareIcons(entryIcon)

		// Generate icon filenames based on entry name
		// Default entry: icon_64.png, icon_256.png
		// Named entry: icon_<name>_64.png, icon_<name>_256.png
		var icon64Name, icon256Name string
		if entry.Name == "" {
			icon64Name = "icon_64.png"
			icon256Name = "icon_256.png"
		} else {
			icon64Name = fmt.Sprintf("icon_%s_64.png", entry.Name)
			icon256Name = fmt.Sprintf("icon_%s_256.png", entry.Name)
		}

		// Save to ui/images directory
		uiImagesDir := filepath.Join(appDir, "app", "ui", "images")
		if err := saveImage(icon64, filepath.Join(uiImagesDir, icon64Name)); err != nil {
			return fmt.Errorf("failed to save icon %s: %w", icon64Name, err)
		}
		if err := saveImage(icon256, filepath.Join(uiImagesDir, icon256Name)); err != nil {
			return fmt.Errorf("failed to save icon %s: %w", icon256Name, err)
		}

		// For default entry, also save to root directory as ICON.PNG and ICON_256.PNG
		if entry.Name == "" {
			if err := saveImage(icon64, filepath.Join(appDir, "ICON.PNG")); err != nil {
				return fmt.Errorf("failed to save ICON.PNG: %w", err)
			}
			if err := saveImage(icon256, filepath.Join(appDir, "ICON_256.PNG")); err != nil {
				return fmt.Errorf("failed to save ICON_256.PNG: %w", err)
			}
		}
	}

	// If no entries had a default entry, use first entry's icon for root icons
	if len(config.Entries) > 0 {
		hasDefaultEntry := false
		for _, entry := range config.Entries {
			if entry.Name == "" {
				hasDefaultEntry = true
				break
			}
		}

		if !hasDefaultEntry {
			// Use first entry's icon for root icons
			firstEntry := config.Entries[0]
			entryIcon, _ := loadIcon(firstEntry.Icon, basePath)
			if entryIcon == nil {
				if defaultIcon == nil {
					defaultIcon, _ = loadDefaultIcon()
				}
				entryIcon = defaultIcon
			}
			if entryIcon != nil {
				icon64, icon256 := prepareIcons(entryIcon)
				saveImage(icon64, filepath.Join(appDir, "ICON.PNG"))
				saveImage(icon256, filepath.Join(appDir, "ICON_256.PNG"))
			}
		}
	}

	return nil
}

// loadIcon loads an icon from the given source string.
// Supports URL sources (file://, http://, https://) and base64 encoded data.
func loadIcon(source string, basePath string) (image.Image, error) {
	iconSource, err := ParseIconSource(source, basePath)
	if err != nil {
		return nil, err
	}
	if iconSource == nil {
		return nil, fmt.Errorf("empty icon source")
	}
	return iconSource.Load()
}

// getBasePath extracts the compose working directory from container labels
// Returns empty string if the label is not present
func getBasePath(labels map[string]string) string {
	// Docker Compose automatically adds this label
	if dir, ok := labels["com.docker.compose.project.working_dir"]; ok {
		return dir
	}
	return "" // Empty string indicates relative paths cannot be resolved
}

// loadDefaultIcon loads the embedded default icon
func loadDefaultIcon() (image.Image, error) {
	data, err := defaultIcons.ReadFile("defaults/ICON_256.PNG")
	if err != nil {
		return nil, err
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	return img, nil
}

// prepareIcons pads a non-square image to square and resizes to 64x64 and 256x256
func prepareIcons(src image.Image) (icon64, icon256 image.Image) {
	squared := squareImage(src)
	icon64 = resizeImage(squared, 64, 64)
	icon256 = resizeImage(squared, 256, 256)
	return
}

// resizeImage resizes an image to the specified dimensions
func resizeImage(src image.Image, width, height int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

// squareImage pads a non-square image to make it square, centering the original image
// The background is transparent
func squareImage(src image.Image) image.Image {
	bounds := src.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()

	// If already square, return as-is
	if srcWidth == srcHeight {
		return src
	}

	// Determine the size of the square (use the larger dimension)
	size := max(srcWidth, srcHeight)

	// Create a new transparent square image
	dst := image.NewRGBA(image.Rect(0, 0, size, size))

	// Calculate offset to center the original image
	offsetX := (size - srcWidth) / 2
	offsetY := (size - srcHeight) / 2

	// Draw the source image centered on the square canvas
	dstRect := image.Rect(offsetX, offsetY, offsetX+srcWidth, offsetY+srcHeight)
	xdraw.Copy(dst, dstRect.Min, src, bounds, xdraw.Over, nil)

	return dst
}

// saveImage saves an image to a file
func saveImage(img image.Image, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return png.Encode(f, img)
}
