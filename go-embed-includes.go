// go.dang runs this helper inside the mounted source tree.
// Input: one Go source file path.
// Output: parsed //go:embed patterns from that file, one per line, with all: stripped.
package main

import (
	"errors"
	"fmt"
	"go/build"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: go run go-embed-includes.go -- <go-file>")
		os.Exit(2)
	}

	patterns, err := embedPatterns(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, pattern := range patterns {
		fmt.Println(pattern)
	}
}

func embedPatterns(filePath string) ([]string, error) {
	pkg, err := build.ImportDir(filepath.Dir(filePath), build.ImportComment)
	if err != nil {
		var noGo *build.NoGoError
		if !errors.As(err, &noGo) {
			return nil, err
		}
	}
	if pkg == nil {
		return nil, nil
	}

	var patterns []string
	patterns = append(patterns, patternsInFile(filePath, pkg.EmbedPatterns, pkg.EmbedPatternPos)...)
	patterns = append(patterns, patternsInFile(filePath, pkg.TestEmbedPatterns, pkg.TestEmbedPatternPos)...)
	patterns = append(patterns, patternsInFile(filePath, pkg.XTestEmbedPatterns, pkg.XTestEmbedPatternPos)...)
	return patterns, nil
}

func patternsInFile(filePath string, patterns []string, positions map[string][]token.Position) []string {
	var matches []string
	for _, pattern := range patterns {
		for _, pos := range positions[pattern] {
			if sameFile(filePath, pos.Filename) {
				matches = append(matches, strings.TrimPrefix(pattern, "all:"))
				break
			}
		}
	}
	return matches
}

func sameFile(a, b string) bool {
	absA, err := filepath.Abs(a)
	if err == nil {
		a = absA
	}
	absB, err := filepath.Abs(b)
	if err == nil {
		b = absB
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
