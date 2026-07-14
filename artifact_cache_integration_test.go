package task_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-task/task/v3"
)

func TestArtifactCacheRestoresPreviousBuilds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeArtifactCacheTaskfile(t, dir)
	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "A")

	runArtifactCacheTask(t, dir, "build")
	assertFileContent(t, filepath.Join(dir, "output.txt"), "A")
	assertLineCount(t, filepath.Join(dir, "runs.log"), 1)

	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "B")
	runArtifactCacheTask(t, dir, "build")
	assertFileContent(t, filepath.Join(dir, "output.txt"), "B")
	assertLineCount(t, filepath.Join(dir, "runs.log"), 2)

	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "A")
	runArtifactCacheTask(t, dir, "build")
	assertFileContent(t, filepath.Join(dir, "output.txt"), "A")
	assertLineCount(t, filepath.Join(dir, "runs.log"), 2)
	assertArchiveCount(t, dir, "build", 2)

	runArtifactCacheTask(t, dir, "second")
	assertFileContent(t, filepath.Join(dir, "second.txt"), "A")
	assertLineCount(t, filepath.Join(dir, "second-runs.log"), 1)
	assertArchiveCount(t, dir, "second", 1)
}

func TestArtifactCacheKeepsIdenticalBuildUpToDate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeArtifactCacheTaskfile(t, dir)
	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "A")
	runArtifactCacheTask(t, dir, "build")

	archivePath := artifactArchivePath(t, dir, "build")
	archiveTime := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(archivePath, archiveTime, archiveTime))

	runArtifactCacheTask(t, dir, "build")
	assertFileContent(t, filepath.Join(dir, "output.txt"), "A")
	assertLineCount(t, filepath.Join(dir, "runs.log"), 1)
	assertArchiveCount(t, dir, "build", 1)
	archiveInfo, err := os.Stat(archivePath)
	require.NoError(t, err)
	assert.True(t, archiveInfo.ModTime().Equal(archiveTime))
}

func TestArtifactCacheStoresDistinctBuilds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeArtifactCacheTaskfile(t, dir)

	for i, source := range []string{"A", "B", "C"} {
		writeArtifactFile(t, filepath.Join(dir, "source.txt"), source)
		runArtifactCacheTask(t, dir, "build")
		assertFileContent(t, filepath.Join(dir, "output.txt"), source)
		assertLineCount(t, filepath.Join(dir, "runs.log"), i+1)
		assertArchiveCount(t, dir, "build", i+1)
	}
}

func TestArtifactCacheRestoresMissingOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeArtifactCacheTaskfile(t, dir)
	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "A")
	runArtifactCacheTask(t, dir, "build")
	require.NoError(t, os.Remove(filepath.Join(dir, "output.txt")))

	runArtifactCacheTask(t, dir, "build")
	assertFileContent(t, filepath.Join(dir, "output.txt"), "A")
	assertLineCount(t, filepath.Join(dir, "runs.log"), 1)
	assertArchiveCount(t, dir, "build", 1)
}

func TestArtifactCacheRemovesStaleGlobOutputs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeArtifactCacheTaskfile(t, dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "outputs"), 0o755))

	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "A")
	runArtifactCacheTask(t, dir, "glob-build")
	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "B")
	runArtifactCacheTask(t, dir, "glob-build")
	stalePath := filepath.Join(dir, "outputs", "stale.txt")
	writeArtifactFile(t, stalePath, "stale")

	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "A")
	runArtifactCacheTask(t, dir, "glob-build")
	assertFileContent(t, filepath.Join(dir, "outputs", "output.txt"), "A")
	_, err := os.Stat(stalePath)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assertLineCount(t, filepath.Join(dir, "glob-runs.log"), 2)
	assertArchiveCount(t, dir, "glob-build", 2)
}

func TestArtifactCacheSharedRunOnceDependency(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeArtifactCacheTaskfile(t, dir)

	for i, step := range []struct {
		source   string
		wantRuns int
	}{
		{source: "A", wantRuns: 1},
		{source: "B", wantRuns: 2},
		{source: "A", wantRuns: 2},
	} {
		writeArtifactFile(t, filepath.Join(dir, "source.txt"), step.source)
		runArtifactCacheTask(t, dir, "diamond")
		assertFileContent(t, filepath.Join(dir, "shared.txt"), step.source)
		assertLineCount(t, filepath.Join(dir, "shared-runs.log"), step.wantRuns)
		assertArchiveCount(t, dir, "shared", min(i+1, 2))
	}
}

func TestArtifactCacheBypassesAndFallbacks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeArtifactCacheTaskfile(t, dir)
	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "A")
	runArtifactCacheTask(t, dir, "build")

	checksumPath := filepath.Join(dir, ".task", "checksum", "build")
	checksum := strings.TrimSpace(readFile(t, checksumPath))
	archivePath := filepath.Join(dir, ".task", "cache", "build", checksum+".tar.zst")

	require.NoError(t, os.Remove(filepath.Join(dir, "output.txt")))
	err := statusArtifactCacheTask(t, dir, "build")
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "output.txt"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	writeArtifactFile(t, archivePath, "not a zstd archive")
	runArtifactCacheTask(t, dir, "build")
	assertFileContent(t, filepath.Join(dir, "output.txt"), "A")
	assertLineCount(t, filepath.Join(dir, "runs.log"), 2)
	refreshedArchive := readBytes(t, archivePath)
	assert.NotEqual(t, []byte("not a zstd archive"), refreshedArchive)

	archiveTime := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(archivePath, archiveTime, archiveTime))
	runArtifactCacheTask(t, dir, "build", task.WithForce(true))
	assertLineCount(t, filepath.Join(dir, "runs.log"), 3)
	assert.Equal(t, refreshedArchive, readBytes(t, archivePath))
	archiveInfo, err := os.Stat(archivePath)
	require.NoError(t, err)
	assert.True(t, archiveInfo.ModTime().Equal(archiveTime))

	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "C")
	runArtifactCacheTask(t, dir, "build", task.WithDry(true))
	assertLineCount(t, filepath.Join(dir, "runs.log"), 3)
	assert.Equal(t, checksum, strings.TrimSpace(readFile(t, checksumPath)))
	assertArchiveCount(t, dir, "build", 1)
}

