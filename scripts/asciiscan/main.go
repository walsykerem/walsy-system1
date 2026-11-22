// Command asciiscan flags any non-ASCII byte in the files verify.sh scans. It
// is byte-accurate: only bytes >= 0x80 are reported, so tab (0x09) and printable
// ASCII (0x20-0x7E) never trip it. Prints "ascii clean: N files" and exits
// non-zero on the first non-ASCII byte of any file.
package main

import (
	"bytes"
	"fmt"
	"os"
)

func main() {
	files := os.Args[1:]
	bad := false
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "non-ascii: %s: %v\n", path, err)
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
			fmt.Fprintf(os.Stderr, "non-ascii: %s:%d\n", path, 1+bytes.Count(b[:i], []byte{'\n'}))
			bad = true
		}
	}
	if bad {
		os.Exit(1)
	}
	fmt.Printf("ascii clean: %d files\n", len(files))
}
