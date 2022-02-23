package linters

// This file contains a generic linter to allow banning certain symbols.
//
// We already have depguard, an open-source linter that can ban entire
// packages, but sometimes we want to ban just a single symbol (function, type,
// const, etc.) while allowing the rest of the package.  This linter does that!
//
// TODO(benkraft): See if we can open-source this, such as by adding it to
// depguard or making a similarly-configurable linter.

import (
	"fmt"
	"go/types"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

// BannedSymbolAnalyzer bans certain symbols -- see _bannedSymbols below.
var BannedSymbolAnalyzer = &analysis.Analyzer{
	Name: "banned_symbol",
	Doc:  "bans certain symbols that we don't want to use",
	Run:  _run,
}

type (
	_filenameFilter func(filename string) bool
	_objectFilter   func(symbolObject types.Object) bool
)

// _testFiles is a filename-filter that matches all non-test files.
var _testFiles _filenameFilter = func(filename string) bool {
	return strings.HasSuffix(filename, "_test.go")
}

// _scriptFiles is a filename-filter that matches all non-executable files.
//
// This is a heuristic; we assume scripts are things that live in cmd/, plus a
// few known special cases.
var _scriptFiles _filenameFilter = func(filename string) bool {
	return (strings.Contains(filename, "/cmd/") ||
		// library for script-only usage (and uses stdout a bunch)
		strings.Contains(filename, "/services/districts/colors/"))
}

func _not(filter _filenameFilter) _filenameFilter {
	return func(filename string) bool {
		return !filter(filename)
	}
}

func _service(name string) _filenameFilter {
	return func(filename string) bool {
		// TODO(benkraft): Use ka-root-relative paths to make this more robust.
		return strings.Contains(filename, fmt.Sprintf("/services/%s/", name))
	}
}

func _pkg(name string) _filenameFilter {
	return func(filename string) bool {
		// TODO(benkraft): Use ka-root-relative paths to make this more robust.
		return strings.Contains(filename, fmt.Sprintf("/pkg/%s/", name))
	}
}

func _dev(name string) _filenameFilter {
	return func(filename string) bool {
		// TODO(benkraft): Use ka-root-relative paths to make this more robust.
		return strings.Contains(filename, fmt.Sprintf("/dev/%s/", name))
	}
}

// _subdirectoryOf matches any path where one of the directories has the given
// name
//
// For example, _subdirectoryOf("models") matches
// "services/fruit/models/apple/apple.go", but not
// "modelsandstuff/models.go".
func _subdirectoryOf(directory string) _filenameFilter {
	return func(filename string) bool {
		for _, part := range strings.Split(filename, string(filepath.Separator)) {
			if part == directory {
				return true
			}
		}
		return false
	}
}

// _allOf is a filename-filter that matches only if all the given filters
// match.
func _allOf(filters ..._filenameFilter) _filenameFilter {
	return func(filename string) bool {
		for _, filter := range filters {
			if !filter(filename) {
				return false
			}
		}
		return true
	}
}

// _anyOf is a filename-filter that matches only if any the given filters
// match.
func _anyOf(filters ..._filenameFilter) _filenameFilter {
	return func(filename string) bool {
		for _, filter := range filters {
			if filter(filename) {
				return true
			}
		}
		return false
	}
}

// This is the only object-filter we need right now.
func _isFunction(symbolObj types.Object) bool {
	_, ok := symbolObj.Type().Underlying().(*types.Signature)
	return ok
}

type _bannedSymbol struct {
	// name is the fully qualified name of the symbol to ban; see
	// lintutil.NameOf for accepted syntaxes and supported kinds.  Note that if
	// this is a type, we only prevent explicit reference to the type, not
	// reference to a value with that type.  For example, if a type T is
	// banned, one could still write functionReturningT(), just not T{...} or
	// func(t T).
	name string
	// nameRegexp is an alternative to name -- you can specify one or
	// the other but not both.  It is useful when name isn't fixed.
	nameRegexp *regexp.Regexp
	// message is the full error message we will report, like
	// like "Don't use fmt.Errorf, use pkg/lib/errors instead." or
	// "Using universe.Destroy is dangerous, proceed with care.".
	message string
	// filenameFilter is a filename-filter which must match before this lint
	// error will be reported.  If nil, errors will be reported in all files
	// (equivalent to a filter that always returns true).
	filenameFilter _filenameFilter
	// objectFilter is a filter which must match the symbol-use before
	// this lint error will be reported.  It can do any checking on
	// the symbol (as a types.Object object) that it wants.  If nil, we
	// won't do any object-checking on the symbol.
	objectFilter _objectFilter
}

// The list of symbols to ban.
//
// NOTE(benkraft): If you want to ban an entire package, use depguard --
// configured in .golangci.yml.
//
// TODO(benkraft): If this list gets long, we should make it a map for fast
// lookups.
var _bannedSymbols = []_bannedSymbol{
	{
		name:           "os.Stdout",
		message:        "Don't emit to stdout in server code!",
		filenameFilter: _allOf(_subdirectoryOf("services"), _not(_scriptFiles)),
	},
	{
		// chdir is disallowed because it could confuse code like
		// pkg/lib.KARoot, which looks at the current directory.  If we need to
		// make exceptions for scripts that might be okay, but we haven't
		// needed it yet and hey, it's global, it doesn't seem like a great
		// idea to begin with.
		name:    "os.Chdir",
		message: "Don't change directories inside Go code!",
	},
	{
		name:           "builtin.print",
		message:        "Don't print to stdout in server code!",
		filenameFilter: _allOf(_subdirectoryOf("services"), _not(_scriptFiles)),
	},
	{
		name:           "builtin.println",
		message:        "Don't print to stdout in server code!",
		filenameFilter: _allOf(_subdirectoryOf("services"), _not(_scriptFiles)),
	},
	{
		name:           "fmt.Print",
		message:        "Don't print to stdout in server code!",
		filenameFilter: _allOf(_subdirectoryOf("services"), _not(_scriptFiles)),
	},
	{
		name:           "fmt.Printf",
		message:        "Don't print to stdout in server code!",
		filenameFilter: _allOf(_subdirectoryOf("services"), _not(_scriptFiles)),
	},
	{
		name:           "fmt.Println",
		message:        "Don't print to stdout in server code!",
		filenameFilter: _allOf(_subdirectoryOf("services"), _not(_scriptFiles)),
	},
	{
		name: "fmt.Errorf",
		message: "Don't use fmt.Errorf, use functions " +
			"in pkg/lib/errors to create errors instead.",
		// We use fmt.Errorf in tests to mock third-party errors
		// (which would not be created using pkg/lib/errors).
		filenameFilter: _not(_testFiles),
	},
	{
		name:    "time.Now",
		message: "Don't use time.Now, use ctx.Time().Now() instead.",
	},
	{
		name:    "time.Since",
		message: "Don't use time.Since, use ctx.Time().Since() instead.",
	},
	{
		// We match on the actual Datastore() call. In resolvers it's okay to
		// pass datastore through; in models it's in principle not, but we do
		// sometimes need to (e.g. for encryption) and the main thing we want
		// to avoid is explicit calls.
		name:    "(github.com/Khan/webapp/pkg/gcloud/datastore.KAContext).Datastore",
		message: "Don't use datastore in the models or resolvers package (see ADR-312).",
		filenameFilter: _allOf(
			_anyOf(_subdirectoryOf("models"), _subdirectoryOf("resolvers")),
			_not(_testFiles)),
	},
	{
		name: "github.com/Khan/webapp/pkg/gcloud/datastore.Transaction",
		message: "Don't use datastore transactions in the " +
			"models or resolvers package (see ADR-312).",
		filenameFilter: _allOf(
			_anyOf(_subdirectoryOf("models"), _subdirectoryOf("resolvers")),
			_not(_testFiles)),
	},
	{
		name:    "net/http.DefaultClient",
		message: "Don't use http.DefaultClient, use ctx.HTTP() instead.",
	},
	{
		// os.Setenv is scary because it affects everyone in this process (and
		// any future child processes).  In tests (which run in serial), we use
		// suite.Setenv to ensure things get cleaned up at end of tests.  In
		// prod, envvars should generally be set when starting the process
		// (i.e. in the toplevel script) and not later.
		name:    "os.Setenv",
		message: "In tests, instead of os.Setenv, use suite.Setenv",
		// We make an exception for suite.Setenv's own tests.
		filenameFilter: _allOf(_testFiles, _not(_dev("khantest"))),
	},
	{
		name:    "os.Setenv",
		message: "Envvars should only be set in toplevel commands",
		// We make an exception for suite.Setenv itself.
		filenameFilter: _not(_anyOf(_scriptFiles, _dev("khantest"))),
	},
	// See also init() below, which adds to this list!
}

// In addition to requiring you use ctx.HTTP(), we want to make you put a
// context on the request.  The easiest way to do this is to ban most of
// the shorthands like http.Get (sad, but they don't support context, so we
// can't really avoid it) and require you use http.NewRequestWithContext
// instead of using http.NewRequest and adding a context later.
// We construct these rules dynamically because there are quite a few of them.
// We don't care so much in tests, but we still want to avoid the methods that
// use the default client (which could, you know, talk to prod!
var _badHTTPFunctions = []string{"Get", "Head", "Post", "PostForm"}

func init() {
	httpMessageTemplate := "Don't use %s.%s (it doesn't accept context), " +
		"use ctx.HTTP().Do() and http.NewRequestWithContext() instead."
	for _, method := range _badHTTPFunctions {
		_bannedSymbols = append(_bannedSymbols, _bannedSymbol{
			name:    fmt.Sprintf("net/http.%s", method),
			message: fmt.Sprintf(httpMessageTemplate, "http", method),
		}, _bannedSymbol{
			name:           fmt.Sprintf("(*net/http.Client).%s", method),
			message:        fmt.Sprintf(httpMessageTemplate, "http.Client", method),
			filenameFilter: _not(_testFiles),
		})
	}
	_bannedSymbols = append(_bannedSymbols, _bannedSymbol{
		name:           "net/http.NewRequest",
		message:        fmt.Sprintf(httpMessageTemplate, "http", "NewRequest"),
		filenameFilter: _not(_testFiles),
	})

	// The old shurcooL gqlclient library is deprecated.  We only allow
	// its use in tests, which we haven't cleaned up to use genqlient yet.
	gqlclientExceptions := []_filenameFilter{
		// - we still use gqlclient in tests, until ADR #461 is implemented.
		_testFiles,
		// - this is where the graphqlFunctions are defined.
		_pkg("web/gqlclient"),
	}
	// You're allowed to use gqlclient.KAContext itself anywhere, to pass it
	// around, but you're limited in wher you can use its methods.
	// We re-use the lists of GraphQL methods from graphql_lint.go, since it
	// has a test that that list is complete.
	for name := range graphqlFunctions { // map by function-name
		_bannedSymbols = append(_bannedSymbols, _bannedSymbol{
			name:           name,
			message:        "Use genqlient for cross-service calls, not gqlclient.",
			filenameFilter: _not(_anyOf(gqlclientExceptions...)),
		})
	}

	// As a result, you also don't need to use the shurcooL graphql
	// type wrappers anymore.
	for _, name := range []string{"Boolean", "Float", "Int", "String", "ID"} {
		_bannedSymbols = append(_bannedSymbols, _bannedSymbol{
			name:           "github.com/Khan/webapp/pkg/web/gqlclient." + name,
			message:        "You don't need gqlclient wrappers for genqlient or GraphQLTask.",
			filenameFilter: _not(_anyOf(gqlclientExceptions...)),
		})
	}

	// In services, we only allow using genqlient from the cross_services
	// directory, with a few exceptions.  Here "using" means calling the
	// functions that genqlient auto-generates; non-cross-service code
	// is still allowed to access genqlient's types and enums.
	genqlientExceptions := []_filenameFilter{
		// - cross_service (that's the point)
		_subdirectoryOf("cross_service"),
		// - this is where the genqlientFunctions are defined.
		_pkg("web/gqlclient"),
		// - pkg isn't a service, so a cross_service dir doesn't make sense.
		//   Note: once ADR-410 is implemented we can make this pkg/khan.
		_subdirectoryOf("pkg"),
		// - rest-gateway is *just* cross-service calls, so it doesn't
		//   make sense for it to have a cross_service/ directory.
		_service("rest-gateway"),
		// - scripts
		_scriptFiles,
	}
	// As above, we take the list of genqlient methods from graphql_lint.go.
	_bannedSymbols = append(_bannedSymbols, _bannedSymbol{
		nameRegexp:     regexp.MustCompile(`^github\.com/Khan/webapp/.*/generated/genqlient\..*`),
		message:        "Don't make genqlient queries outside of the cross_service directory.",
		filenameFilter: _not(_anyOf(genqlientExceptions...)),
		objectFilter:   _isFunction,
	})

	// In services, we only allow using gqlclient.Mux from the cross_services
	// directory, with a few exceptions.
	muxExceptions := []_filenameFilter{
		// - cross_service (that's the point)
		_subdirectoryOf("cross_service"),
		// - this is where the mux methods are defined.
		_pkg("web/gqlclient"),
		// - pkg isn't a service, so a cross_service dir doesn't make sense.
		//   Note: once ADR-410 is implemented we can make this pkg/khan.
		_subdirectoryOf("pkg"),
		// - rest-gateway is *just* cross-service calls, so it doesn't
		//   make sense for it to have a cross_service/ directory.
		_service("rest-gateway"),
	}

	// We also require that mocks be defined in the cross_service directory,
	// rather than in individual tests.  (With the same exceptions.)
	muxMethods := []string{
		"(*github.com/Khan/webapp/pkg/web/gqlclient.Mux).HandleOperation",
		"(*github.com/Khan/webapp/pkg/web/gqlclient.Mux).HandleOperationWithVars",
		"(*github.com/Khan/webapp/pkg/web/gqlclient.Mux).MatchOperation",
		"(*github.com/Khan/webapp/pkg/web/gqlclient.Mux).MatchOperationWithVarsa",
	}
	for _, name := range muxMethods {
		_bannedSymbols = append(_bannedSymbols, _bannedSymbol{
			name: name,
			message: "Put GraphQL mocks in the cross_service directory " +
				"(in *_mocks.go, parallel to the queries).",
			filenameFilter: _not(_anyOf(muxExceptions...)),
		})
	}
}

func _run(pass *analysis.Pass) (interface{}, error) {
	for use, obj := range pass.TypesInfo.Uses {
		filename := pass.Fset.File(use.Pos()).Name()
		name := lintutil.NameOf(obj)
		for _, banned := range _bannedSymbols {
			if banned.name != "" && name != banned.name {
				continue
			}
			if banned.nameRegexp != nil && !banned.nameRegexp.MatchString(name) {
				continue
			}
			if banned.filenameFilter != nil && !banned.filenameFilter(filename) {
				continue
			}
			if banned.objectFilter != nil && !banned.objectFilter(obj) {
				continue
			}
			pass.Reportf(use.Pos(), banned.message)
		}
	}
	return nil, nil
}
