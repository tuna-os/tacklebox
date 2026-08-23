package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fileCreatingCalls are the calls that bring a path into existence on the
// host filesystem. A fixed /tmp/ literal reaching one of these is the bug
// this test exists for.
var fileCreatingCalls = map[string]bool{
	"os.WriteFile":     true,
	"os.Create":        true,
	"os.OpenFile":      true,
	"os.Mkdir":         true,
	"os.MkdirAll":      true,
	"ioutil.WriteFile": true,
	"ioutil.TempFile":  true, // dir argument
	"os.NewFile":       true,
	"os.Symlink":       true,
	"os.Rename":        true,
	"os.CreateTemp":    true, // dir argument
	"os.MkdirTemp":     true, // dir argument
	"os.WriteString":   true,
	"runner.WriteFile": true,
	"sudoWriteFile":    true,
	"os.Chmod":         true,
	"os.Chown":         true,
	"os.Truncate":      true,
	"os.RemoveAll":     true,
	"os.Remove":        true,
	"filepath.WalkDir": true,
	"filepath.Walk":    true,
}

// TestNoFixedTmpPathsAreWritten fails if a non-test Go source file passes a
// fixed path under /tmp to a call that creates or mutates a host file.
//
// This exists because that pattern has already shipped once and survived
// being reported. Issue #43 flagged os.WriteFile("/tmp/tbox-recipe.json", …)
// followed by `sudo cp` — a predictable, world-writable staging path for a
// file that lands as /etc/tacklebox/recipe.json, which the root update-all
// timer reads to decide which images to pull and deploy. The issue was closed
// COMPLETED on a comment saying the fix had been applied. It had not: the line
// was still in build.go at every commit either side of the closure, and stayed
// there for ten more weeks.
//
// A reviewer cannot be relied on to spot one /tmp/ literal in a 1300-line
// file, and a closing comment is not a verification. This test is.
//
// Two deliberate narrowings keep it free of false positives:
//
//   - It matches string literals in the AST, not text, so the comment in
//     provisionUpdateSystem that explains this history does not trip it.
//   - It only flags literals that are ARGUMENTS to a host-file call. Fixed
//     /tmp paths are perfectly correct as container-side mount targets —
//     internal/install/remora.go returns "/tmp/remora-manifest.json" as the
//     path inside the podman container, with the host side staged through
//     os.CreateTemp and bind-mounted :ro. That is right, and this test leaves
//     it alone.
//
// Staging through a temp file to reach a root-owned destination is fine and
// necessary — see sudoWriteFile in media.go. The requirement is only that the
// host-side name comes from os.CreateTemp / os.MkdirTemp, which give O_EXCL,
// mode 0600 or 0700, and a name nobody can predict.
func TestNoFixedTmpPathsAreWritten(t *testing.T) {
	// Repo root, from cmd/tacklebox.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	fset := token.NewFileSet()
	var findings []string

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "fixtures":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Not this test's job to police syntax; the build does that.
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !fileCreatingCalls[callName(call.Fun)] {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, uerr := strconv.Unquote(lit.Value)
				if uerr != nil || !strings.HasPrefix(val, "/tmp/") {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				findings = append(findings, rel+":"+
					strconv.Itoa(fset.Position(lit.Pos()).Line)+" "+
					callName(call.Fun)+"("+strconv.Quote(val)+", …)")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(findings) > 0 {
		t.Errorf("fixed /tmp path passed to a host-file call; use os.CreateTemp "+
			"or os.MkdirTemp so the name is unpredictable and the file is "+
			"created with O_EXCL:\n  %s", strings.Join(findings, "\n  "))
	}
}

// callName renders a call's callee as "pkg.Func" or "Func", or "" when it is
// neither (a method on a value, a func literal, …).
func callName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
	}
	return ""
}
