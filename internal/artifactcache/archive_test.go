package artifactcache

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreAndRestore(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cacheDir := t.TempDir()
	outputPath := filepath.Join(root, "nested", "output.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(outputPath), 0o755))
	require.NoError(t, os.WriteFile(outputPath, []byte("cached output"), 0o750))
	modTime := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(outputPath, modTime, modTime))

	files := []string{outputPath}
	linkPath := filepath.Join(root, "output-link")
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Symlink(filepath.Join("nested", "output.txt"), linkPath))
		files = append(files, linkPath)
	}

	archivePath := filepath.Join(cacheDir, "nested", "checksum.tar.zst")
	require.NoError(t, Store(context.Background(), archivePath, root, files))

	require.NoError(t, os.WriteFile(outputPath, []byte("stale output"), 0o600))
	stalePath := filepath.Join(root, "stale.txt")
	require.NoError(t, os.WriteFile(stalePath, []byte("remove me"), 0o600))

	hit, err := Restore(context.Background(), archivePath, root, append(files, stalePath))
	require.NoError(t, err)
	assert.True(t, hit)
	content, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Equal(t, "cached output", string(content))
	info, err := os.Stat(outputPath)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(modTime))
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o750), info.Mode().Perm())
		target, err := os.Readlink(linkPath)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("nested", "output.txt"), target)
	}
	_, err = os.Stat(stalePath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRestoreMissingArchive(t *testing.T) {
	t.Parallel()

	hit, err := Restore(context.Background(), filepath.Join(t.TempDir(), "missing.tar.zst"), t.TempDir(), nil)
	require.NoError(t, err)
	assert.False(t, hit)
}

func TestRestoreCorruptArchiveLeavesOutputs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outputPath := filepath.Join(root, "output.txt")
	require.NoError(t, os.WriteFile(outputPath, []byte("current output"), 0o600))
	archivePath := filepath.Join(t.TempDir(), "corrupt.tar.zst")
	require.NoError(t, os.WriteFile(archivePath, []byte("not a zstd archive"), 0o600))

	hit, err := Restore(context.Background(), archivePath, root, []string{outputPath})
	require.Error(t, err)
	assert.False(t, hit)
	content, readErr := os.ReadFile(outputPath)
	require.NoError(t, readErr)
	assert.Equal(t, "current output", string(content))
}

func TestStoreRejectsFileOutsideRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))

	err := Store(context.Background(), filepath.Join(t.TempDir(), "cache.tar.zst"), root, []string{outside})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside project root")
}

func TestRestoreRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "malicious.tar.zst")
	writeTestArchive(t, archivePath, "../escaped.txt", "escaped")

	hit, err := Restore(context.Background(), archivePath, root, nil)
	require.Error(t, err)
	assert.False(t, hit)
	_, statErr := os.Stat(filepath.Join(filepath.Dir(root), "escaped.txt"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func writeTestArchive(t *testing.T, archivePath, name, content string) {
	t.Helper()

	var archive bytes.Buffer
	encoder, err := zstd.NewWriter(&archive)
	require.NoError(t, err)
	tw := tar.NewWriter(encoder)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o600,
		Size: int64(len(content)),
	}))
	_, err = tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, encoder.Close())
	require.NoError(t, os.WriteFile(archivePath, archive.Bytes(), 0o600))
}
