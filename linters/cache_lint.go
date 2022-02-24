package linters

// This file contains static analysis of cache.Cache.  It checks that:
// - at most one cache.Cache is applied to each function (and it is always
//   applied to a function in the same package); this avoids collisions due to
//   the way we generate cache keys
// - the cachers come in the right order (fastest to slowest)

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var CacheAnalyzer = &analysis.Analyzer{
	Name: "cache",
	Doc:  "checks arguments to cache.Cache",
	Run:  _runCache,
}

// _checkFunction checks that the cached function is valid.  The main thing to
// check here is that you don't do
//	var cached = cache.Cache(cache.Cache(myFunction, ...), ...)
// nor
//  var cached1 = cache.Cache(myFunction, ...)
//  var cached2 = cache.Cache(myFunction, ...)
// either of which could cause cache-key collisions since we use the
// function-name in the key.
func _checkFunction(pass *analysis.Pass, cachedFn ast.Expr, cachedFns map[types.Object]ast.Expr) {
	if _, ok := cachedFn.(*ast.FuncLit); ok {
		// It's ok to cache a function-literal; nothing else can
		// possibly have a reference to it.
		return
	}

	fnObj := lintutil.ObjectFor(cachedFn, pass.TypesInfo)
	if fnObj == nil {
		// This prevents things like
		//	cache.Cache(cache.Cache(...), ...)
		// TODO(benkraft): There are several ways to get around this, be more
		// precise if it comes up.
		pass.Reportf(cachedFn.Pos(),
			"argument to cache.Cache must be a named function")

		return
	}
	if fnObj.Pkg() != pass.Pkg {
		// It's not strictly a problem to cache a function in another
		// package, but it makes it a lot harder to be sure that no one
		// else is doing so, so we just forbid it.
		pass.Reportf(cachedFn.Pos(),
			"don't cache functions in another package directly (define a wrapper)")

		return
	}

	if otherFn, ok := cachedFns[fnObj]; ok {
		// This would cause cache-key collisions.
		pass.Reportf(cachedFn.Pos(),
			"don't call cache.Cache on the same function twice "+
				"(other call at %v)", pass.Fset.Position(otherFn.Pos()))

		return
	}
	cachedFns[fnObj] = cachedFn
}

// _cachePriority is a map from cache-name to priority, where the fastest
// caches (the ones we should check first) have the highest priority, and all
// priorities are greater than zero.  Caches with equal priority may come in
// either order.
var _cachePriority = map[string]int{
	"github.com/Khan/webapp/pkg/lib.RequestCache":         2000,
	"github.com/Khan/webapp/pkg/lib.InstanceCache":        1000,
	"github.com/Khan/webapp/pkg/gcloud/memorystore.Cache": 200,
	"github.com/Khan/webapp/pkg/gcloud/datastore.Cache":   100,
	// TODO(benkraft): Add support for caches that aren't package-vars
	// (but instead you configure and pass in your own var), like
	// settings-cache and lru-cache.
}

// _checkCacheOrder checks that the caches come in the expected order, i.e.
// fastest to slowest.
func _checkCacheOrder(pass *analysis.Pass, options []ast.Expr) {
	nameOf := func(node ast.Node) string {
		return lintutil.NameOf(lintutil.ObjectFor(node, pass.TypesInfo))
	}

	var caches []ast.Expr
	for _, opt := range options {
		call, ok := opt.(*ast.CallExpr)
		if !ok || nameOf(call.Fun) != "github.com/Khan/webapp/pkg/lib/cache.In" {
			continue
		}

		if len(call.Args) != 1 { // (shouldn't happen in type-checked code)
			pass.Reportf(call.Pos(), "invalid call to cache.In (should have 1 argument)")

			return
		}

		caches = append(caches, call.Args[0])
	}

	priorities := make([]int, len(caches))
	for i, cache := range caches {
		// Caches we don't know about get 0 priority, which lets them go
		// anywhere.  (NameOf maps nil to "nil", so this works even if
		// ObjectFor can't find them, e.g. they're not a shared var.)
		priorities[i] = _cachePriority[nameOf(cache)]
	}

	var lastCache ast.Expr
	lastPriority := 0
	for i, priority := range priorities {
		if priority == 0 {
			continue
		}
		if lastPriority != 0 && priority > lastPriority {
			pass.Reportf(caches[i].Pos(), "%v should come before %v",
				// TODO(benkraft): Abbreviate the name.
				nameOf(caches[i]), nameOf(lastCache))
		}
		lastCache = caches[i]
		lastPriority = priority
	}
}

func _runCache(pass *analysis.Pass) (interface{}, error) {
	cachedFns := map[types.Object]ast.Expr{}
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			// Look for calls to cache.Cache
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true // recurse
			}

			cacheFnObj := lintutil.ObjectFor(call.Fun, pass.TypesInfo)
			if lintutil.NameOf(cacheFnObj) != "github.com/Khan/webapp/pkg/lib/cache.Cache" {
				return true
			}

			if len(call.Args) == 0 { // (shouldn't happen in type-checked code)
				pass.Reportf(call.Pos(), "invalid call to cache.Cache (no arguments)")

				return true
			}

			_checkFunction(pass, call.Args[0], cachedFns)
			_checkCacheOrder(pass, call.Args[1:])

			return true
		})
	}

	return nil, nil
}
