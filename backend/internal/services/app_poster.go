package services

import (
	"encoding/xml"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path"
	"path/filepath"
	"strings"

	_ "github.com/deepteams/webp"
)

// validateAppPoster confina o poster a uma imagem existente de ui/, sem symlinks.
func validateAppPoster(b PluginSurfaceBundle, poster string) error {
	if b.Directory == "" || !safeBundledEntry(poster) {
		return ErrInvalidInput
	}
	dir := filepath.Join(b.Directory, "ui")
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidInput
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ErrInvalidInput
	}
	defer root.Close()
	relative := strings.TrimPrefix(poster, "ui/")
	parts := strings.Split(relative, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ErrInvalidInput
		}
	}
	file, err := root.Open(relative)
	if err != nil {
		return ErrInvalidInput
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ErrInvalidInput
	}
	extension := strings.ToLower(path.Ext(relative))
	if extension == ".svg" {
		var document struct{ XMLName xml.Name }
		if err := xml.NewDecoder(file).Decode(&document); err != nil || document.XMLName.Local != "svg" {
			return ErrInvalidInput
		}
		return nil
	}
	switch extension {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		return ErrInvalidInput
	}
	config, _, err := image.DecodeConfig(file)
	if err != nil || config.Width < 1 || config.Height < 1 {
		return ErrInvalidInput
	}
	return nil
}
