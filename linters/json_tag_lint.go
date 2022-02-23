package linters

// Lints that structs used in JSON marshal/unmarshal calls have explicit JSON
// tags.

import (
	"fmt"
	"go/ast"
	"go/types"
	"reflect"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

// JSONTagAnalyzer lints that structs used in JSON marshal/unmarshal calls have
// explicit JSON tags.
//
// The encoding/json interface will happily JSONify structs that don't have
// JSON tags, just using the Go field names for the JSON keys.  This is rarely
// a good idea for us; it means changing a field name will change the JSON
// output.  (Additionally, on unmarshal, it may mean the field just gets
// ignored instead of being decoded correctly!)
//
// To avoid this, we require that any struct which we encode to or decode from
// JSON must have json struct tags on all its fields (or must implement the
// appropriate Marshaler/Unmarshaler interface, meaning that it fully defines
// how it encodes to JSON).
//
// For further documentation on how to define json tags, see
// `go doc encoding/json.Marshal`.
//
// TODO(benkraft): If other encodings like gob and xml become common, apply the
// same rules to them as well.
// TODO(benkraft): Search for types used in struct fields tagged with
// `kadatastore_json`, and validate those as well.
// TODO(benkraft): Search for structs which have a JSON tag, and assume that
// means they are JSONified, perhaps via an empty interface, or other code that
// this linter can't trace.
var JSONTagAnalyzer = &analysis.Analyzer{
	Name: "json_tag",
	Doc:  "bans JSONifying structs without explicit tags",
	Run:  _runJSON,
}

// A function we want to look for which does JSON marshaling or unmarshaling,
// like json.Marshal.  These are typically in the encoding/json package, but in
// theory could be elsewhere, say if we start using a third-party JSON library.
type _jsonifyingFunction struct {
	name     string // as determined by lintutil.NameOf
	argIndex int    // index of the argument that's encoded or decoded into
	// marshaler is the unqualified name of the unmarshaler/marshaler interface
	// this function may use, depending whether it's encoding or decoding.
	// It's assumed (by getMarshalInterface()) that this belongs to the same
	// package as the function itself.
	marshaler string
}

// Given the package in which this jsonifier is defined, return the actual
// *types.Interface representing its marshaler.
func (jsonifier _jsonifyingFunction) getMarshalInterface(pkg *types.Package) *types.Interface {
	obj := pkg.Scope().Lookup(jsonifier.marshaler)
	if obj == nil {
		// likely means that the list of jsonifying functions itself is
		// wrong, so just panic
		panic(fmt.Sprintf("couldn't find marshaler interface %v in %v",
			jsonifier.marshaler, pkg.Path()))
	}
	underlyingType := obj.Type().Underlying()
	underlyingInterface, ok := underlyingType.(*types.Interface)
	if !ok {
		// again, likely a problem in the list of jsonifying functions
		panic(fmt.Sprintf("marshaler %v.%v is %T, not interface",
			pkg.Path(), jsonifier.marshaler, underlyingType))
	}
	return underlyingInterface
}

var _jsonifyingFunctions = []_jsonifyingFunction{
	{"encoding/json.Marshal", 0, "Marshaler"},
	{"encoding/json.MarshalIndent", 0, "Marshaler"},
	{"encoding/json.Unmarshal", 1, "Unmarshaler"},
	{"(*encoding/json.Decoder).Decode", 0, "Unmarshaler"},
	{"(*encoding/json.Encoder).Encode", 0, "Marshaler"},
}

var _jsonifyingFunctionsByName = map[string]_jsonifyingFunction{}

func init() {
	for _, f := range _jsonifyingFunctions {
		_jsonifyingFunctionsByName[f.name] = f
	}
}

type _jsonifiedValue struct {
	node ast.Node   // node where this value is jsonified
	typ  types.Type // type into/out of which we jsonify
	// marshalInterface stores a reference to the relevant
	// marshaler/unmarshaler interface, depending on whether this function is
	// an encoder or a decoder.  We store it here because it's surprisingly
	// hard to look up a function by name with go/types, but it's very easy if
	// you have a reference to the function in the same package.
	marshalInterface *types.Interface
}

func _getJSONifiedValues(pass *analysis.Pass) []_jsonifiedValue {
	var retval []_jsonifiedValue
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			callExpr, ok := node.(*ast.CallExpr)
			if !ok {
				return true // recurse
			}

			funcObj := lintutil.ObjectFor(callExpr.Fun, pass.TypesInfo)
			funcName := lintutil.NameOf(funcObj)
			jsonifier, ok := _jsonifyingFunctionsByName[funcName]
			// len check is just to be safe, it should never happen
			if !ok || jsonifier.argIndex >= len(callExpr.Args) {
				return true
			}

			arg := callExpr.Args[jsonifier.argIndex]
			retval = append(retval, _jsonifiedValue{
				node:             arg,
				typ:              pass.TypesInfo.TypeOf(arg),
				marshalInterface: jsonifier.getMarshalInterface(funcObj.Pkg()),
			})
			return true
		})
	}
	return retval
}

type stringStack struct {
	// the stack is buf[:length]; the rest of buf is buffer to reuse
	buf    []string
	length int
}

func (s *stringStack) push(v string) {
	if s.length >= len(s.buf) {
		s.buf = append(s.buf, v)
	} else {
		s.buf[s.length] = v
	}
	s.length++
}

