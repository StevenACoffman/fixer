// Package linters contains KAContext-related linters.  See each linter for
// details.
package linters

// This file defines the linter for general use of context.

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var KAContextAnalyzer = &analysis.Analyzer{
	Name: "kacontext",
	Doc: "enforces rules relating to go- and KA-context, such as " +
		"that Context is always the first parameter and is named ctx.",
	Run: _runContext,
}

// isContextType returns true if the input is a context-type (either Go-style
// context.Context or a KA-context style interface embedding it).
//
// TODO(benkraft): Cache this: while the toplevel call will likely not be
// repeated too much, the recursive calls isContextType(kacontext.Base) and
// suchlike will be.  Luckily, we don't have many super-deep nested interfaces.
// Sadly, we can't use cache.Cache until INFRA-4215 is fixed.
func isContextType(typ types.Type) bool {
	if lintutil.TypeIs(typ, "context", "Context") {
		return true
	}
	iface, ok := typ.Underlying().(*types.Interface)
	if !ok {
		return false
	}
	for i := 0; i < iface.NumEmbeddeds(); i++ {
		if isContextType(iface.EmbeddedType(i)) {
			return true
		}
	}

	return false
}

// _badCtxOkWithin returns true if it's ok to use context.Background()/nil ctx
// inside this node because it's init or main and there may not be a meaningful
// one.
// TODO(benkraft): Should we settle on one or the other?
// TODO(benkraft): Also include context.Background() in function calls in
// toplevel var statements (which are executed as init blocks)
func _badCtxOkWithin(node ast.Node) bool {
	funcDecl, ok := node.(*ast.FuncDecl)

	return ok && funcDecl.Recv == nil &&
		(funcDecl.Name.Name == "init" || funcDecl.Name.Name == "main")
}

// _lintContextBackground lints for context.Background() calls.
func _lintContextBackground(
	report func(analysis.Diagnostic),
	file *ast.File,
	typesInfo *types.Info,
) {
	// TODO(benkraft): If we end up with a ton of Inspect-based analyzers,
	// use x/tools/go/analysis/passes/inspect to make them faster.
	// TODO(benkraft): Move this into banned-symbol linter -- would need to
	// add support for allowing it in init/main.
	ast.Inspect(file, func(node ast.Node) bool {
		name := lintutil.NameOf(lintutil.ObjectFor(node, typesInfo))
		if name == "context.Background" {
			report(analysis.Diagnostic{
				Pos:     node.Pos(),
				Message: "do not use context.Background() outside tests",
			})
			// The children of the node representing context.Background aren't
			// independently interesting, so we don't traverse them.  (This
			// also avoids duplicate errors, since the "Background" also ends
			// up referring to context.Background.
			return false
		}

		return !_badCtxOkWithin(node) // don't traverse init/main
	})
}

// handler functions that receive a `*http.Request` param are allowed
// to upgrade context.
func _hasHTTPRequestParam(fields []*ast.Field) bool {
	for _, param := range fields {
		sid, ok := param.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := sid.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if sel.Sel.Name != "Request" {
			continue
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" {
			return true
		}
	}

	return false
}

func _isPreSaveMethod(funcDecl *ast.FuncDecl, typesInfo *types.Info) bool {
	// TODO(jared): We could verify that the receiver inherits from
	// datastore.BaseModel if we want to...
	if funcDecl.Recv == nil {
		return false
	}
	if funcDecl.Name.Name != "PreSave" {
		return false
	}
	if len(funcDecl.Type.Results.List) != 1 {
		return false
	}
	if res, ok := funcDecl.Type.Results.List[0].Type.(*ast.Ident); !ok || res.Name != "error" {
		return false
	}
	// ctx context.Context should be the only argument
	if len(funcDecl.Type.Params.List) != 1 {
		return false
	}
	firstArg := funcDecl.Type.Params.List[0]

	return lintutil.TypeIs(typesInfo.TypeOf(firstArg.Type), "context", "Context")
}

