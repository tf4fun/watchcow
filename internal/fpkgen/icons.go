package fpkgen

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

//go:embed defaults/ICON.PNG defaults/ICON_256.PNG
var defaultIcons embed.FS

// handleIcons downloads/generates and saves all required icon files for all entries
func (g *Generator) handleIcons(appDir string, config *AppConfig) error {
	var defaultIcon image.Image

	// Process each entry's icon
	for _, entry := range config.Entries {
		entryIcon, err := loadIconFromSource(entry.Icon)
		if err != nil {
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

		// Resize to required sizes
		icon64 := resizeImage(entryIcon, 64, 64)
		icon256 := resizeImage(entryIcon, 256, 256)

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
			entryIcon, _ := loadIconFromSource(firstEntry.Icon)
			if entryIcon == nil {
				if defaultIcon == nil {
					defaultIcon, _ = loadDefaultIcon()
				}
				entryIcon = defaultIcon
			}
			if entryIcon != nil {
				icon64 := resizeImage(entryIcon, 64, 64)
				icon256 := resizeImage(entryIcon, 256, 256)
				saveImage(icon64, filepath.Join(appDir, "ICON.PNG"))
				saveImage(icon256, filepath.Join(appDir, "ICON_256.PNG"))
			}
		}
	}

	return nil
}

// loadIconFromSource loads an icon from URL or local file path
func loadIconFromSource(iconSource string) (image.Image, error) {
	if iconSource == "" {
		return nil, fmt.Errorf("empty icon source")
	}

	if strings.HasPrefix(iconSource, "file://") {
		// Load from local file path
		localPath := strings.TrimPrefix(iconSource, "file://")
		return loadLocalIcon(localPath)
	} else if strings.HasPrefix(iconSource, "http") {
		// Download from URL
		return downloadIcon(iconSource)
	}

	return nil, fmt.Errorf("unsupported icon source: %s", iconSource)
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

// loadLocalIcon loads an icon from local file path
func loadLocalIcon(path string) (image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	return img, nil
}

// downloadIcon downloads an icon from URL
func downloadIcon(url string) (image.Image, error) {
	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download icon: status %d", resp.StatusCode)
	}

	// Read the body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Decode the image
	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	return img, nil
}

// resizeImage resizes an image to the specified dimensions
func resizeImage(src image.Image, width, height int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
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
