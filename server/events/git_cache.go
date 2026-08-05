package events

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/runatlantis/atlantis/server/logging"
)

const (
	// gcStaleLockTimeout is how old a gc.pid lock file must be before we
	// consider it stale and remove it. GC takes at most ~30s, so 5 minutes
	// means the lock is definitely from a killed/crashed process.
	gcStaleLockTimeout = 5 * time.Minute

	// mirrorCloneTimeout is the maximum time allowed for git clone --mirror
	// when creating a new mirror. Prevents hanging on network issues while
	// holding the mutex.
	mirrorCloneTimeout = 5 * time.Minute

	// fetchTimeout is the maximum time allowed for git fetch --all on a mirror.
	fetchTimeout = 2 * time.Minute

	// fsckTimeout is the maximum time allowed for git fsck --no-full.
	fsckTimeout = 2 * time.Minute

	// gcTimeout is the maximum time allowed for git gc --auto.
	gcTimeout = 5 * time.Minute
)

// GitCacheManager maintains local git mirror clones to speed up workspace cloning.
// It runs a background goroutine that periodically fetches updates and checks mirror health.
type GitCacheManager struct {
	CacheDir string
	Interval time.Duration
	Logger   logging.SimpleLogging

	mu sync.Mutex // protects mirror creation
}

// Start begins the background refresh loop. It blocks until ctx is cancelled.
func (g *GitCacheManager) Start(ctx context.Context) {
	g.Logger.Info("git cache manager started, cache dir: %s, refresh interval: %s", g.CacheDir, g.Interval)
	g.safeRefreshAll()

	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			g.safeRefreshAll()
		case <-ctx.Done():
			g.Logger.Info("git cache manager stopped")
			return
		}
	}
}

func (g *GitCacheManager) safeRefreshAll() {
	defer func() {
		if r := recover(); r != nil {
			g.Logger.Err("git cache manager: recovered from panic: %v", r)
		}
	}()
	g.refreshAll()
}

// EnsureMirror ensures a mirror clone exists for the given repo.
// If it doesn't exist, it creates one. Returns the mirror path.
// This is safe to call concurrently.
func (g *GitCacheManager) EnsureMirror(logger logging.SimpleLogging, cloneURL string, repoFullName string) string {
	if g == nil || g.CacheDir == "" {
		return ""
	}

	mirrorPath := g.mirrorPath(repoFullName)

	if g.isValidMirror(mirrorPath) {
		return mirrorPath
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Double-check after acquiring lock
	if g.isValidMirror(mirrorPath) {
		return mirrorPath
	}

	logger.Info("creating git mirror for %s at %s", repoFullName, mirrorPath)

	if err := os.MkdirAll(filepath.Dir(mirrorPath), 0700); err != nil {
		logger.Warn("failed to create mirror parent dir: %v", err)
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), mirrorCloneTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "clone", "--mirror", cloneURL, mirrorPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Warn("failed to create mirror for %s: %s: %s", repoFullName, err, string(output))
		os.RemoveAll(mirrorPath)
		return ""
	}

	logger.Info("git mirror created for %s", repoFullName)
	return mirrorPath
}

func (g *GitCacheManager) mirrorPath(repoFullName string) string {
	return filepath.Join(g.CacheDir, repoFullName+".git")
}

func (g *GitCacheManager) isValidMirror(path string) bool {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

func (g *GitCacheManager) refreshAll() {
	entries, err := g.findMirrors()
	if err != nil {
		g.Logger.Warn("git cache refresh: failed to scan cache dir: %v", err)
		return
	}

	for _, mirrorPath := range entries {
		g.refreshMirror(mirrorPath)
	}
}

func (g *GitCacheManager) refreshMirror(mirrorPath string) {
	if !g.isHealthy(mirrorPath) {
		g.Logger.Warn("git cache: mirror %s is corrupt, removing", mirrorPath)
		os.RemoveAll(mirrorPath)
		return
	}

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer fetchCancel()

	cmd := exec.CommandContext(fetchCtx, "git", "-C", mirrorPath, "fetch", "--all")
	output, err := cmd.CombinedOutput()
	if err != nil {
		g.Logger.Warn("git cache: fetch failed for %s: %s: %s", mirrorPath, err, string(output))
	}

	// Run gc --auto to prevent pack file and loose object accumulation from
	// frequent fetches and force pushes. This is a one-shot command (not a
	// persistent mode) — it checks thresholds, runs gc if needed, and exits.
	// Must be called every cycle.
	//
	// Loose objects: individual object files in objects/XX/ created by single
	// operations (e.g. a commit amend or cherry-pick). Threshold: >6700.
	// Pack files: compressed bundles of objects created by fetch/clone. Each
	// git fetch that receives new objects creates a new small pack file.
	// These packs are additive (old packs still contain historical objects).
	// Over time, many small packs accumulate — git must search all pack
	// indexes to find an object, so more packs = slower lookups. Threshold: >50.
	//
	// When triggered, gc combines all packs into one optimized pack with
	// better delta compression, then removes the old small packs. This
	// reduces disk usage and speeds up object lookups.
	//
	// gc --auto is a no-op (~0.1s) unless thresholds are exceeded, in which
	// case it runs a full repack (~14-30s for large repos).
	// If gc becomes slow in the future, tune git's gc.autoPackLimit config
	// to trigger less often (e.g. git config gc.autoPackLimit 100).
	g.runGC(mirrorPath)
}

func (g *GitCacheManager) runGC(mirrorPath string) {
	lockPath := filepath.Join(mirrorPath, "gc.pid")

	// Remove stale gc lock if older than gcStaleLockTimeout. This handles
	// the case where Atlantis was killed after gc was terminated but before
	// we could clean up the lock file.
	if info, err := os.Stat(lockPath); err == nil {
		if time.Since(info.ModTime()) > gcStaleLockTimeout {
			g.Logger.Info("git cache: removing stale gc.pid lock for %s (age: %s)", mirrorPath, time.Since(info.ModTime()))
			os.Remove(lockPath)
		}
	}

	gcCtx, gcCancel := context.WithTimeout(context.Background(), gcTimeout)
	defer gcCancel()

	gcCmd := exec.CommandContext(gcCtx, "git", "-C", mirrorPath, "gc", "--auto")
	if gcOutput, gcErr := gcCmd.CombinedOutput(); gcErr != nil {
		// If gc failed (timeout or error), remove the lock file since we know
		// the process is dead (CommandContext kills it on timeout).
		os.Remove(lockPath)
		g.Logger.Warn("git cache: gc --auto failed for %s: %s: %s", mirrorPath, gcErr, string(gcOutput))
	}
}

func (g *GitCacheManager) isHealthy(mirrorPath string) bool {
	if !g.isValidMirror(mirrorPath) {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), fsckTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", mirrorPath, "fsck", "--no-full", "--no-progress")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// If fsck timed out, assume healthy — if it's actually corrupt,
		// the next plan will fail and user retries after goroutine recovers.
		if ctx.Err() == context.DeadlineExceeded {
			g.Logger.Warn("git cache: fsck timed out for %s, assuming healthy", mirrorPath)
			return true
		}
		g.Logger.Warn("git cache: fsck failed for %s: %s", mirrorPath, string(output))
		return false
	}
	return true
}

func (g *GitCacheManager) findMirrors() ([]string, error) {
	var mirrors []string

	err := filepath.Walk(g.CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".git") && path != g.CacheDir {
			if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
				mirrors = append(mirrors, path)
				return filepath.SkipDir
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking cache dir: %w", err)
	}
	return mirrors, nil
}
