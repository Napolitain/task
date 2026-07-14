package artifactcache

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/go-task/task/v3/errors"
)

// ErrNoFiles indicates that no generated files were available to cache.
var ErrNoFiles = errors.New("artifact cache has no generated files")

type archiveFile struct {
	absolute string
	relative string
	info     fs.FileInfo
}

// Store writes generated files to an atomic tar.zst archive.
func Store(ctx context.Context, archivePath, root string, files []string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	archiveFiles, err := collectArchiveFiles(root, files)
	if err != nil {
		return err
	}
	if len(archiveFiles) == 0 {
		return ErrNoFiles
	}
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(archivePath), ".artifact-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := writeArchive(ctx, temp, archiveFiles); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, archivePath); err != nil {
		// os.Rename does not replace files on Windows.
		if removeErr := os.Remove(archivePath); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return err
		}
		return os.Rename(tempPath, archivePath)
	}
	return nil
}

// Restore extracts an archive and replaces the currently generated files.
func Restore(ctx context.Context, archivePath, root string, cleanupPaths []string) (bool, error) {
	input, err := os.Open(archivePath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	restoreErr := restoreArchive(ctx, input, root, cleanupPaths)
	closeErr := input.Close()
	if restoreErr != nil {
		return false, restoreErr
	}
	return true, closeErr
}

func writeArchive(ctx context.Context, output io.Writer, files []archiveFile) error {
	encoder, err := zstd.NewWriter(output,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(encoder)
	buffer := make([]byte, 128*1024)

	closeWithError := func(writeErr error) error {
		tarErr := tw.Close()
		zstdErr := encoder.Close()
		if writeErr != nil {
			return writeErr
		}
		if tarErr != nil {
			return tarErr
		}
		return zstdErr
	}

	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return closeWithError(err)
		}
		linkTarget := ""
		if file.info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(file.absolute)
			if err != nil {
				return closeWithError(err)
			}
		}
		header, err := tar.FileInfoHeader(file.info, linkTarget)
		if err != nil {
			return closeWithError(err)
		}
		header.Name = filepath.ToSlash(file.relative)
		header.Format = tar.FormatPAX
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		if err := tw.WriteHeader(header); err != nil {
			return closeWithError(err)
		}
		if !file.info.Mode().IsRegular() {
			continue
		}
		input, err := os.Open(file.absolute)
		if err != nil {
			return closeWithError(err)
		}
		_, copyErr := io.CopyBuffer(tw, &contextReader{ctx: ctx, reader: input}, buffer)
		closeErr := input.Close()
		if copyErr != nil {
			return closeWithError(copyErr)
		}
		if closeErr != nil {
			return closeWithError(closeErr)
		}
	}
	return closeWithError(nil)
}

