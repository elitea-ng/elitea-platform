package adminsurface

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// modulePath is the import path of this module.
const modulePath = "github.com/elitea-ng/elitea-platform/services/elitea-main"

// moduleRoot is the directory of this module on disk, found from this file.
var moduleRoot = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// <root>/internal/api/adminsurface/caller.go
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
}()

// caller names the function that asked the resolver, and whether it is in
// front of the handler (internal/api/middleware) or in it.
//
// It names the function by its SOURCE: the package directory of the frame's
// file and the top-level declaration that encloses the frame's line. The
// symbol name of a frame is not stable: a closure created by a function the
// compiler inlined is named after the function it was inlined INTO, and the
// shape of that name changed between Go releases (a gate built in the router
// reads `api.NewRouter.func12` on one, `middleware.RequireCentralPermissions…`
// on another). The file and line do not change with inlining.
//
// Frames of this package, and of the middleware's in-handler helpers
// (HoldsPermissionIn, HoldsResolvedPermission), and the standing-set helper
// auth.ResolveStandingAdministration (ADR-0059 R2.1b) are skipped, so the
// handler that called them is named.
func caller() (string, bool) {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		pkg, fn, ok := source(frame)
		if !ok {
			// Not a file of this module: name the symbol.
			return frame.Function, false
		}
		switch {
		case pkg == modulePath+"/internal/api/adminsurface":
		case pkg == modulePath+"/internal/api/middleware" && strings.HasPrefix(fn, "Holds"):
		case pkg == modulePath+"/internal/auth" && fn == "ResolveStandingAdministration":
		case pkg == modulePath+"/internal/api/middleware":
			// Every other function of the middleware package runs in front of
			// the handler: the Require* gates, and middleware such as the
			// maintenance window that decides for the whole API.
			return pkg + "." + fn, true
		default:
			return pkg + "." + fn, false
		}
		if !more {
			return "", false
		}
	}
}

// source answers the package import path and the enclosing top-level
// declaration ("Name" or "(*Recv).Name") of a frame in this module.
func source(frame runtime.Frame) (pkg, fn string, ok bool) {
	if moduleRoot == "" || frame.File == "" {
		return "", "", false
	}
	rel, err := filepath.Rel(moduleRoot, filepath.Dir(frame.File))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", false
	}
	fn, ok = enclosing(frame.File, frame.Line)
	if !ok {
		return "", "", false
	}
	pkg = modulePath
	if rel != "." {
		pkg += "/" + filepath.ToSlash(rel)
	}
	return pkg, fn, true
}

type declSpan struct {
	from, to int
	name     string
}

var (
	declMu    sync.Mutex
	declCache = map[string][]declSpan{}
)

// enclosing answers the top-level function declaration of file that holds
// line. Each file is parsed once.
func enclosing(file string, line int) (string, bool) {
	declMu.Lock()
	spans, cached := declCache[file]
	if !cached {
		spans = parseDecls(file)
		declCache[file] = spans
	}
	declMu.Unlock()
	for _, span := range spans {
		if line >= span.from && line <= span.to {
			return span.name, true
		}
	}
	return "", false
}

func parseDecls(file string) []declSpan {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var spans []declSpan
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			name = "(" + receiver(fd.Recv.List[0].Type) + ")." + name
		}
		spans = append(spans, declSpan{
			from: fset.Position(fd.Pos()).Line,
			to:   fset.Position(fd.End()).Line,
			name: name,
		})
	}
	return spans
}

func receiver(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return "*" + receiver(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return receiver(e.X)
	case *ast.IndexListExpr:
		return receiver(e.X)
	}
	return "?"
}
