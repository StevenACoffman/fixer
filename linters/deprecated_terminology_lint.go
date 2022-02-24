package linters

// This file contains a generic linter to prohibit certain terminology we've
// decided to deprecate; see DeprecatedTerminologyAnalyzer, below, for details.

import (
	"context"
	"fmt"
	"go/ast"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lib"
	"github.com/StevenACoffman/fixer/lintutil"
)

// DeprecatedTerminologyAnalyzer is the entrypoint to this linter (from
// golangci-lint or other go/analysis based tools).
//
// See https://khanacademy.org/r/renaming for more context on the deprecated
// terms we look for.
//
// Note that we only look for deprecated terms in filenames and identifiers.
// We could also look at strings and comments, but in practice many of those
// seem to be false positives; comments particularly have a lot of free-form
// text that ends up saying things like "we have language to say that...", and
// the main places we might care about strings are in GraphQL enum values and
// struct tags, both of which are places we typically don't want to make people
// fix up right away (because doing so requires backfills, GraphQL schema
// changes, or similar).
//
// We report at the definition-site, not use-sites, to cut down on duplicate
// errors, and on problems in clients of an API that uses a nolint to silence
// this linter.
//
// Note that that means we never report on identifiers defined in generated (or
// otherwise lint-ignored) code.  That's actually a feature, in practice: our
// generated code is (so far) based on our GraphQL APIs, which typically we do
// not want to rename (because it's too much work).  (We still have to nolint
// resolver methods, since those are defined in our code to match an interface
// in generated code.) Anyway, if in the future we do want to lint these, we
// can do so in the GraphQL schema linter, since the schemas are where they are
// truly defined.
var DeprecatedTerminologyAnalyzer = &analysis.Analyzer{
	Name: "deprecated_terminology",
	Doc:  "prohibits terminology we want to replace",
	Run:  _lintDeprecatedTerminology,
}

// Split s into words, splitting on spaces, underscores, dashes, and
// camel-case.  We also treat a run of capital letters as a single
// word (ignoring the last capital letter in the run), to match how
// Go handles intialisms.  We only (correctly) handle 7-bit ascii.
func _words(s string) []string {
	var words []string
	var buf strings.Builder
	finish := func() {
		word := buf.String()
		if word != "" {
			words = append(words, word)
		}
		buf.Reset()
	}

	for i, c := range s {
		if unicode.IsSpace(c) || c == '_' || c == '-' {
			// Start a new word, don't include the delimeter.
			finish()
		} else {
			// If we have an uppercase letter, it starts a new word
			// *unless* it's in the middle of an initialism like
			// MakeHTMLBuffer.  "In the middle of" means not
			// the first character and not the last.
			if unicode.IsUpper(c) {
				// We cheat here since we know we are 7-bit ascii.
				// "Proper" code would use bufio (and UnreadRune) instead.
				lastWasUpper := i > 0 && s[i-1] >= 'A' && s[i-1] <= 'Z'
				nextIsUpper := i == len(s)-1 || (s[i+1] >= 'A' && s[i+1] <= 'Z')
				if !lastWasUpper || !nextIsUpper {
					finish()
				}
			}
			// Either way, add this character.
			buf.WriteRune(c)
		}
	}
	finish() // Finish off the last word.

	return words
}

// _term describes a term we want to forbid.  See _deprecatedTerms for the
// list!
type _term struct {
	// The words we will match (we'll match them in both camel-cased and
	// snake_cased forms).  Note we will match these words as a substring of a
	// longer phrase, but only look for whole words (so if the deprecated term
	// is "model" we'll prohibit "ModelID" but not "ModelingAgency".
	// TODO(benkraft): Support initialisms (ModelID) in the term itself; they
	// currently don't work because _words doesn't break them correctly.
	words []string
	// List of terms that are okay usages of the term (but contain it), such as
	// an allowed casing or a term only allowed when preceded by a certain
	// qualifier.
	okWithin []_term
	// Suggested replacements, as strings (typically space-separated).  Used in
	// error messages only; they are not treated specially.  May be omitted
	// from terms used in okWithin.
	replacements []string
	// Filenames that are allowed to use the forbidden term, as absolute paths.
	allowedAbspaths []string
	// Directories that are allowed to contain the forbidden term, either
	// in any file in the directory itself or any file in any subdirectory
	// (recursively), as absolute paths.
	allowedDirsAbspaths []string
}