func restoreArchive(ctx context.Context, input io.Reader, root string, cleanupPaths []string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	decoder, err := zstd.NewReader(input)
	if err != nil {
		return err
	}
	defer decoder.Close()
	tr := tar.NewReader(decoder)
	staging, err := os.MkdirTemp(root, ".task-cache-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	var entries []string
	seen := make(map[string]struct{})
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		relative, err := cleanArchivePath(header.Name)
		if err != nil {
			return err
		}
		if _, ok := seen[relative]; ok {
			return fmt.Errorf("duplicate artifact path %q", header.Name)
		}
		seen[relative] = struct{}{}
		if err := extractEntry(ctx, tr, header, root, staging, relative, buffer); err != nil {
			return err
		}
		entries = append(entries, relative)
	}
	if len(entries) == 0 {
		return ErrNoFiles
	}

	if err := removeFiles(root, cleanupPaths); err != nil {
		return err
	}
	sort.Strings(entries)
	for _, relative := range entries {
		if err := rejectSymlinkParents(root, relative); err != nil {
			return err
		}
		source := filepath.Join(staging, relative)
		target := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if info, err := os.Lstat(target); err == nil {
			if info.IsDir() {
				return fmt.Errorf("refusing to replace directory %q", target)
			}
			if err := os.Remove(target); err != nil {
				return err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Rename(source, target); err != nil {
			return err
		}
	}
	return nil
}

func collectArchiveFiles(root string, files []string) ([]archiveFile, error) {
	result := make([]archiveFile, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, name := range files {
		absolute, relative, err := withinRoot(root, name)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[relative]; ok {
			continue
		}
		seen[relative] = struct{}{}
		if err := rejectSymlinkParents(root, relative); err != nil {
			return nil, err
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("artifact output %q is not a regular file or symlink", absolute)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(absolute)
			if err != nil {
				return nil, err
			}
			if err := validateSymlinkTarget(root, absolute, target); err != nil {
				return nil, err
			}
		}
		result = append(result, archiveFile{absolute: absolute, relative: relative, info: info})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].relative < result[j].relative })
	return result, nil
}

func extractEntry(ctx context.Context, tr io.Reader, header *tar.Header, root, staging, relative string, buffer []byte) error {
	if err := rejectSymlinkParents(staging, relative); err != nil {
		return err
	}
	target := filepath.Join(staging, relative)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	switch header.Typeflag {
	case tar.TypeReg:
		if header.Size < 0 {
			return fmt.Errorf("invalid artifact size for %q", header.Name)
		}
		mode := header.FileInfo().Mode().Perm()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		written, copyErr := io.CopyBuffer(output, &contextReader{ctx: ctx, reader: io.LimitReader(tr, header.Size)}, buffer)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != header.Size {
			return fmt.Errorf("short artifact entry %q", header.Name)
		}
		if err := os.Chmod(target, mode); err != nil {
			return err
		}
		return os.Chtimes(target, header.ModTime, header.ModTime)
	case tar.TypeSymlink:
		finalPath := filepath.Join(root, relative)
		if err := validateSymlinkTarget(root, finalPath, header.Linkname); err != nil {
			return err
		}
		return os.Symlink(header.Linkname, target)
	default:
		return fmt.Errorf("unsupported artifact entry type for %q", header.Name)
	}
}

func removeFiles(root string, files []string) error {
	for _, name := range files {
		absolute, relative, err := withinRoot(root, name)
		if err != nil {
			return err
		}
		if err := rejectSymlinkParents(root, relative); err != nil {
			return err
		}
		info, err := os.Lstat(absolute)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("refusing to remove generated directory %q", absolute)
		}
		if err := os.Remove(absolute); err != nil {
			return err
		}
	}
	return nil
}

func cleanArchivePath(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) || path.Clean(name) != name || name == "." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("invalid artifact path %q", name)
	}
	relative := filepath.FromSlash(name)
	if filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", fmt.Errorf("invalid artifact path %q", name)
	}
	return relative, nil
}

func withinRoot(root, name string) (absolute, relative string, err error) {
	if !filepath.IsAbs(name) {
		name = filepath.Join(root, name)
	}
	absolute, err = filepath.Abs(name)
	if err != nil {
		return "", "", err
	}
	absolute = filepath.Clean(absolute)
	relative, err = filepath.Rel(root, absolute)
	if err != nil {
		return "", "", err
	}
	if relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("artifact path %q is outside project root %q", name, root)
	}
	return absolute, filepath.Clean(relative), nil
}

func rejectSymlinkParents(root, relative string) error {
	current := root
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact path parent %q is a symlink", current)
		}
	}
	return nil
}

func validateSymlinkTarget(root, linkPath, target string) error {
	if filepath.IsAbs(target) {
		return fmt.Errorf("artifact symlink %q has absolute target %q", linkPath, target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
	_, relative, err := withinRoot(root, resolved)
	if err != nil {
		return fmt.Errorf("artifact symlink %q escapes project root: %w", linkPath, err)
	}
	if err := rejectSymlinkParents(root, relative); err != nil {
		return err
	}
	if evaluated, err := filepath.EvalSymlinks(resolved); err == nil {
		if _, _, err := withinRoot(root, evaluated); err != nil {
			return fmt.Errorf("artifact symlink %q resolves outside project root: %w", linkPath, err)
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
