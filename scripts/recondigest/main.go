// Command recondigest prints the digest and kind lines verify.sh needs from an
// lrecon --json report, so the verification script runs no Python.
package main

import (
	"encoding/json"
	"fmt"
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
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: recondigest <report.json>")
		os.Exit(2)
	}
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var r recon
	if err := json.Unmarshal(b, &r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	digest := r.Digest
	if digest == "" {
		digest = r.Target.FileSHA256
	}
	if digest == "" {
		digest = r.UUID
	}
	fmt.Println("lrecon digest:", digest)
	fmt.Println("lrecon kind:", r.Kind)
}
