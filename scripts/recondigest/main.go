// Command recondigest prints the digest and kind lines verify.sh needs from an
// lrecon --json report, so the verification script runs no Python.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type recon struct {
	Digest string `json:"digest"`
	UUID   string `json:"uuid"`
	Kind   string `json:"kind"`
	Target struct {
		FileSHA256 string `json:"file_sha256"`
	} `json:"target"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the process exit status. A report without a digest is an error,
// never an empty "lrecon digest:" line.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: recondigest <report.json>")
		return 2
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var r recon
	if err := json.Unmarshal(b, &r); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	digest := r.Digest
	if digest == "" {
		digest = r.Target.FileSHA256
	}
	if digest == "" {
		digest = r.UUID
	}
	if digest == "" {
		fmt.Fprintln(stderr, "recondigest: no digest in report")
		return 1
	}
	fmt.Fprintln(stdout, "lrecon digest:", digest)
	fmt.Fprintln(stdout, "lrecon kind:", r.Kind)
	return 0
}