func (s *stringStack) pop() string {
	if s.length == 0 {
		panic("pop from empty stack")
	}

	s.length--
	return s.buf[s.length]
}

// read the full stack; the returned value MUST NOT be modified in place.
func (s *stringStack) read() []string {
	return s.buf[:s.length]
}

type _typeAnalyzer struct {
	marshalInterface *types.Interface
	seen             map[types.Type]bool
	path             stringStack
}

// Analyze the given type, and return paths to any fields that need JSON tags.
//
// The argument marshalInterface is documented at
// _jsonifiedValue.marshalInterface; path should be "" on the first call and is
// used for recursion.
func (analyzer *_typeAnalyzer) run(typ types.Type) []string {
	// avoid infinite recursion
	if analyzer.seen[typ] {
		return nil
	}
	analyzer.seen[typ] = true

	// If this type implements Marshaler/Unmarshaler (as appropriate),
	// it presumptively knows what it's doing.
	// TODO(benkraft): Also allow TextMarshaler/TextUnmarshaler, which
	// encoding/json knows how to use (and are more convenient when the type
	// will be marshaled to a string).
	if types.Implements(typ, analyzer.marshalInterface) ||
		// We also allow if the pointer to the type implements the interface,
		// because JSON does (e.g. if you have a struct field, and a pointer to
		// that type implements Marshaler.)
		// TODO(benkraft): I'm not totally sure if there are edge cases where
		// this isn't allowed; if we find any, add them.
		types.Implements(types.NewPointer(typ), analyzer.marshalInterface) {
		return nil
	}

	// otherwise, traverse the type, and see what we find.
	var badPaths []string
	recurse := func(typ types.Type, pathSuffix string) {
		if pathSuffix != "" {
			analyzer.path.push(pathSuffix)
			defer analyzer.path.pop()
		}

		badPaths = append(badPaths, analyzer.run(typ)...)
	}
	complain := func(pathSuffix string) {
		if pathSuffix != "" {
			analyzer.path.push(pathSuffix)
			defer analyzer.path.pop()
		}

		badPaths = append(badPaths, strings.Join(analyzer.path.read(), "."))
	}

	// cases from https://github.com/golang/example/tree/master/gotypes#types
	switch typ := typ.(type) {
	case *types.Array:
		recurse(typ.Elem(), "[element]")
	case *types.Basic: // nothing to validate
	case *types.Chan: // not jsonifiable
	case *types.Interface: // too hard to validate
	case *types.Map:
		// recurse on value only; key must be string-ish
		recurse(typ.Elem(), "[value]")
	case *types.Named:
		// if this type had a marshaler/unmarshaler method, we handled that
		// above, so just recurse on the underlying type (e.g. struct fields)
		recurse(typ.Underlying(), "")
	case *types.Pointer:
		recurse(typ.Elem(), "")
	case *types.Signature: // not jsonifiable
	case *types.Slice:
		recurse(typ.Elem(), "[element]")
	case *types.Struct:
		for i := 0; i < typ.NumFields(); i++ {
			field := typ.Field(i)
			if !field.Exported() {
				continue // json won't see this at all
			}

			_, isStruct := field.Type().Underlying().(*types.Struct)
			tag, ok := reflect.StructTag(typ.Tag(i)).Lookup("json")
			switch {
			// Embedded structs are encoded as if their inner exported
			// fields were fields of the outer struct, unless they have an
			// explicit struct tag.  So we need to recurse on them, but we
			// *don't* need to complain if they lack a tag.
			//
			// For example, if we have
			//	type T struct {
			//		A string `json:"a"`
			//		U
			//	}
			//	type U struct {
			//		B string `json:"b"`
			//	}
			// then when we JSONify we'll get a single object with keys "a" and
			// "b".  We need to recurse through U, to check that B has a struct
			// tag.  But we don't need U itself to have a struct tag.
			case !ok && !(isStruct && field.Embedded()):
				complain(field.Name())
			case tag == "-":
				continue // ignored by json, no need to recurse
			}

			recurse(field.Type(), field.Name())
		}
	case *types.Tuple: // not a value that can be passed
	default:
		panic(fmt.Sprintf("unexpected type %v (%T)", typ, typ))
	}

	return badPaths
}

func _runJSON(pass *analysis.Pass) (interface{}, error) {
	// map from [type, marshal-interface] to its problems.
	problemsCache := map[[2]types.Type][]string{}
	for _, val := range _getJSONifiedValues(pass) {
		cacheKey := [2]types.Type{val.typ, val.marshalInterface}
		problems, ok := problemsCache[cacheKey]
		if !ok {
			analyzer := _typeAnalyzer{
				marshalInterface: val.marshalInterface,
				seen:             map[types.Type]bool{},
			}
			problems = analyzer.run(val.typ)
			problemsCache[cacheKey] = problems
		}

		if len(problems) > 0 {
			sort.Strings(problems)
			numToShow := 5
			if len(problems) > numToShow {
				suffix := fmt.Sprintf("and %v more", len(problems)-numToShow)
				problems = append(problems[:numToShow], suffix)
			}

			pass.Reportf(val.node.Pos(),
				"JSONified type %v lacks explicit struct tags; "+
					"add explicit struct tags to avoid surprises. "+
					"see `go doc dev/linters.JSONTagAnalyzer` for more. "+
					"fields missing tags: %s",
				val.typ, strings.Join(problems, ", "))
		}
	}

	return nil, nil
}