func _isDataLoader(funcDecl *ast.FuncDecl, typesInfo *types.Info) bool {
	if funcDecl.Type.Results == nil || len(funcDecl.Type.Results.List) != 1 {
		return false
	}
	slice, ok := typesInfo.TypeOf(funcDecl.Type.Results.List[0].Type).(*types.Slice)
	if !ok {
		return false
	}
	ptr, ok := slice.Elem().(*types.Pointer)
	if !ok {
		return false
	}

	return lintutil.TypeIs(ptr.Elem(), "github.com/graph-gophers/dataloader", "Result")
}

// _lintContextUpgrade lints against using kacontext.Upgrade to create new
// variables outside of a few specific contexts.
func _lintContextUpgrade(
	report func(analysis.Diagnostic),
	file *ast.File,
	typesInfo *types.Info,
) {
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		isAllowedToUpgrade := (_isMain(funcDecl) ||
			lintutil.IsResolverFunc(funcDecl, typesInfo) ||
			_hasHTTPRequestParam(funcDecl.Type.Params.List) ||
			_isPreSaveMethod(funcDecl, typesInfo) ||
			_isDataLoader(funcDecl, typesInfo))
		if !isAllowedToUpgrade {
			_forbidContextUpgrade(report, decl, typesInfo)
		}
	}
}

func _isMain(funcDecl *ast.FuncDecl) bool {
	return funcDecl.Name.Name == "main"
}

func _forbidContextUpgrade(
	report func(analysis.Diagnostic),
	node ast.Node,
	typesInfo *types.Info,
) {
	allowedUses := make(map[ast.Node]bool)
	ast.Inspect(node, func(node ast.Node) bool {
		stmt, ok := node.(*ast.AssignStmt)
		if !ok || len(stmt.Lhs) != 1 {
			return true
		}
		// redefining, no chance to change/add interfaces
		// for example:
		// 	   g, gCtx := errgroup.WithContext(ctx)
		//     ctx = kacontext.Upgrade(gCtx)
		if stmt.Tok.String() == "=" {
			val := stmt.Rhs[0]
			t := typesInfo.TypeOf(val)
			if _isKAContextEverythingType(t) {
				allowedUses[val] = true
			}
		}

		return true
	})

	ast.Inspect(node, func(node ast.Node) bool {
		if lit, ok := node.(*ast.FuncLit); ok {
			// Nested function literals that take *http.Request as an argument
			// are allowed to do kacontext.Upgade, so don't recurse into them.
			if _hasHTTPRequestParam(lit.Type.Params.List) {
				return false
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		// serve.Init(func (ctx kacontext.Base) {}) function literals are
		// allowed to call kacontext.Upgrade
		if id, ok := call.Fun.(*ast.Ident); ok {
			if lintutil.NameOf(lintutil.ObjectFor(id, typesInfo)) ==
				"github.com/Khan/pkg/web/serve.Init" {
				// serve.Init function arguments are allowed to use
				// kacontext.Upgrade, so don't recurse into them.
				return false
			}
		}

		t := typesInfo.TypeOf(call)
		if _isKAContextEverythingType(t) {
			if !allowedUses[node] {
				report(analysis.Diagnostic{
					Pos: node.Pos(),
					Message: "Producing the 'kaContext' type (such as with kacontext.Upgrade) " +
						"is not allowed in this function. See https://khanacademy.atlassian.net" +
						"/l/c/NE7ith83#Allowed-usages-of-kacontext.Upgrade",
				})
			}
		}

		return true
	})
}

// _lintContextParameter lints for incorrect context parameters.
func _lintContextParameter(
	report func(analysis.Diagnostic),
	file *ast.File,
	typesInfo *types.Info,
) {
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		for i, param := range funcDecl.Type.Params.List {
			if !isContextType(typesInfo.TypeOf(param.Type)) {
				continue
			}
			if i != 0 || len(param.Names) > 1 {
				report(analysis.Diagnostic{
					Pos:     param.Pos(),
					Message: "Context should be the first parameter",
				})
			}
			// this happens with a function with an un named paramater like:
			// func foo(context.Context) {}
			if len(param.Names) == 0 {
				continue
			}
			name := param.Names[0].Name
			if name != "ctx" && name != "_" {
				// This duplicates the check in golint, but we may as well
				// do it here too.
				report(analysis.Diagnostic{
					Pos:     param.Pos(),
					Message: "Context parameter should be called 'ctx'",
				})
			}
		}
	}
}

// _lintNilContext lints for passing nil to a function expecting a context.
func _lintNilContext(
	report func(analysis.Diagnostic),
	file *ast.File,
	typesInfo *types.Info,
) {
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return !_badCtxOkWithin(node) // don't traverse init/main
		}

		funcType, ok := typesInfo.Types[call.Fun].Type.(*types.Signature)
		if !ok { // impossible, I think?
			return true
		}
		for i, arg := range call.Args {
			if !typesInfo.Types[arg].IsNil() {
				continue
			}
			if isContextType(getParamAt(funcType, i).Type()) {
				report(analysis.Diagnostic{
					Pos:     arg.Pos(),
					Message: "do not pass nil context",
				})
			}
		}

		return true // recurse (e.g. on arguments)
	})
}