// _quotedDisjunctiveEnglishList takes a list like {"a", "b", "c"}, and
// returns a human-readable english string like "'a', 'b', or 'c'"
func _quotedDisjunctiveEnglishList(list []string) string {
	l := len(list)
	switch l {
	case 0:
		return ""
	case 1:
		return "'" + list[0] + "'"
	case 2:
		return "'" + list[0] + "' or '" + list[1] + "'"
	default:
		return "'" + strings.Join(list[:l-1], "', '") + "', or '" + list[l-1] + "'"
	}
}

func (t *_term) message() string {
	return fmt.Sprintf(
		"the name '%s' is deprecated, use %s "+
			"instead (or see https://khanacademy.org/r/renaming)",
		strings.Join(t.words, " "), _quotedDisjunctiveEnglishList(t.replacements))
}

func (t *_term) _matches(words []string) bool {
	if len(t.words) != len(words) {
		return false
	}
	for i := 0; i < len(t.words); i++ {
		if !strings.EqualFold(t.words[i], words[i]) {
			return false
		}
	}

	return true
}

func (t *_term) isOKMatch(words []string) bool {
	for _, okTerm := range t.okWithin {
		if okTerm.isContainedIn(words) {
			return true
		}
	}

	return false
}

func (t *_term) isContainedIn(words []string) bool {
	for offset := 0; offset < len(words)-len(t.words)+1; offset++ {
		if t._matches(words[offset : offset+len(t.words)]) {
			// found a match; first check if it's an ok term.
			// TODO(benkraft): This is not strictly correct: we should only
			// allow matches if the matching ok term is a superset of this one.
			// But that's more work, and rare.  (See OKWithinBadMatch test for
			// an example.)
			if !t.isOKMatch(words) {
				return true
			}
		}
	}

	return false
}

func (t *_term) isAllowedInFile(abspath string) bool {
	for _, allowedPath := range t.allowedAbspaths {
		if abspath == allowedPath {
			return true
		}
	}

	for _, allowedDirpath := range t.allowedDirsAbspaths {
		if strings.HasPrefix(abspath, allowedDirpath) {
			return true
		}
	}

	return false
}

// Fluent functions to help construct _deprecatedTerms.
func _makeTerm(actual string, replacements ...string) *_term {
	return &_term{
		words:        _words(actual),
		replacements: replacements,
	}
}

func (t *_term) isOKWithin(okWithin ...string) *_term {
	t.okWithin = make([]_term, len(okWithin))
	for i, s := range okWithin {
		t.okWithin[i] = _term{words: _words(s)}
	}

	return t
}

func (t *_term) isAllowedInFiles(relpaths ...string) *_term {
	ctx := context.Background()
	t.allowedAbspaths = make([]string, len(relpaths))
	for i, path := range relpaths {
		t.allowedAbspaths[i] = lib.KARootJoin(ctx, path)
	}

	return t
}

// isAllowedInDirectories adds directories that are allowed to contain the
// term, either in any file in the directory itself or any file in any
// subdirectory (recursively).
func (t *_term) isAllowedInDirectories(relpaths ...string) *_term {
	ctx := context.Background()
	t.allowedDirsAbspaths = make([]string, len(relpaths))
	for i, path := range relpaths {
		// lib.KARootJoin() will never return a path with a trailing slash, so
		// we add one to make sure we don't accidentally match files or
		// other directories that have the given directory name as a prefix.
		t.allowedDirsAbspaths[i] = lib.KARootJoin(ctx, path) + "/"
	}

	return t
}

