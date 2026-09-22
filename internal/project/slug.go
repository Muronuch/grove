package project

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const MaxSlugLen = 30

func Slugify(branch string) string {
	var b strings.Builder
	b.Grow(len(branch))
	lastDash := true
	for _, r := range strings.ToLower(branch) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > MaxSlugLen {
		s = strings.TrimRight(s[:MaxSlugLen], "-")
	}
	if s == "" {
		s = "env"
	}

	return s
}

func SlugFor(branch string, taken func(slug string) bool) string {
	base := Slugify(branch)
	if taken == nil || !taken(base) {
		return base
	}
	sum := sha256.Sum256([]byte(branch))
	suffix := "-" + hex.EncodeToString(sum[:])[:4]
	trunc := base
	if len(trunc)+len(suffix) > MaxSlugLen {
		trunc = strings.TrimRight(base[:MaxSlugLen-len(suffix)], "-")
	}
	candidate := trunc + suffix
	if !taken(candidate) {
		return candidate
	}

	for n := 6; n <= 16; n += 2 {
		suffix = "-" + hex.EncodeToString(sum[:])[:n]
		trunc = base
		if len(trunc)+len(suffix) > MaxSlugLen {
			trunc = strings.TrimRight(base[:MaxSlugLen-len(suffix)], "-")
		}
		if c := trunc + suffix; !taken(c) {
			return c
		}
	}
	return candidate
}

func AllocateSlot(used map[int]bool, max int) int {
	for i := 1; i <= max; i++ {
		if !used[i] {
			return i
		}
	}
	return 0
}
