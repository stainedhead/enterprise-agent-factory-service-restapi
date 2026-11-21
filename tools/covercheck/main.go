// Command covercheck runs go test -cover on the given packages and fails when any package with
// statements is below the minimum coverage.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
)

var coverRE = regexp.MustCompile(`^(?:ok\s+)?\s*(\S+)\s.*coverage: ([0-9.]+)% of statements`)

func main() {
	minPct := flag.Float64("min", 90, "minimum coverage percent per package")
	flag.Parse()
	cmd := exec.CommandContext(context.Background(), "go", append([]string{"test", "-cover"}, flag.Args()...)...) //nolint:gosec // developer tool; args come from the Makefile
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &out)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "covercheck: go test failed:", err)
		os.Exit(1)
	}
	low := check(&out, *minPct)
	for _, l := range low {
		fmt.Fprintln(os.Stderr, "covercheck:", l)
	}
	if len(low) > 0 {
		os.Exit(1)
	}
}

// check returns one message per package below minPct.
func check(r io.Reader, minPct float64) []string {
	var low []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		m := coverRE.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err == nil && v < minPct {
			low = append(low, fmt.Sprintf("%s coverage %.1f%% is below %.0f%%", m[1], v, minPct))
		}
	}
	return low
}
