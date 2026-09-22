package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
)

const TokenFile = "router.token"

func (s *Store) RouterToken() (string, error) {
	path := s.Path(TokenFile)
	b, err := os.ReadFile(path)
	if err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			if err := checkTokenPerms(path); err != nil {
				return "", err
			}
			return tok, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return tok, nil
}

func (s *Store) ResetRouterToken() (string, error) {
	if err := os.Remove(s.Path(TokenFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return s.RouterToken()
}

func checkTokenPerms(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %#o); run: chmod 600 %s",
			path, st.Mode().Perm(), path)
	}
	return nil
}

func HookHash(commands []string) string {
	h := sha256.New()
	for _, c := range commands {
		h.Write([]byte(c))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (p *ProjectState) HooksTrusted(commands []string) bool {
	if len(commands) == 0 {
		return true
	}
	return slices.Contains(p.TrustedHooks, HookHash(commands))
}

func (r *Registry) TrustHooks(p *ProjectState, commands []string) {
	if len(commands) == 0 {
		return
	}
	h := HookHash(commands)
	if slices.Contains(p.TrustedHooks, h) {
		return
	}
	p.TrustedHooks = append(p.TrustedHooks, h)
	r.Touch()
}