const kacontextPath = "github.com/Khan/webapp/pkg/kacontext"

// _isKAContextEverythingType returns true if the given type is
// kacontext.kaContext or *kacontext.kaContext, which is the "everything-type"
// that gives access to all the kacontext methods without restriction.
func _isKAContextEverythingType(typ types.Type) bool {
	pointer, isPointer := typ.(*types.Pointer)

	return lintutil.TypeIs(typ, kacontextPath, "kaContext") ||
		(isPointer && lintutil.TypeIs(pointer.Elem(), kacontextPath, "kaContext"))
}

// _lintContextVars lints that if you put a kacontext in a variable, you give
// it an explicit type.
//
// The idea is: there's no way to write a function that explicitly references
// pkg/kacontext.kaContext.  But you can still get such a value, a la
//	ktx := kacontext.Upgrade(ctx)
// We don't want none of that!  You gotta do
//	var ktx <some interface> = kacontext.Upgrade(ctx)
func _lintContextVars(
	report func(analysis.Diagnostic),
	file *ast.File,
	typesInfo *types.Info,
) {
	ast.Inspect(file, func(node ast.Node) bool {
		// We look for the place where you define a variable with the
		// everything-type.  (This is easy to do from typesInfo.Defs; and it's
		// the best place to report the error.) This means we miss the case
		// where you don't assign the value a name at all, such as returning it
		// or passing it directly to a function, but that's ok because the type
		// will be clear from what you did with it (the current or callee
		// function's type, respectively).
		// TODO(benkraft): Make an exception in the linter for code that calls
		// NewBlank(), Clone(), or similar.  Most such code is in tests, but
		// not all!
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true // always recurse -- we check this even in init/main!
		}

		obj := typesInfo.Defs[ident]
		if obj != nil && _isKAContextEverythingType(obj.Type()) {
			msg := fmt.Sprintf(
				`%s implicitly has the kacontext "everything-type"; give `+
					"it an explicit type by doing `var %s <type> = ...` or "+
					"see the documentation of kacontext.Upgrade for details",
				ident.Name, ident.Name)
			report(analysis.Diagnostic{
				Pos:     ident.Pos(),
				Message: msg,
			})
		}

		return true
	})
}

func _runContext(pass *analysis.Pass) (interface{}, error) {
	for _, file := range pass.Files {
		_lintContextParameter(pass.Report, file, pass.TypesInfo)
		filename := pass.Fset.File(file.Pos()).Name()
		if strings.HasSuffix(filename, "_test.go") {
			// We allow tests to use context.Background().
			continue
		}
		if strings.Contains(filename, "dev/linters/") {
			// We also allow linters.
			continue
		}
		if !strings.HasSuffix(filename, "/main.go") {
			_lintContextUpgrade(pass.Report, file, pass.TypesInfo)
		}

		_lintContextBackground(pass.Report, file, pass.TypesInfo)
		_lintNilContext(pass.Report, file, pass.TypesInfo)
		_lintContextVars(pass.Report, file, pass.TypesInfo)
	}

	return nil, nil
}
