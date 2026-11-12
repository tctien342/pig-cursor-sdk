package pig_cursor_sdk

// A `package main` anywhere under the extension root makes pig refuse to load it:
// "contains both factory and standalone main packages; select one exact root".
// That error looks like a broken extension to anyone who pulls the repo, so the
// layout is asserted here instead of discovered by a user.
//
// dev/ is a separate Go module and is where probes and one-off tools belong.
import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoMainPackageInExtensionRoot(t *testing.T) {
	excluded := map[string]bool{".git": true, "dev": true, "gen": true, "docs": true, ".pi": true}
	fset := token.NewFileSet()

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if excluded[info.Name()] && path != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.PackageClauseOnly)
		if perr != nil {
			return perr
		}
		if f.Name.Name == "main" {
			t.Errorf("%s declares package main: pig will refuse to load the extension. Put tools in dev/ (separate module).", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
