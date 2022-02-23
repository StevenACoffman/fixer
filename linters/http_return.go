package linters

// This file contains a linter requiring we return after a call to http.Error
// or http.Redirect.
//
// TODO(benkraft): Might be worth trying to open-source this one.

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/ctrlflow"
	"golang.org/x/tools/go/cfg"

	"github.com/StevenACoffman/fixer/lintutil"
)

// HTTPReturnAnalyzer says you have to return after http.Error or
// http.Redirect.
//
// As the documentation says: [http.Error] does not end the request; the caller
// should ensure no further writes are done to w.  We want to make sure we do
// that!  (Especially coming from python, it's easy to forget it doesn't panic
// or anything like that.)  In principle it would be okay to do other things --
// say some logging -- but in practice we don't, and "you have to return right
// away" is the easiest thing to lint for.
//
// In fact, we don't even let you do
//	if ... {
//		http.Error(...)
//	} else {
//		...
//	}
//	return
// which is partly to simplify the linter and partly because it's good to
// *obviously* return.
//
// TODO(benkraft): In principle we should also trace across functions, and
// ensure that if you call a function which calls http.Error, you also return
// after that.  But that's a bit more work, and not super common in practice.
var HTTPReturnAnalyzer = &analysis.Analyzer{
	Name:     "httpreturn",
	Doc:      "we should return after http.Error or similar",
	Run:      _runHTTPReturn,
	Requires: []*analysis.Analyzer{ctrlflow.Analyzer},
}

// _runHTTPReturn looks at the data exported by ctrlflow.Analyzer, and checks
// that any call to http.Error or similar is followed by a return.
//
// In particular, ctrlflow.Analyzer exports a control-flow graph (CFG) for each
// function; we call checkCFG on each.
func _runHTTPReturn(pass *analysis.Pass) (interface{}, error) {
	cfgs, _ := pass.ResultOf[ctrlflow.Analyzer].(*ctrlflow.CFGs)
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				_checkCFG(pass, cfgs.FuncDecl(node))
			case *ast.FuncLit:
				_checkCFG(pass, cfgs.FuncLit(node))
			}
			return true // always recurse
		})
	}
	return nil, nil
}

// _checkCFG checks that any calls to http.Error or similar within this
// particular function's control-flow graph are followed by a return.
func _checkCFG(pass *analysis.Pass, cfg *cfg.CFG) {
	// We iterate through blocks, which are sections of code which are executed
	// serially.
	for _, block := range cfg.Blocks {
		if !block.Live {
			// If the block is dead code, we can ignore it.
			continue
		}

		// We set mustReturn to be the node after which we must return.
		var mustReturn *ast.Ident
		report := func() {
			pass.Reportf(mustReturn.Pos(),
				"HTTP handlers (or helpers) must return after calling %v",
				lintutil.NameOf(lintutil.ObjectFor(mustReturn, pass.TypesInfo)))
		}

		// Iterate through the nodes; usually those are statements but they
		// might be expressions (e.g. the condition of an if)
		for _, node := range block.Nodes {
			// If we need to return after the preceding statement, but this is
			// not a return, complain.
			_, isReturn := node.(*ast.ReturnStmt)
			if mustReturn != nil && !isReturn {
				report()
			}

			// Otherwise, check whether we need to return after *this* stmt.
			mustReturn = _mustReturnAfter(pass, node)
		}

		// We're at the end of the block; if the last statement needed to
		// return, complain.  Note that the CFG builder puts in an explicit
		// return at the end of every function, so we don't need to treat that
		// case specially.
		if mustReturn != nil && len(block.Succs) > 0 {
			// TODO(benkraft): Technically we should allow successor-blocks
			// that begin with a return.  But as discussed in the
			// HTTPReturnAnalyzer, we'd prefer to *obviously* return.
			report()
		}
	}
}

// _mustReturnAfter returns an identifier denoting a call after which the we
// must return, if there is one inside the given node.
func _mustReturnAfter(pass *analysis.Pass, node ast.Node) *ast.Ident {
	var ret *ast.Ident
	ast.Inspect(node, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			switch lintutil.NameOf(lintutil.ObjectFor(node, pass.TypesInfo)) {
			case "net/http.Error", "net/http.Redirect":
				ret = node
			}
			return false // nowhere to recurse
		case *ast.FuncLit:
			return false // has its own CFG analyzed separately.
		default:
			return true // otherwise, recurse
		}
	})
	return ret
}
