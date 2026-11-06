package core

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Git keeps one bare mirror of the playbook repo and hands out read-only
// checkouts keyed by commit, so every ansible run on the same commit shares
// one tree and a burst of callers costs one fetch.
type Git struct {
	bin, url string
	gitDir   string
	workDir  string
	ttl      time.Duration

	mu        sync.Mutex
	lastFetch time.Time
	trees     map[string]*tree
}

type tree struct {
	path string
	refs int
	used time.Time
}

const idleTrees = 3 // unused checkouts kept for reuse

func NewGit(cfg Config) (*Git, error) {
	g := &Git{
		bin:     cfg.GitBin,
		url:     cfg.Repo,
		gitDir:  filepath.Join(cfg.DataDir, "git", "repo.git"),
		workDir: filepath.Join(cfg.DataDir, "git", "work"),
		ttl:     cfg.FetchTTL,
		trees:   map[string]*tree{},
	}
	// Checkouts are cheap to rebuild and nothing references them after a restart.
	os.RemoveAll(g.workDir)
	if err := os.MkdirAll(g.workDir, 0o700); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Git) run(ctx context.Context, auth []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.bin, args...)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=file:https:http:ssh:git",
		"GIT_CONFIG_NOSYSTEM=1",
	}, auth...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(tail(errb.Bytes(), 600)))
	}
	return out.Bytes(), nil
}

func (g *Git) fetch(ctx context.Context, auth []string) error {
	if _, err := os.Stat(filepath.Join(g.gitDir, "HEAD")); err != nil {
		if _, err := g.run(ctx, nil, "init", "--bare", "--quiet", g.gitDir); err != nil {
			return err
		}
	}
	_, err := g.run(ctx, auth, "--git-dir", g.gitDir, "fetch", "--quiet", "--prune", "--force", g.url,
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	if err == nil {
		g.lastFetch = time.Now()
	}
	return err
}

func (g *Git) resolve(ctx context.Context, ref string) string {
	cands := []string{"refs/heads/" + ref, "refs/tags/" + ref}
	if shaRe.MatchString(ref) {
		cands = append(cands, ref)
	}
	for _, c := range cands {
		if out, err := g.run(ctx, nil, "--git-dir", g.gitDir, "rev-parse", "--verify", "--quiet", c+"^{commit}"); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}

// Checkout returns the commit and a directory holding the repo at ref. The
// caller must call release when the run is over.
func (g *Git) Checkout(ctx context.Context, ref string, auth []string) (sha, dir string, release func(), err error) {
	if err = validRef(ref); err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	fetched := false
	if time.Since(g.lastFetch) > g.ttl {
		if err = g.fetch(ctx, auth); err != nil {
			return
		}
		fetched = true
	}
	sha = g.resolve(ctx, ref)
	if sha == "" && !fetched {
		// Might be a branch pushed within the fetch TTL.
		if err = g.fetch(ctx, auth); err != nil {
			return
		}
		sha = g.resolve(ctx, ref)
	}
	if sha == "" {
		err = fmt.Errorf("branch/ref %q not found in %s", ref, g.url)
		return
	}

	t := g.trees[sha]
	if t == nil {
		path := filepath.Join(g.workDir, sha)
		if err = g.extract(ctx, sha, path); err != nil {
			return
		}
		t = &tree{path: path}
		g.trees[sha] = t
	}
	t.refs++
	t.used = time.Now()
	return sha, t.path, func() { g.release(sha) }, nil
}

func (g *Git) release(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t := g.trees[sha]; t != nil {
		t.refs--
		t.used = time.Now()
	}
	for {
		idle, oldest := 0, ""
		for s, t := range g.trees {
			if t.refs > 0 {
				continue
			}
			idle++
			if oldest == "" || t.used.Before(g.trees[oldest].used) {
				oldest = s
			}
		}
		if idle <= idleTrees {
			return
		}
		os.RemoveAll(g.trees[oldest].path)
		delete(g.trees, oldest)
	}
}

// extract writes the tree of sha to path from `git archive`, with no .git.
func (g *Git) extract(ctx context.Context, sha, path string) error {
	tmp := path + ".tmp"
	os.RemoveAll(tmp)
	os.RemoveAll(path)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, g.bin, "--git-dir", g.gitDir, "archive", "--format=tar", sha)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	uerr := untar(out, tmp)
	io.Copy(io.Discard, out)
	if werr := cmd.Wait(); werr != nil {
		uerr = fmt.Errorf("git archive: %v: %s", werr, strings.TrimSpace(errb.String()))
	}
	if uerr != nil {
		os.RemoveAll(tmp)
		return uerr
	}
	return os.Rename(tmp, path)
}

func untar(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := filepath.Clean(h.Name)
		if name == "." {
			continue
		}
		if name == ".." || strings.HasPrefix(name, "../") || filepath.IsAbs(name) {
			return errors.New("archive entry leaves the checkout: " + h.Name)
		}
		dst := filepath.Join(root, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o400)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, dst); err != nil {
				return err
			}
		}
	}
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