func TestArtifactCacheValidationAndFailedBuild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeArtifactCacheTaskfile(t, dir)
	writeArtifactFile(t, filepath.Join(dir, "source.txt"), "A")

	for _, test := range []struct {
		task string
		want string
	}{
		{task: "timestamp", want: "requires method checksum"},
		{task: "missing-sources", want: "requires both sources and generates"},
		{task: "missing-generates", want: "requires both sources and generates"},
	} {
		t.Run(test.task, func(t *testing.T) {
			t.Parallel()

			err := runArtifactCacheTaskError(t, dir, test.task)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}

	err := runArtifactCacheTaskError(t, dir, "failure")
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(dir, ".task", "checksum", "failure"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	assertArchiveCount(t, dir, "failure", 0)
}

func writeArtifactCacheTaskfile(t *testing.T, dir string) {
	t.Helper()

	writeArtifactFile(t, filepath.Join(dir, "Taskfile.yml"), `version: '3'
silent: true

tasks:
  build:
    cache: true
    sources: [source.txt]
    generates: [output.txt]
    cmds:
      - |
        read VALUE < source.txt || true
        printf '%s' "$VALUE" > output.txt
        printf 'build\n' >> runs.log

  second:
    cache: true
    sources: [source.txt]
    generates: [second.txt]
    cmds:
      - |
        read VALUE < source.txt || true
        printf '%s' "$VALUE" > second.txt
        printf 'second\n' >> second-runs.log

  glob-build:
    cache: true
    sources: [source.txt]
    generates: [outputs/*.txt]
    cmds:
      - |
        read VALUE < source.txt || true
        printf '%s' "$VALUE" > outputs/output.txt
        printf 'glob-build\n' >> glob-runs.log

  diamond:
    deps: [parent-a, parent-b]

  parent-a:
    deps: [shared]

  parent-b:
    deps: [shared]

  shared:
    run: once
    cache: true
    sources: [source.txt]
    generates: [shared.txt]
    cmds:
      - |
        read VALUE < source.txt || true
        printf '%s' "$VALUE" > shared.txt
        printf 'shared\n' >> shared-runs.log

  timestamp:
    cache: true
    method: timestamp
    sources: [source.txt]
    generates: [timestamp.txt]
    cmd: printf timestamp > timestamp.txt

  missing-sources:
    cache: true
    generates: [missing-sources.txt]
    cmd: printf output > missing-sources.txt

  missing-generates:
    cache: true
    sources: [source.txt]
    cmd: printf output > missing-generates.txt

  failure:
    cache: true
    sources: [source.txt]
    generates: [failure.txt]
    cmd: false
`)
}

func runArtifactCacheTask(t *testing.T, dir, taskName string, opts ...task.ExecutorOption) {
	t.Helper()
	require.NoError(t, runArtifactCacheTaskError(t, dir, taskName, opts...))
}

func runArtifactCacheTaskError(t *testing.T, dir, taskName string, opts ...task.ExecutorOption) error {
	t.Helper()

	e := newArtifactCacheExecutor(t, dir, opts...)
	return e.Run(t.Context(), &task.Call{Task: taskName})
}

func statusArtifactCacheTask(t *testing.T, dir, taskName string) error {
	t.Helper()

	e := newArtifactCacheExecutor(t, dir)
	return e.Status(t.Context(), &task.Call{Task: taskName})
}

func newArtifactCacheExecutor(t *testing.T, dir string, opts ...task.ExecutorOption) *task.Executor {
	t.Helper()

	var output SyncBuffer
	tempDir := task.TempDir{
		Remote:      filepath.Join(dir, ".task"),
		Fingerprint: filepath.Join(dir, ".task"),
	}
	baseOpts := []task.ExecutorOption{
		task.WithDir(dir),
		task.WithTempDir(tempDir),
		task.WithStdout(&output),
		task.WithStderr(&output),
	}
	e := task.NewExecutor(append(baseOpts, opts...)...)
	require.NoError(t, e.Setup())
	return e
}

func artifactArchivePath(t *testing.T, dir, taskName string) string {
	t.Helper()

	checksum := strings.TrimSpace(readFile(t, filepath.Join(dir, ".task", "checksum", taskName)))
	return filepath.Join(dir, ".task", "cache", taskName, checksum+".tar.zst")
}

func assertArchiveCount(t *testing.T, dir, taskName string, want int) {
	t.Helper()

	archives, err := filepath.Glob(filepath.Join(dir, ".task", "cache", taskName, "*.tar.zst"))
	require.NoError(t, err)
	assert.Len(t, archives, want)
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	assert.Equal(t, want, readFile(t, path))
}

func assertLineCount(t *testing.T, path string, want int) {
	t.Helper()
	assert.Equal(t, want, strings.Count(readFile(t, path), "\n"))
}

func writeArtifactFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	return string(readBytes(t, path))
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}
