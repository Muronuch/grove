package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Muronuch/grove/internal/config"
)

const KeyVersion = 1

const maxInputFiles = 20000

type KeyInput struct {
	Driver   string
	Image    string
	Prepare  config.PrepareConfig
	Worktree string
	Inputs   []string
}

type KeyResult struct {
	Key   string
	Files []string
	Bytes int64
}

func ComputeKey(in KeyInput) (KeyResult, error) {
	files, bytes, err := matchInputs(in.Worktree, in.Inputs)
	if err != nil {
		return KeyResult{}, err
	}

	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			fmt.Fprintf(h, "%d:%s\x00", len(p), p)
		}
	}
	write("v", fmt.Sprint(KeyVersion))
	write("driver", in.Driver)
	write("image", in.Image)
	write("mode", string(in.Prepare.Mode))
	write("prepare-service", in.Prepare.Service)
	write("command", strings.Join(in.Prepare.Command, "\x00"))

	for _, rel := range files {
		sum, err := hashFile(filepath.Join(in.Worktree, filepath.FromSlash(rel)))
		if err != nil {
			return KeyResult{}, err
		}
		write("file", rel, sum)
	}
	return KeyResult{Key: hex.EncodeToString(h.Sum(nil)), Files: files, Bytes: bytes}, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash input %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash input %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func matchInputs(root string, globs []string) ([]string, int64, error) {
	if len(globs) == 0 {
		return nil, 0, nil
	}
	patterns := make([]string, 0, len(globs))
	for _, g := range globs {
		patterns = append(patterns, filepath.ToSlash(strings.TrimPrefix(g, "./")))
	}

	var (
		out   []string
		total int64
	)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			if !anyPrefixPossible(patterns, rel) {
				return fs.SkipDir
			}
			return nil
		}
		for _, pat := range patterns {
			if MatchGlob(pat, rel) {
				info, ierr := d.Info()
				if ierr == nil {
					total += info.Size()
				}
				out = append(out, rel)
				break
			}
		}
		if len(out) > maxInputFiles {
			return fmt.Errorf("inputs match more than %d files; narrow the globs in %s",
				maxInputFiles, config.FileName)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(out)
	return out, total, nil
}

func MatchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return len(name) > 0
			}

			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

func anyPrefixPossible(patterns []string, dir string) bool {
	segs := strings.Split(dir, "/")
	for _, pat := range patterns {
		if prefixPossible(strings.Split(pat, "/"), segs) {
			return true
		}
	}
	return false
}

func prefixPossible(pat, dir []string) bool {
	for i := 0; i < len(dir); i++ {
		if len(pat) == 0 {
			return false
		}
		if pat[0] == "**" {
			return true
		}
		ok, err := path.Match(pat[0], dir[i])
		if err != nil || !ok {
			return false
		}
		pat = pat[1:]
	}
	return true
}