// The terms we don't allow, modified from the canonical list at
// https://khanacademy.org/r/renaming.  A few we don't look for include:
// - topic: many legitimate uses in varied contexts
// - coach, coachee, child, student: all have legitimate uses
// - name: only applies to exercises, many legitimate uses elsewhere
// - kmap: incorrect capitalization appears almost nowhere at this point, and
//   would require special support to handle
var _deprecatedTerms = []*_term{
	_makeTerm("student list", "classroom"),
	_makeTerm("student lists", "classrooms"),

	_makeTerm("u13", "under age gate", "child (if you mean 'user with a parent')"),
	_makeTerm("u13s", "under age gates", "children (if you mean 'users with parents')"),
	_makeTerm("under13", "under age gate", "child (if you mean 'user with a parent')"),
	_makeTerm("under13s", "under age gates", "children (if you mean 'users with parents')"),

	_makeTerm("language", "KA locale").
		isOKWithin(
			// language-tag is the thing in the domain/querystring, and if you
			// know to say that you probably know that it's not the same as a
			// ka-locale.
			"language tag",
			// The "language picker" is how we refer to the thing in the
			// footer of the logged out homepage that lets you pick what
			// ka-locale to use.
			"language picker",
			// natural-language comes up in the context of translations and is
			// fine.
			"natural language",
			// American Sign Language is a language we want to check for
			// within locales
			"American Sign Language",
		),
	_makeTerm("languages", "KA locales").
		isOKWithin("language tags", "natural languages"),
	_makeTerm("lang", "KA locale").
		isOKWithin(
			// youtubeLanguage would be better but this isn't the
			// place to complain.
			"youtube lang", "crowdin lang",
			// Sometimes people use "langTag" to refer to the `?lang=xx` tag.
			"lang tag", "lang tags",
		).
		isAllowedInFiles(
			// locale_util.go is generic, and thus uses generic terminology.
			"services/content-library/rssfeeds/locale_util.go",
			"services/content-library/rssfeeds/locale_util_test.go",
		),
	_makeTerm("langs", "KA locales").
		isOKWithin("youtube langs", "crowdin langs"),
	_makeTerm("locale", "KA locale", "youtube locale/etc").
		isOKWithin(
			// Saying the explicit type of locale is, of course, a-ok!
			"ka locale", "youtube locale", "crowdin locale", "zendesk locale",
			// A few integration points in our code need to deal with multiple
			// types of locale codes, which we pass around in a type that we
			// call "locale info".
			"locale info", "locale infos",
			// "Pseudo-locale" is a collective term for locales that serve
			// some special purpose and aren't "real", such as the testing
			// locales "boxes" and "accents" (whose translations are
			// autogenerated nonsense), and "en-pt" (whose "translations" are
			// actually the Crowdin JIPT string codes).
			"pseudo locale",
			// "English locale" isn't great naming, but since it's the same
			// in every locale system I'm not going to bother to change it.
			"english locale",
			// Permissions has a concept of a "locale scope".
			"locale scope",
		).
		isAllowedInFiles(
			"services/content-library/rssfeeds/locale_util.go",
			"services/content-library/rssfeeds/locale_util_test.go",
		),
	_makeTerm("locales", "KA locales", "youtube locales/etc").
		isOKWithin(
			// See the rationales for "locale" above
			"ka locales", "youtube locales", "crowdin locales",
			"pseudo locales",
			"locale scopes",
		).
		isAllowedInDirectories(
			// We actually want a catch-all term for this package
			// name, since it deals with different types of locales.
			"services/content-editing/locales",
		),

	_makeTerm("commit sha", "published content version"),
	_makeTerm("commit shas", "published content versions"),
	_makeTerm("commit SHAs", "published content versions"),
	_makeTerm("publish sha", "published content version"),
	_makeTerm("publish shas", "published content versions"),
	_makeTerm("publish SHAs", "published content versions"),

	// DCUL (Domain/Course/Unit/Lesson), not Domain/Subject/Topic/Tutorial,
	// or the obscure Domain/Curation/Concept/Tutorial.
	_makeTerm("subject", "course", "subject line").
		isOKWithin(
			"subject line", "subject lines", // for emails
		).
		// also uses "subject" as an email subject line
		isAllowedInFiles("services/users/testutil/all_fails_email_client.go").
		isAllowedInDirectories(
			// The emails pkg and service are allowed to use "subject"
			// to mean "subject line".  We know they don't deal with
			// content types so "subject" is unambiguous there.
			"pkg/emails/", "services/emails/",
		),
	_makeTerm("subjects", "courses").
		isAllowedInDirectories("pkg/emails/", "services/emails/"),
	_makeTerm("curation", "course").
		isOKWithin(
			"curation node", "curation nodes", // term for "non-leaf node"
			"curation path", "curation paths", // term for that path to the left of leaf nodes
			"curation page", "curation pages", // an SEO-oriented landing page
			"curation data",         // publishing term for curation-node specific data
			"curation module",       // term for components rendered on "non-leaf node" pages
			"landing page curation", // deprecated test-prep terminology
		),
	_makeTerm("curations", "courses"),

	_makeTerm("topic", "unit", "curation node", "topic name (for pubsub topics)").
		isOKWithin(
			// When talking about pubsub topics.
			"topic name", "pubsub topic",
			// These aren't actually ok, but we have a separate check for
			// them below; we list them here so we don't double-warn.
			"topic quiz", "topic quizzes", "topic unit test", "topic unit tests",
			"topic icon url", "topic path", "topic paths",
		).
		isAllowedInFiles(
			// TAP code is semi-deprecated, so we're not bothering to
			// fix it up at this time.
			"services/content-editing/resolvers/tap.go",
			"services/content-editing/tap/result_data.go",
			"services/content-editing/tap/result_data_test.go",
			// This legacy model will go away once we port all our content to
			// the new course editor; let's let it be legacy in the meantime.
			"services/content-editing/models/topic_revision.go",
			"services/content-editing/models/topic_revision_test.go",
			"services/content-editing/models/raw_models/topic_revision.go",
			"services/content-editing/models/raw_models/testutil/mock.go",
		).
		// Files in the gcloud package are referring to the pubsub definition
		// of "topic"
		isAllowedInDirectories("pkg/gcloud/"),
	_makeTerm("topics", "units", "curation nodes", "topic names (for pubsub topics)").
		isOKWithin(
			// These aren't actually ok, but we have a separate check for
			// them below; we list them here so we don't double-warn.
			"topic quizzes", "topic unit tests",
			"topic icon urls", "topic paths",
		).
		isAllowedInFiles(
			// TAP code is semi-deprecated, so we're not bothering to
			// fix it up at this time.
			"services/content-editing/resolvers/tap.go",
			"services/content-editing/tap/result_data.go",
			"services/content-editing/tap/result_data_test.go",
		).
		// Files in the gcloud package are referring to the pubsub definition
		// of "topic"
		isAllowedInDirectories("pkg/gcloud/"),

	_makeTerm("topic quiz", "quiz"),
	_makeTerm("topic quizzes", "quizzes"),
	_makeTerm("topic unit test", "unit test"),
	_makeTerm("topic unit tests", "unit tests"),
	_makeTerm("topic icon url", "classroom icon url"),
	_makeTerm("topic path", "url path"),
	_makeTerm("topic paths", "url paths"),

	_makeTerm("concept", "unit").
		isOKWithin(
			"concept tag", "concept tags", // a (deprecated) form of tagging
		).
		// GTP has things called "concepts" which are like skills
		isAllowedInDirectories("services/test-prep/"),
	_makeTerm("concepts", "units").
		// GTP has things called "concepts" which are like skills
		isAllowedInDirectories("services/test-prep/"),

	_makeTerm("tutorial", "lesson"),
	_makeTerm("tutorials", "lessons"),

	// Our leaf (content) nodes have some changes too, both to their
	// types and some fields they own.
	_makeTerm("scratchpad", "program"),
	_makeTerm("scratchpads", "programs"),

	_makeTerm("readable ID", "slug"),
	_makeTerm("readable IDs", "slugs"),

	_makeTerm("learn menu", "courses menu", "course collections"),
	_makeTerm("learn menus", "courses menus", "course collections"),
}

