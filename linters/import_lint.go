package linters

// This file contains rules about who can import from whom.
//
// See also:
//  banned_symbol_lint.go for rules about who can import a specific
//		package or symbol
//	dev/consistency_test/import_test.go for checking more global properties,
//		like "nothing deployed to prod can import from dev"
// This file is for dynamic but per-package rules, like "no service can import
// from another service".

import (
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// ImportAnalyzer enforces various import rules.
//
// Current rules:
// - pkg can't import from services
// - no service can import from another service
// - only resolvers can import a service's generated/graphql package
var ImportAnalyzer = &analysis.Analyzer{
	Name: "import",
	Doc:  "rules about who can import from whom",
	Run:  _runImportLint,
}

const _webappPrefix = "github.com/Khan/webapp/"

// _webappArea takes a slice of path-parts and returns the first two
// path-parts, then the rest of the path (if any).  (For example, for
// "github.com/Khan/webapp/services/myservice/mypackage/subpackage" we'd return
// ("services", "myservice", "mypackage/subpackage").  If the path is in webapp
// but has fewer than two path-parts, or is not in webapp, return ("", "", "").
func _webappArea(path string) (area, subArea, rest string) {
	if !strings.HasPrefix(path, _webappPrefix) {
		return "", "", ""
	}
	path = path[len(_webappPrefix):]

	split := strings.SplitN(path, "/", 3)
	switch len(split) {
	case 0, 1:
		return "", "", ""
	case 2:
		return split[0], split[1], ""
	case 3:
		return split[0], split[1], split[2]
	default:
		panic(fmt.Sprintf("unexpected length %v", len(split)))
	}
}

// _checkImport returns an error message if we should prohibit the given
// import, or "" if it's okay.
//
// The importer argument is a package-path + filename (e.g.
// "github.com/Khan/webapp/services/myservice/somefile.go"); importee is just
// the package-path.  The error message need not mention the specific paths;
// those will be added by the caller.
func _checkImport(importer, importee string) (errorMessage string) {
	importerArea, importerSubArea, importerRest := _webappArea(importer)
	importeeArea, importeeSubArea, importeeRest := _webappArea(importee)

	// If we are in pkg (and not a test), no importing from services.
	if importerArea == "pkg" && importeeArea == "services" {
		return "pkg may not import from services"
	}

	// Services may not import from other services.
	if importerArea == "services" && importeeArea == "services" &&
		importerSubArea != importeeSubArea {
		return "services may not import from other services"
	}

	// The graphql package in a service may only be imported by resolvers
	if importerArea == "services" && importeeArea == "services" &&
		strings.HasPrefix(importeeRest, "generated/graphql") &&
		!strings.HasPrefix(importerRest, "resolvers/") &&
		// The content service is currently doing a bunch of type-reusing,
		// which violates these rules.
		// TODO(benkraft): Figure out if we want to change that.
		importerSubArea != "content" &&
		// The main.go file is also allowed to import graphql (to get at
		// `graphql.NewExecutableSchema` and such).
		importerRest != "cmd/serve/main.go" {
		return "only the resolvers package may import from generated/graphql (see ADR-312)"
	}

	// This is for tests, see dev/linters/import_lint_test.go for context.
	if importer == "lintfixtures/import/badimporter/badimporter.go" &&
		importee == "lintfixtures/import/badimportee" {
		return "bad import for this linter's own tests (if you're " +
			"seeing this elsewhere, something has gone very wrong)"
	}

	return ""
}

func _runImportLint(pass *analysis.Pass) (interface{}, error) {
	// pass.Pkg.Imports() has all the imports, but we want to know which files
	// they come from so we can report the right place.  So we walk the files;
	// it's not much harder.  This also lets us know if we're in a test.
	for _, file := range pass.Files {
		for _, spec := range file.Imports {
			_, filename := filepath.Split(pass.Fset.File(file.Pos()).Name())
			importee, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				// should never happen, since go/analysis should only give us
				// valid packages
				return nil, fmt.Errorf("invalid import rawpath: %+v %w", spec.Path.Value, err)
			}

			msg := _checkImport(path.Join(pass.Pkg.Path(), filename), importee)
			if msg != "" {
				pass.Reportf(spec.Pos(),
					"%v may not import from %v: %v",
					pass.Pkg.Path(), importee, msg)
			}
		}
	}

	return nil, nil
}
