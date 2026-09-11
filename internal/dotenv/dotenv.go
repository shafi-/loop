// Package dotenv loads KEY=VALUE environment files (the .env convention).
// Policy: the real environment always wins — .env fills gaps, never
// overrides exported variables.
package dotenv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads path and sets every variable that is not already present in
// the environment. Missing files are not an error (the caller may not use
// .env at all); malformed lines are returned as warnings so typos surface
// instead of silently producing empty keys.
func Load(path string) (loaded int, warnings []string, err error) {
	data, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	defer data.Close()

	base := filepath.Base(path)
	sc := bufio.NewScanner(data)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s:%d: not KEY=VALUE, ignored", base, lineNo))
			continue
		}
		key = strings.TrimSpace(key)
		value = cleanValue(strings.TrimSpace(value))
		if key == "" {
			warnings = append(warnings, fmt.Sprintf("%s:%d: empty key, ignored", base, lineNo))
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // real environment wins
		}
		if err := os.Setenv(key, value); err != nil {
			return loaded, warnings, err
		}
		loaded++
	}
	return loaded, warnings, sc.Err()
}

// cleanValue strips one layer of matching surrounding quotes and an
// inline trailing comment outside quotes (VALUE # note).
func cleanValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}
