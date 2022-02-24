package linters

// This file contains lint checks that we have the desired documentation.

import (
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// DocumentationAnalyzer runs lint checks that we have the desired
// documentation.
var DocumentationAnalyzer = &analysis.Analyzer{
	Name: "documentation",
	Doc:  "ensures all files and packages are documented",
	Run:  _runDocumentation,
}

// Standard comment to mark generated code, per
// https://golang.org/pkg/cmd/go/internal/generate/#pkg-variables
var _generatedCommentRegexp = regexp.MustCompile(
	`^// Code generated .* DO NOT EDIT\.$`)

// This matches only "contentful" docstring lines, and not ones that
// Go interprets as docstrings but are actually build directives.
// (It properly excludes lines like `//nolint:foo` but not lines
// like `// +build !darwin`.)
// TODO(csilvers): allow leading `+` if it's not followed by `build`.
// TODO(csilvers): it doesn't always exclude directive lines properly,
// e.g. `//nolint:foo // bar`.
var _contentfulDocstringRegexp = regexp.MustCompile(
	`(?m)^[^+]`)

// _isGeneratedComment returns true if the given comment is a "Code generated
// by ... DO NOT EDIT." comment.
func _isGeneratedComment(comment *ast.CommentGroup) bool {
	if comment == nil {
		return false
	}
	for _, singleComment := range comment.List {
		if _generatedCommentRegexp.MatchString(singleComment.Text) {
			return true
		}
	}

	return false
}

// _isEntirelyGenerated returns true if the files in this pass are all
// generated, and should not be linted.
//
// In principle, it's not really our job to ignore such code.  But `go test`
// generates some files (never seen by the user) which, unfortunately, end up
// getting linted in our tests.  We need to make sure we don't complain about
// them.
func _isEntirelyGenerated(pass *analysis.Pass) bool {
	for _, file := range pass.Files {
		isGenerated := _isGeneratedComment(file.Doc)
		for _, comment := range file.Comments {
			isGenerated = isGenerated || _isGeneratedComment(comment)
		}
		if !isGenerated {
			return false
		}
	}

	return true
}

// _hasPackageDoc returns whether this file has a package-doc that does
// not consist entirely of machine directives.
func _hasPackageDoc(file *ast.File) bool {
	return file.Doc != nil && _contentfulDocstringRegexp.MatchString(file.Doc.Text())
}

// _hasFileDoc returns whether this file has a file-doc.
func _hasFileDoc(file *ast.File, fset *token.FileSet) bool {
	if len(file.Comments) == 0 {
		return false
	}

	// A file-comment is a comment before the first line of code (which
	// could be an import, or a declaration if this file has no imports).
	// We don't count a comment on the same line as the package declaration.
	firstCommentPos := file.Comments[0].Pos()
	if fset.Position(firstCommentPos).Line <= fset.Position(file.Package).Line {
		return false
	}

	switch {
	case len(file.Imports) > 0:
		return firstCommentPos < file.Imports[0].Pos()
	case len(file.Decls) > 0:
		return firstCommentPos < file.Decls[0].Pos()
	default:
		// If there's nothing else in the file, anything after the package
		// clause (which we already must be) is ok.
		return true
	}
}

type _byFilename struct {
	fset  *token.FileSet
	files []*ast.File
}

func (bf _byFilename) Len() int      { return len(bf.files) }
func (bf _byFilename) Swap(i, j int) { bf.files[i], bf.files[j] = bf.files[j], bf.files[i] }
func (bf _byFilename) Less(i, j int) bool {
	return bf.fset.File(bf.files[i].Pos()).Name() < bf.fset.File(bf.files[j].Pos()).Name()
}

// _runDocumentation checks that:
// - each package has a package-doc
// - each file has a package-doc or a file-doc
func _runDocumentation(pass *analysis.Pass) (interface{}, error) {
	if _isEntirelyGenerated(pass) {
		return nil, nil
	}

	// Grab just the non-test files.  (We don't require tests are documented.)
	var files []*ast.File
	for _, file := range pass.Files {
		filename := pass.Fset.File(file.Pos()).Name()
		if !strings.HasSuffix(filename, "_test.go") {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		// If you have tests in a separate package, no need to document that
		// package.  (In principle maybe if they aren't parallel to a non-test
		// file, they should, but we won't bother to require it.)
		return nil, nil
	}

	// Then sort by filename to make errors show up in a consistent place.
	sort.Sort(_byFilename{pass.Fset, files})

	foundPackageDoc := false
	var filesWithoutDoc []*ast.File
	for _, file := range files {
		switch {
		case _hasPackageDoc(file):
			foundPackageDoc = true // ideal!
		case _hasFileDoc(file, pass.Fset):
			// also good!
		default:
			filesWithoutDoc = append(filesWithoutDoc, file)
		}
	}

	if !foundPackageDoc {
		pass.Reportf(
			files[0].Pos(),
			"Missing package doc; add one above the package "+
				"declaration in any file")
		if len(filesWithoutDoc) > 0 && files[0] == filesWithoutDoc[0] {
			// If we would have also complained about a file-doc on this file,
			// don't bother.  (golangci-lint will only take the first message
			// anyway, and this one is more important.)
			filesWithoutDoc = filesWithoutDoc[1:]
		}
	}

	for _, file := range filesWithoutDoc {
		pass.Reportf(
			file.Pos(),
			"Missing package or file doc; put a file doc after the "+
				"package declaration but before the imports/code")
	}

	return nil, nil
}
