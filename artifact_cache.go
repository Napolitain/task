package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-task/task/v3/internal/artifactcache"
	"github.com/go-task/task/v3/internal/fingerprint"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/taskfile/ast"
)

type taskArtifactCache struct {
	executor    *Executor
	task        *ast.Task
	checksum    string
	archivePath string
}

func (e *Executor) validateArtifactCacheTask(t *ast.Task) error {
	if !t.Cache {
		return nil
	}
	method := t.Method
	if method == "" {
		method = e.Taskfile.Method
	}
	if method != "checksum" {
		return fmt.Errorf("task: cache for task %q requires method checksum", t.Name())
	}
	if len(t.Sources) == 0 || len(t.Generates) == 0 {
		return fmt.Errorf("task: cache for task %q requires both sources and generates", t.Name())
	}
	return nil
}

func (e *Executor) newTaskArtifactCache(t *ast.Task, calculatedChecksum string) (*taskArtifactCache, error) {
	checksumPath := fingerprint.ChecksumFilePath(e.TempDir.Fingerprint, t)
	data, err := os.ReadFile(checksumPath)
	if err != nil {
		return nil, err
	}
	checksum := strings.TrimSpace(string(data))
	if checksum == "" || checksum != calculatedChecksum || len(checksum) > 32 || strings.IndexFunc(checksum, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) >= 0 {
		return nil, fmt.Errorf("invalid persisted checksum for task %q", t.Name())
	}
	taskDir := filepath.Base(checksumPath)
	return &taskArtifactCache{
		executor:    e,
		task:        t,
		checksum:    checksum,
		archivePath: filepath.Join(e.TempDir.Fingerprint, "cache", taskDir, checksum+".tar.zst"),
	}, nil
}

func (e *Executor) restoreTaskFromArtifactCache(
	ctx context.Context,
	t *ast.Task,
	checker *fingerprint.ChecksumChecker,
	options ...fingerprint.CheckerOption,
) (*taskArtifactCache, bool, error) {
	if checker.LastChecksum() == "" {
		return nil, false, nil
	}

	cache, err := e.newTaskArtifactCache(t, checker.LastChecksum())
	if err != nil {
		e.warnArtifactCache(t, "prepare", err)
		return nil, false, nil
	}
	hit, err := cache.restore(ctx)
	if err != nil {
		cache.warn("restore", err)
		return cache, false, nil
	}
	if !hit {
		return cache, false, nil
	}

	upToDate, err := fingerprint.IsTaskUpToDate(ctx, t, options...)
	if err != nil {
		return nil, false, err
	}
	if upToDate {
		return cache, true, nil
	}
	if checker.LastChecksum() == cache.checksum {
		return cache, false, nil
	}

	cache, err = e.newTaskArtifactCache(t, checker.LastChecksum())
	if err != nil {
		e.warnArtifactCache(t, "prepare", err)
		return nil, false, nil
	}
	return cache, false, nil
}

func (c *taskArtifactCache) restore(ctx context.Context) (bool, error) {
	generated, err := c.generatedFiles()
	if err != nil {
		return false, err
	}
	hit, err := artifactcache.Restore(ctx, c.archivePath, c.executor.Dir, generated)
	if err != nil {
		_ = os.Remove(c.archivePath)
	}
	return hit, err
}

func (c *taskArtifactCache) store(ctx context.Context) error {
	generated, err := c.generatedFiles()
	if err != nil {
		return err
	}
	return artifactcache.Store(ctx, c.archivePath, c.executor.Dir, generated)
}

func (c *taskArtifactCache) generatedFiles() ([]string, error) {
	return fingerprint.Globs(c.task.Dir, c.task.Generates, c.task.ShouldUseGitignore())
}

func (c *taskArtifactCache) warn(action string, err error) {
	c.executor.warnArtifactCache(c.task, action, err)
}

func (e *Executor) warnArtifactCache(t *ast.Task, action string, err error) {
	e.Logger.VerboseErrf(logger.Yellow, "task: could not %s cache for %q: %v\n", action, t.Name(), err)
}
