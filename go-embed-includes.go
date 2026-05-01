package main

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/scanner"
	"unicode"
	"unicode/utf8"
)

func main() {
	seenIncludes := map[string]bool{}
	if err := filepath.WalkDir(".", func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if filepath.Ext(filePath) == ".go" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(filePath) != ".go" {
			return nil
		}

		workspacePath := strings.TrimPrefix(filepath.ToSlash(filePath), "./")
		patterns, err := embedPatterns(workspacePath)
		if err != nil {
			return fmt.Errorf("%s: %w", workspacePath, err)
		}
		for _, pattern := range patterns {
			for _, include := range includePatterns(workspacePath, pattern) {
				if include == "" || seenIncludes[include] {
					continue
				}
				seenIncludes[include] = true
				fmt.Println(include)
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func embedPatterns(filePath string) ([]string, error) {
	src, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var s scanner.Scanner
	s.Init(strings.NewReader(string(src)))
	s.Mode = scanner.GoTokens &^ scanner.SkipComments

	var patterns []string
	for tok := s.Scan(); tok != scanner.EOF; tok = s.Scan() {
		if tok != scanner.Comment {
			continue
		}
		commentPatterns, err := parseGoEmbed(s.TokenText())
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, commentPatterns...)
	}
	return patterns, nil
}

func parseGoEmbed(comment string) ([]string, error) {
	const directive = "//go:embed"
	comment = strings.TrimSpace(comment)
	if !strings.HasPrefix(comment, directive) {
		return nil, nil
	}
	args := strings.TrimPrefix(comment, directive)
	if args != "" {
		r, _ := utf8.DecodeRuneInString(args)
		if !unicode.IsSpace(r) {
			return nil, nil
		}
	}
	return parseGoEmbedArgs(args)
}

func parseGoEmbedArgs(args string) ([]string, error) {
	var patterns []string
	for args = strings.TrimSpace(args); args != ""; args = strings.TrimSpace(args) {
		var pattern string
		switch args[0] {
		case '`':
			i := strings.Index(args[1:], "`")
			if i < 0 {
				return nil, fmt.Errorf("invalid quoted string in //go:embed: %s", args)
			}
			pattern = args[1 : i+1]
			args = args[i+2:]
		case '"':
			i := 1
			for ; i < len(args); i++ {
				if args[i] == '\\' {
					i++
					continue
				}
				if args[i] == '"' {
					quoted := args[:i+1]
					unquoted, err := strconv.Unquote(quoted)
					if err != nil {
						return nil, fmt.Errorf("invalid quoted string in //go:embed: %s", quoted)
					}
					pattern = unquoted
					args = args[i+1:]
					break
				}
			}
			if pattern == "" {
				return nil, fmt.Errorf("invalid quoted string in //go:embed: %s", args)
			}
		default:
			i := len(args)
			for j, r := range args {
				if unicode.IsSpace(r) {
					i = j
					break
				}
			}
			pattern = args[:i]
			args = args[i:]
		}

		if args != "" {
			r, _ := utf8.DecodeRuneInString(args)
			if !unicode.IsSpace(r) {
				return nil, fmt.Errorf("invalid quoted string in //go:embed: %s", args)
			}
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

func includePatterns(filePath, pattern string) []string {
	pattern = strings.TrimPrefix(pattern, "all:")
	dir := path.Dir(filepath.ToSlash(filePath))
	if dir == "." {
		dir = ""
	}
	include := path.Clean(path.Join(dir, pattern))
	if include == "." {
		return nil
	}
	return []string{include, include + "/**"}
}
