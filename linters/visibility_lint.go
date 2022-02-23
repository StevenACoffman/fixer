package linters

// This file contains lint checks related to file-private symbols.

import (
	"fmt"
	"strings"

	"golang.org/x/tools/go/analysis"
)

var VisibilityAnalyzer = &analysis.Analyzer{
	Name: "visibility",
	Doc:  "check that symbols starting with _ are only used in the same file",
	Run:  _runVisibility,
}

// TODO(benkraft): Also ban references to underscore-prefixed struct-fields or
// receiver-methods not defined in this file.
func _runVisibility(pass *analysis.Pass) (interface{}, error) {
	// We run on a whole package at a time -- but we only care about each file
	// separately.
	for _, file := range pass.Files {
		filename := pass.Fset.File(file.Pos()).Name()
		if strings.HasSuffix(filename, "_test.go") {
			// We allow tests to break the rules.
			continue
		}
		// file.Unresolved is the toplevel identifiers that could not be
		// resolved entirely within this file -- i.e. those that reference
		// other files in this package, as well as imports in this file.
		for _, ident := range file.Unresolved {
			// TODO(benkraft): Should we allow imports of names starting with
			// underscore?  If so, we want to check this against file.Imports.
			// For now we forbid that.
			if ident.Name[0] == '_' {
				pass.Reportf(ident.Pos(),
					fmt.Sprintf("cannot refer to file-private %v", ident.Name))
			}
		}
	}
	// The first return value here is available if we want to pass data to
	// other analyzers.  We don't use it at this time.
	return nil, nil
}
