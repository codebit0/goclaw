package skills

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Sentinel errors for archive extraction.
var (
	ErrUnsafePath      = errors.New("archive: unsafe path rejected")
	ErrZipBomb         = errors.New("archive: uncompressed size exceeds limit (zip bomb protection)")
	ErrUnknownArchive  = errors.New("archive: unknown format")
	ErrFileTooLarge    = errors.New("archive: single file exceeds limit")
)

// ArchiveFile is a single extracted entry held in memory.
type ArchiveFile struct {
	Name    string
	Mode    fs.FileMode
	Size    int64
	Content []byte
}

// Magic byte sequences for format detection.
var (
	magicGzip = []byte{0x1f, 0x8b}
	magicZip  = []byte{0x50, 0x4b, 0x03, 0x04}
	magicELF  = []byte{0x7f, 0x45, 0x4c, 0x46}
)

// ExtractArchive detects the format by magic bytes + extension fallback and extracts.
// maxUncompressed caps total uncompressed bytes (zip-bomb protection).
func ExtractArchive(path string, maxUncompressed int64) ([]ArchiveFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var head [4]byte
	n, _ := io.ReadFull(f, head[:])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	prefix := head[:n]

	switch {
	case bytes.HasPrefix(prefix, magicGzip):
		return extractTarGz(f, maxUncompressed)
	case bytes.HasPrefix(prefix, magicZip):
		return extractZip(path, maxUncompressed)
	case bytes.HasPrefix(prefix, magicELF):
		return extractRaw(f, filepath.Base(path))
	}
	// Fallback on extension.
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(f, maxUncompressed)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(path, maxUncompressed)
	}
	// Last resort: treat as raw binary.
	return extractRaw(f, filepath.Base(path))
}

// sanitizePath rejects absolute, parent-escaping, and Windows-drive paths.
// Returns a cleaned relative path safe to join with an install dir.
func sanitizePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty", ErrUnsafePath)
	}
	// Reject null bytes explicitly.
	if strings.ContainsRune(name, 0x00) {
		return "", fmt.Errorf("%w: null byte", ErrUnsafePath)
	}
	// Reject Windows drive prefix like "C:\".
	if len(name) >= 2 && name[1] == ':' {
		return "", fmt.Errorf("%w: windows drive %q", ErrUnsafePath, name)
	}
	// Normalize both separators.
	normalized := strings.ReplaceAll(name, "\\", "/")
	// Reject absolute.
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafePath, name)
	}
	// Reject any "../" component.
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: traversal component in %q", ErrUnsafePath, name)
		}
	}
	cleaned := path.Clean(normalized)
	// path.Clean of a non-absolute path should remain non-absolute.
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: escapes base after clean %q → %q", ErrUnsafePath, name, cleaned)
	}
	return cleaned, nil
}

// extractTarGz streams a gzip'd tar and returns all regular-file entries.
func extractTarGz(r io.Reader, maxUncompressed int64) ([]ArchiveFile, error) {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	var out []ArchiveFile
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			// supported
		case tar.TypeSymlink, tar.TypeLink:
			slog.Warn("archive: skipping link entry", "name", hdr.Name, "type", string(hdr.Typeflag))
			continue
		default:
			// Skip directories and other special types.
			continue
		}

		clean, err := sanitizePath(hdr.Name)
		if err != nil {
			return nil, err
		}

		if hdr.Size < 0 {
			return nil, fmt.Errorf("tar: negative size for %q", hdr.Name)
		}
		if total+hdr.Size > maxUncompressed {
			return nil, ErrZipBomb
		}
		total += hdr.Size

		buf := make([]byte, 0, hdr.Size)
		w := bytes.NewBuffer(buf)
		// Use limited reader so a truncated tar doesn't spin forever.
		if _, err := io.Copy(w, io.LimitReader(tr, hdr.Size)); err != nil {
			return nil, fmt.Errorf("tar read %q: %w", hdr.Name, err)
		}
		out = append(out, ArchiveFile{
			Name:    clean,
			Mode:    fs.FileMode(hdr.Mode) & fs.ModePerm,
			Size:    hdr.Size,
			Content: w.Bytes(),
		})
	}
	return out, nil
}

// extractZip opens a zip file and returns all regular file entries.
func extractZip(filePath string, maxUncompressed int64) ([]ArchiveFile, error) {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return nil, fmt.Errorf("zip open: %w", err)
	}
	defer zr.Close()

	// Pre-check declared uncompressed sizes.
	var sum uint64
	for _, f := range zr.File {
		if f.Mode().IsDir() {
			continue
		}
		sum += f.UncompressedSize64
		if sum > uint64(maxUncompressed) {
			return nil, ErrZipBomb
		}
	}

	var out []ArchiveFile
	var total int64
	for _, f := range zr.File {
		if f.Mode().IsDir() {
			continue
		}
		if f.Mode()&fs.ModeSymlink != 0 {
			slog.Warn("archive: skipping symlink entry", "name", f.Name)
			continue
		}
		clean, err := sanitizePath(f.Name)
		if err != nil {
			return nil, err
		}

		if f.UncompressedSize64 > uint64(maxUncompressed) {
			return nil, ErrFileTooLarge
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("zip open %q: %w", f.Name, err)
		}

		// Also enforce streaming cap in case declared size lies.
		lr := io.LimitReader(rc, maxUncompressed-total+1)
		buf, err := io.ReadAll(lr)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("zip read %q: %w", f.Name, err)
		}
		if int64(len(buf)) > maxUncompressed-total {
			return nil, ErrZipBomb
		}
		total += int64(len(buf))

		out = append(out, ArchiveFile{
			Name:    clean,
			Mode:    f.Mode().Perm(),
			Size:    int64(len(buf)),
			Content: buf,
		})
	}
	return out, nil
}

// extractRaw reads the entire file as a single binary entry.
func extractRaw(f *os.File, name string) ([]ArchiveFile, error) {
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	clean, err := sanitizePath(name)
	if err != nil {
		return nil, err
	}
	_ = ErrUnknownArchive // retained for external callers
	return []ArchiveFile{{
		Name:    clean,
		Mode:    0o755,
		Size:    int64(len(b)),
		Content: b,
	}}, nil
}
