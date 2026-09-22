package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Muronuch/grove/internal/finding"
)

func main() {
	out := flag.String("out", "docs/doctor-codes.md", "file to write")
	check := flag.Bool("check", false, "fail if the file is out of date instead of writing it")
	flag.Parse()

	want := finding.Markdown()
	if *check {
		got, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if string(got) != want {
			fmt.Fprintf(os.Stderr, "%s is out of date; run: go generate ./internal/finding\n", *out)
			os.Exit(1)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, []byte(want), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
