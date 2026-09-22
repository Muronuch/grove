package cli

import (
	"strings"
	"testing"
)

func TestVisibleWidthIgnoresColour(t *testing.T) {
	cases := map[string]int{
		"running":                  7,
		"\x1b[32mrunning\x1b[0m":   7,
		"":                         0,
		"\x1b[2m\x1b[0m":           0,
		"—":                        1,
		"\x1b[1mfeat-x\x1b[0m · 2": 10,
	}
	for in, want := range cases {
		if got := visibleWidth(in); got != want {
			t.Errorf("visibleWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestTableAlignsColouredCells(t *testing.T) {
	green := func(s string) string { return "\x1b[32m" + s + "\x1b[0m" }
	header := []string{"\x1b[2mENV\x1b[0m", "\x1b[2mSTATE\x1b[0m", "\x1b[2mURL\x1b[0m"}
	rows := [][]string{
		{"feat-warehouse-rebuild", green("running"), "http://a.localhost"},
		{"feat-hr", green("paused"), "http://b.localhost"},
	}

	lines := strings.Split(strings.TrimRight(renderTable(header, rows), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}

	want := columnStarts(t, lines[0])
	for _, line := range lines[1:] {
		if got := columnStarts(t, line); !equal(got, want) {
			t.Errorf("column starts %v, want %v\nheader: %q\nrow:    %q", got, want, lines[0], line)
		}
	}
}

func TestTableNeverPadsTheLastColumn(t *testing.T) {
	out := renderTable([]string{"A", "B"}, [][]string{{"x", "yyy"}, {"xx", "y"}})
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("line %q ends in blanks", line)
		}
	}
}

func TestWipeLinesClearsEveryRow(t *testing.T) {
	got := wipeLines("one\n\nthree\n")
	want := "one" + clearLine + "\n" + clearLine + "\nthree" + clearLine + "\n"
	if got != want {
		t.Errorf("wipeLines() = %q, want %q", got, want)
	}
}

func columnStarts(t *testing.T, line string) []int {
	t.Helper()
	starts := []int{0}
	col, gap := 0, 0
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			i = skipEscape(line, i)
			continue
		}
		if line[i] == ' ' {
			gap++
		} else {
			if gap >= 2 {
				starts = append(starts, col)
			}
			gap = 0
		}
		col++
		i++
	}
	return starts
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
