package safe_test

import (
	"os/exec"
	"strings"
	"testing"
)

// modulePath is this repository's module path. Anything under it is dbbat code,
// which is precisely what must not appear in this package's dependency closure.
const modulePath = "github.com/fclairamb/dbbat"

// packagePath is internal/safe itself, which `go list -deps` lists last, after
// its dependencies.
const packagePath = modulePath + "/internal/safe"

// TestPackageImportsOnlyStandardLibrary is the pin for the one property that
// makes internal/safe worth being its own package.
//
// The guards used to live in internal/proxy/shared, which imports internal/cache
// and internal/proxy/upstream. Goroutines in *those* packages therefore could
// not reach the guards — importing back would be a cycle — so each hand-copied
// the recover, and the copies were kept in step by a comment. Being a leaf is
// what deleted those copies, and a leaf stops being one the moment somebody adds
// a convenience import that looks harmless.
//
// The check is `go list -deps`, which walks the full transitive closure (a
// stdlib-looking direct import that itself pulls in dbbat code is caught too),
// resolved entirely from the module cache — no network. Test-only imports are
// deliberately out of scope: this file's own dependencies are not linked into
// the binary and cannot create a cycle.
func TestPackageImportsOnlyStandardLibrary(t *testing.T) {
	t.Parallel()

	out, err := exec.Command("go", "list", "-deps", packagePath).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}

	for _, dep := range strings.Fields(string(out)) {
		if dep == packagePath {
			continue
		}

		// A standard-library path has no dot in its first element:
		// "log/slog" is stdlib, "github.com/..." is not. That is the same
		// rule the go command itself uses to tell the two apart.
		first, _, _ := strings.Cut(dep, "/")
		if !strings.Contains(first, ".") && !strings.HasPrefix(dep, modulePath) {
			continue
		}

		t.Errorf("internal/safe must import only the standard library, but its closure contains %q; "+
			"keeping it a leaf is what lets every package in the process use the same panic guards", dep)
	}
}
