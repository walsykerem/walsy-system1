// Command asciiscan flags any non-ASCII byte in the files verify.sh scans. It
// is byte-accurate: only bytes >= 0x80 are reported, so tab (0x09) and printable
// ASCII (0x20-0x7E) never trip it. Prints "ascii clean: N files" and exits
// non-zero on the first non-ASCII byte of any file, or when a listed input path
// is missing or not a regular file, or when no path is given at all (an empty
// list would otherwise report "ascii clean: 0 files" without scanning).
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run scans every listed path and returns the process exit status. An unusable
// path is an error, not a silent skip.
func run(files []string, stdout, stderr io.Writer) int {
	if len(files) == 0 {
		fmt.Fprintln(stderr, "asciiscan: no files given")
		return 2
	}
	bad := false
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(stderr, "asciiscan: %s: %v\n", path, err)
			bad = true
			continue
		}
		if !info.Mode().IsRegular() {
			fmt.Fprintf(stderr, "asciiscan: %s: not a regular file\n", path)
			bad = true
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "non-ascii: %s: %v\n", path, err)
			bad = true
			continue
		}
		i := -1
		for j, c := range b {
			if c >= 0x80 {
				i = j
				break
			}
		}
		if i >= 0 {
			fmt.Fprintf(stderr, "non-ascii: %s:%d\n", path, 1+bytes.Count(b[:i], []byte{'\n'}))
			bad = true
		}
	}
	if bad {
		return 1
	}
	fmt.Fprintf(stdout, "ascii clean: %d files\n", len(files))
	return 0
}
