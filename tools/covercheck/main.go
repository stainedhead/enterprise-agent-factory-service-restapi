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
	"strings"
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

// isFake reports whether pkg is a package of test fakes, named after its parent with "test" appended
// (usecase/teams/teamstest, reconcile/team/teamtest). A fake package has no tests of its own; the
// packages that use it prove it.
func isFake(pkg string) bool {
	parts := strings.Split(pkg, "/")
	if len(parts) < 2 {
		return false
	}
	last, parent := parts[len(parts)-1], parts[len(parts)-2]
	return last == "test" || last == parent+"test" || last == strings.TrimSuffix(parent, "s")+"test"
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
		if isFake(m[1]) {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err == nil && v < minPct {
			low = append(low, fmt.Sprintf("%s coverage %.1f%% is below %.0f%%", m[1], v, minPct))
		}
	}
	return low
}
