// Command ldpreloadgen writes the marker-only probe for offline inspection.
// Normal ARTEX builds construct the same bytes directly from source and do not
// need this command or a checked-in shared library.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Autumn-27/artex/session/internal/ldpreload"
)

func main() {
	dst := filepath.Join("session", "ldpreload_x86_64.so")
	if len(os.Args) > 1 {
		dst = os.Args[1]
	}
	blob := ldpreload.Build()
	if err := os.WriteFile(dst, blob, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "ldpreloadgen:", err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s (%d 字节)\n", dst, len(blob))
}
