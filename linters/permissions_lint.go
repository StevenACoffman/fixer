package linters

// This file contains lint checks that we have permissions checks in each of
// our GraphQL resolvers.  See PermissionsAnalyzer for details.

import (
	"go/ast"
	"path/filepath"
	"reflect"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// PermissionsAnalyzer checks that GraphQL resolvers have a permissions check.
//
// In ADR-211 [1], we decided that resolvers are responsible for checking
// permissions in services.  An important part of the value of this approach is
// that we can lint for it: this is the linter that does so.  In particular, it
// requires that every resolver calls some function which is identified as a
// permission-checker.
//
// Permission-checker functions are identified by a special directive.  To mark
// a permission-checker function, add a comment
//  //ka:permission-check (optional further commentary for humans)
// on the line before the function (i.e. in its doc-comment).  Then any call to
// that function will count as a permission check.
//
// Note the directive has no space, which is the standard for machine-readable
// comments.  In Go 1.15+, such comments will automatically be omitted from
// godoc output.  Note also that for functions called via an interface type
// (e.g. KAContext methods), both the function-definition and the
// interface-method should have the directive, to make sure either way of
// calling the method gets handled correctly.
//
// A few services due to their purpose rarely need permission checks; we
// exclude those from this linter in .golangci.yml.  At present this is just
// the content service.
//
// [1] https://docs.google.com/document/d/16BglSM025Bu73G73BRGa3r8ZvDbF6a10-mKmtsAVb1U/edit#
var PermissionsAnalyzer = &analysis.Analyzer{
	Name: "permissions",
	Doc:  "requires GraphQL resolvers have a permissions check",
	Run:  _runPermissions,
	// This analyzer also returns the list of resolvers in this package which
	// may be used by other analyzers (see ResolverErrorAnalyzer for an
	// example).
	ResultType: reflect.TypeOf([]resolver(nil)),
	FactTypes:  []analysis.Fact{new(_isPermissionChecker)},
}

// Fact exported for a *types.Func when the function is a permission checker.
//
// See the docs for more about Facts:
// https://pkg.go.dev/golang.org/x/tools/go/analysis?tab=doc#hdr-Modular_analysis_with_Facts
type _isPermissionChecker struct{}

// AFact tells go/analysis that this is a valid fact type.
func (*_isPermissionChecker) AFact() {}

// String makes test-assertions work: we can say
//	func name(...) { // want name:"_isPermissionChecker"
// to assert that we mark that the given package is a permission checker.
func (*_isPermissionChecker) String() string { return "_isPermissionChecker" }

// _hasPermissionCheckDirective returns true if the given comment-block has a
// //ka:permission-check directive.
func _hasPermissionCheckDirective(comment *ast.CommentGroup) bool {
	if comment == nil {
		return false
	}
	// We look line-by-line: Go 1.15+ will filter directives out of
	// CommentGroup.Text(), and plus we only want to look at the start
	// of a line.
	for _, line := range comment.List {
		if strings.HasPrefix(line.Text, "//ka:permission-check") {
			return true
		}
	}
	return false
}

// _markPermissionCheckers finds the functions annotated with
// ka:permission-check, and exports them for use in our analyses of this and
// future packages.
func _markPermissionCheckers(pass *analysis.Pass) {
	_mark := func(name *ast.Ident) {
		obj := pass.TypesInfo.ObjectOf(name)
		if obj != nil {
			pass.ExportObjectFact(obj, new(_isPermissionChecker))
		}
	}

	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				// look for top-level functions/receivers
				if _hasPermissionCheckDirective(node.Doc) {
					_mark(node.Name)
				}
			case *ast.InterfaceType:
				// and for interface methods
				for _, method := range node.Methods.List {
					if _hasPermissionCheckDirective(method.Doc) {
						for _, name := range method.Names {
							_mark(name)
						}
					}
				}
			case *ast.GenDecl:
				// and top level variable declarations that may be cached
				// functions
				if _hasPermissionCheckDirective(node.Doc) {
					for _, spec := range node.Specs {
						valueSpec, ok := spec.(*ast.ValueSpec)
						if ok {
							for _, name := range valueSpec.Names {
								_mark(name)
							}
						}
					}
				}
			}
			return true
		})
	}
}

type resolver struct {
	f        *ast.FuncDecl
	typeName string
}

// _findResolvers returns the resolver functions in the given package whose
// names match the given filter.
//
// The names passed to the filter will be lowerCamelCase.  Currently they're
// just the name of the type, without "Resolver".
func _findResolvers(pass *analysis.Pass) []resolver {
	var retval []resolver
	for _, file := range pass.Files {
		// We look for methods of types *myTypeResolver in resolvers packages
		// and files.  Really, the right thing to do would be to look at the
		// toplevel resolver (referenced from <service>/cmd/serve) and find the
		// other resolvers by traversing from there, but that's more work.
		// For packages, we look for several names: ADR-312 says resolvers are
		// in a "resolvers" package, but some older services have a
		// "resolver.go" file.
		// For the types, we look for any type ending with "Resolver".  As an
		// exception, we do *not* check methods of the toplevel "Resolver"
		// type, because those don't have any interesting logic.
		// TODO(benkraft): Once everyone is consistently using a resolvers
		// package, simplify this.
		if pass.Pkg.Name() == "resolvers" ||
			filepath.Base(pass.Fset.File(file.Pos()).Name()) == "resolver.go" {
			for _, decl := range file.Decls {
				funcDecl, ok := decl.(*ast.FuncDecl)
				if !ok || !ast.IsExported(funcDecl.Name.Name) || funcDecl.Recv == nil {
					continue
				}

				recvType, ok := funcDecl.Recv.List[0].Type.(*ast.StarExpr)
				if !ok {
					continue
				}

				recv, ok := recvType.X.(*ast.Ident)
				if ok && recv.Name != "Resolver" &&
					strings.HasSuffix(recv.Name, "Resolver") {
					retval = append(retval, resolver{funcDecl, recv.Name})
				}
			}
		}
	}
	return retval
}

// _hasPermissionCheck returns true if the given function contains a
// permission-check.
func _hasPermissionCheck(pass *analysis.Pass, funcDecl *ast.FuncDecl) bool {
	foundCheck := false
	ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true // recurse
		}

		obj := pass.TypesInfo.ObjectOf(ident)
		if obj != nil && pass.ImportObjectFact(obj, new(_isPermissionChecker)) {
			foundCheck = true
		}

		return false // nothing to recurse on
	})
	return foundCheck
}

func _runPermissions(pass *analysis.Pass) (interface{}, error) {
	_markPermissionCheckers(pass)

	resolvers := _findResolvers(pass)
	for _, resolver := range resolvers {
		// Note that we prefer the permission check to be "first", but that's
		// not always possible to do, and hard to define even where it is.
		if !_hasPermissionCheck(pass, resolver.f) {
			pass.Reportf(resolver.f.Pos(), "%s resolver has no permission check: "+
				"add one, or see `go doc dev/linters.PermissionsAnalyzer` for "+
				"details on how to mark permission checks",
				resolver.f.Name.Name)
		}
	}

	// Return resolvers so other analyzers can use them.
	// TODO(benkraft): In principle maybe this should be done by a separate
	// analyzer whose job is to return resolvers, and read by this and others.
	return resolvers, nil
}
