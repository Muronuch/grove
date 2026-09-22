package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string {
	if d == 0 {
		return "0s"
	}
	td := time.Duration(d)
	if td%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(td/(24*time.Hour)), 10) + "d"
	}
	return td.String()
}

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	if i := strings.IndexByte(s, 'd'); i >= 0 && i+1 <= len(s) {
		head, tail := s[:i], s[i+1:]
		if n, err := strconv.ParseFloat(head, 64); err == nil {
			total := time.Duration(n * 24 * float64(time.Hour))
			if tail == "" {
				return total, nil
			}
			rest, err := time.ParseDuration(tail)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q: %w", s, err)
			}
			return total + rest, nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (want e.g. 30s, 15m, 2h, 14d)", s)
	}
	return v, nil
}

type Size struct {
	Bytes   int64
	Percent float64
	text    string
}

func (s Size) IsZero() bool { return s.Bytes == 0 && s.Percent == 0 }

func (s Size) String() string {
	if s.text != "" {
		return s.text
	}
	if s.Percent > 0 {
		return strconv.FormatFloat(s.Percent, 'f', -1, 64) + "%"
	}
	return strconv.FormatInt(s.Bytes, 10)
}

func (s Size) Resolve(total int64) int64 {
	if s.Percent > 0 {
		return int64(float64(total) * s.Percent / 100)
	}
	return s.Bytes
}

func (s *Size) UnmarshalText(b []byte) error {
	v, err := ParseSize(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

func (s Size) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"tb", 1 << 40},
	{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
	{"b", 1},
}

func ParseSize(s string) (Size, error) {
	orig := strings.TrimSpace(s)
	l := strings.ToLower(orig)
	if l == "" {
		return Size{}, fmt.Errorf("empty size")
	}
	if strings.HasSuffix(l, "%") {
		p, err := strconv.ParseFloat(strings.TrimSuffix(l, "%"), 64)
		if err != nil || p <= 0 || p > 100 {
			return Size{}, fmt.Errorf("invalid percentage %q (want 1%%..100%%)", orig)
		}
		return Size{Percent: p, text: orig}, nil
	}
	for _, u := range sizeUnits {
		if strings.HasSuffix(l, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(l, u.suffix), 64)
			if err != nil {
				continue
			}
			return Size{Bytes: int64(n * float64(u.mult)), text: orig}, nil
		}
	}
	n, err := strconv.ParseInt(l, 10, 64)
	if err != nil {
		return Size{}, fmt.Errorf("invalid size %q (want e.g. 512m, 8g, 50%%)", orig)
	}
	return Size{Bytes: n, text: orig}, nil
}