func _lintDeprecatedTerminology(pass *analysis.Pass) (interface{}, error) {
	// Check the filename.  (Package names are covered by their package
	// clause, which is an ident, below.)
	for _, file := range pass.Files {
		abspath := pass.Fset.File(file.Pos()).Name()
		filename := filepath.Base(abspath)
		filename = strings.TrimSuffix(filename, filepath.Ext(filename))
		filenameWords := _words(filename)

		for _, term := range _deprecatedTerms {
			if term.isContainedIn(filenameWords) && !term.isAllowedInFile(abspath) {
				pass.Reportf(file.Pos(), "%s (in filename)", term.message())
			}
		}
	}

	// We allow deprecated terminology in resolver functions -- the
	// name of the function and the name of its args -- because gqlgen
	// specifies how those functions are named; we can't change them.
	resolverNames := make(map[*ast.Ident]bool)
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if lintutil.IsResolverFunc(funcDecl, pass.TypesInfo) {
				// Name of the function.
				resolverNames[funcDecl.Name] = true
				// Name of the function parameters.
				for _, param := range funcDecl.Type.Params.List {
					for _, name := range param.Names {
						resolverNames[name] = true
					}
				}
			}
		}
	}

	// Everything else we care about is an identifier (see analyzer doc).
	for ident := range pass.TypesInfo.Defs {
		if resolverNames[ident] {
			continue
		}

		words := _words(ident.Name)
		for _, term := range _deprecatedTerms {
			if term.isContainedIn(words) {
				abspath := pass.Fset.File(ident.Pos()).Name()
				if !term.isAllowedInFile(abspath) {
					pass.Reportf(ident.Pos(), term.message())
				}
			}
		}
	}

	return nil, nil
}
