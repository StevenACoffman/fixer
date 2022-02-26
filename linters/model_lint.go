// Package linters contains datastore model-related linters.
package linters

// This file defines the linter for various requirements we have for datastore
// model definitions. For example, defining a transaction safety policy,
// checking the policy in PreSave, embedding the necessary base models, etc.
//
// NOTE(marksandstrom): if any new model lint rules are added, they should
// probably be implemented in a separate analyzer.
//
// NOTE(marksandstrom): consider breaking this analyzer in two: an analyzer
// that performs the base and transaction safety checks, and a analyzer that
// checks CheckTransactionSafetyForPut.

import (
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/StevenACoffman/fixer/lintutil"
)

var ModelAnalyzer = &analysis.Analyzer{
	Name: "model",
	Doc:  "enforces rules relating to the definition of datastore model types",
	Run:  _runModel,
}

const (
	_datastorePkg                     = "github.com/Khan/webapp/pkg/gcloud/datastore"
	_baseModelName                    = _datastorePkg + ".BaseModel"
	_structruedPropertyBaseModelName  = _datastorePkg + ".StructuredPropertyBaseModel"
	_checkTransactionSafetyForPutName = _datastorePkg + ".CheckTransactionSafetyForPut"
)

// KNOWN ISSUES:
//   - Fields in embedded structs aren't inspected to see if they contain
//     nested models.
//   - The method signature for PreSave on StructuredPropertyBaseModel isn't
//     checked, meaning this this linter doesn't guarantee that the types
//     implement the corresponding interfaces. (See TODO below.)
//   - Nested properties must be in the same lint pass as the the models that
//     contain them. This is currently the case for all of out code, but it may
//     not always be so!

// TODO(marksandstrom): Create a StructuredPropertyModelBase interface and use
// types.Implements to check nested structs instead of checking via the ast
// (this would also make it possible/easier to check nested fields on embedded
// structs).

func _runModel(pass *analysis.Pass) (interface{}, error) {
	// If you are trying to modify or debug this analyzer, it might be helpful
	// to see the full ast the analyzer is traversing. To print the ast, use:
	//
	//     ast.Print(pass.Fset, pass.Files)
	//     return nil, nil

	// A map of package-qualified names of structs that embed BaseModel to
	// expressions where the structs are defined.
	baseModelNameToExpr := make(map[string]ast.Expr)
	// A map of package-qualified names of structs that embed
	// StructuredPropertyBaseModel to expressions where the structs are defined
	structuredPropertyModelNameToExpr := make(map[string]ast.Expr)
	// A map of package-qualified names of structs to information about the
	// PreSave methods defined on those structs.
	modelNameToPreSaveMethodInfo := make(map[string]*_preSaveMethodInfo)

	// Expressions for all of the struct field types that are nested in models.
	var nestedModelTypeExprs []ast.Expr

	// getExprName gets the package-qualified names for the given expression.
	getExprName := func(expr ast.Expr) string {
		obj := lintutil.ObjectFor(expr, pass.TypesInfo)

		return lintutil.NameOf(obj)
	}

	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			// Check to see if the declaration is a PreSave method declaration.
			// If it is, note the method information for later processing.
			preSaveMethodInfo, ok := _maybeGetPreSaveMethodInfo(decl, getExprName)
			if ok {
				modelName := getExprName(preSaveMethodInfo.modelTypeIdent)
				modelNameToPreSaveMethodInfo[modelName] = preSaveMethodInfo

				continue
			}

			// Check to see if the declaration is a struct type
			// declaration. If it is, check to see if the struct embeds a model
			// base, and note the type of model for later processing.
			structInfo, ok := _maybeGetStructInfo(decl, pass.TypesInfo)

			if !ok {
				continue
			}

			typeName := getExprName(structInfo.typeIdent)
			var isModel bool

			for _, embeddedTypeExpr := range structInfo.embeddedTypeExprs {
				embedName := getExprName(embeddedTypeExpr)

				switch embedName {
				case _baseModelName:
					isModel = true

					// If we don't already think this struct is a structured
					// property model, record it as a toplevel model.
					if structuredPropertyModelNameToExpr[typeName] == nil {
						baseModelNameToExpr[typeName] = structInfo.typeIdent
					} else {
						pass.Reportf(embeddedTypeExpr.Pos(),
							"model struct cannot embed both "+
								"BaseModel and StructuredPropertyBaseModel")
						// Remove the record of the toplevel model so that we
						// don't surface other model-related violations.
						delete(structuredPropertyModelNameToExpr, typeName)
					}

				case _structruedPropertyBaseModelName:
					isModel = true

					// If we don't already think this struct is a toplevel
					// model, record it as a structured property model.
					if baseModelNameToExpr[typeName] == nil {
						structuredPropertyModelNameToExpr[typeName] = structInfo.typeIdent
					} else {
						pass.Reportf(embeddedTypeExpr.Pos(),
							"model struct cannot embed both "+
								"BaseModel and StructuredPropertyBaseModel")
						// Remove the record of the toplevel model so that we
						// don't surface other model-related violations.
						delete(baseModelNameToExpr, typeName)
					}
				}
			}
			if isModel {
				// If we just found a model, note all of the fields that have
				// struct types. We expect these to be structured property
				// models.
				nestedModelTypeExprs = append(
					nestedModelTypeExprs, structInfo.nestedModelTypeExprs...)

				filename := pass.Fset.File(file.Pos()).Name()
				if !strings.HasSuffix(filename, "_test.go") {
					// Since this is for bug-prevention, we don't mind if tests
					// violate it.  They are, after all, tested!
					for _, structFieldExpr := range structInfo.structsMissingOmitEmpty {
						// If a struct doesn't use omitempty, that's probably a
						// bug -- a blank structured-property is almost never
						// what you want, and a zero time.Time or
						// datastore.GeoPt even more so.
						pass.Reportf(structFieldExpr.Pos(),
							"time/struct field without omitempty may save bad data; "+
								"see datastore docs for details")
					}
				}
			}
		}
	}

	for _, expr := range nestedModelTypeExprs {
		name := getExprName(expr)

		switch {
		case baseModelNameToExpr[name] != nil:
			pass.Reportf(expr.Pos(),
				"only models that embed StructuredPropertyModelBase "+
					"can be nested in other models")
		case structuredPropertyModelNameToExpr[name] == nil:
			pass.Reportf(expr.Pos(),
				"nested models must embed StructuredPropertyBaseModel")
		}
	}

	allModelExprs := make(
		[]ast.Expr, 0, len(baseModelNameToExpr)+len(structuredPropertyModelNameToExpr))

	for _, expr := range baseModelNameToExpr {
		allModelExprs = append(allModelExprs, expr)
	}
	for _, expr := range structuredPropertyModelNameToExpr {
		allModelExprs = append(allModelExprs, expr)
	}

	for _, expr := range allModelExprs {
		modelName := getExprName(expr)
		methodInfo, ok := modelNameToPreSaveMethodInfo[modelName]
		if !ok {
			// Should never happen for toplevel models (which must match
			// ToplevelModel interface), but it might for structured property
			// models.  (Do we care?)
			pass.Reportf(expr.Pos(),
				"all models must define a PreSave method that "+
					"calls CheckTransactionSafetyForPut")
		} else {
			// TODO(marksandstrom) Verify that PreSave has the correct
			// signature.
			if !methodInfo.callsCheckTransactionSafetyForPut {
				pass.Reportf(methodInfo.funcDecl.Pos(),
					"PreSave method must call CheckTransactionSafetyForPut")
			}
			// toplevel model PreSave functions must also call their "super"
			// (we don't require it for structured property -- in fact at this
			// time there is no StructuredPropertyBaseModel.PreSave to call).
			if baseModelNameToExpr[modelName] != nil &&
				!lintutil.CallsSuper(methodInfo.funcDecl, pass.TypesInfo) {
				pass.Reportf(methodInfo.funcDecl.Pos(),
					"PreSave method must call m.BaseModel.PreSave")
			}
		}

		typ := pass.TypesInfo.TypeOf(expr)
		if untaggedFields := _getUntaggedFields(typ); len(untaggedFields) > 0 {
			pass.Reportf(expr.Pos(),
				"Datastore model %v lacks explicit struct tags; "+
					"add `datastore:\"field_name\"` to avoid surprises. "+
					"See `go doc pkg/gcloud/datastore` for more. "+
					"Fields missing tags: %s",
				modelName, strings.Join(untaggedFields, ", "))
		}
	}

	return nil, nil
}

// _structInfo contains all of the information we need to know about a struct
// type declaration for model linting.
type _structInfo struct {
	// The type ident of the model declaration
	typeIdent *ast.Ident
	// The type exprs (either *ast.Ident of *ast.SelectorExpr) of all embedded
	// models
	embeddedTypeExprs []ast.Expr
	// The type idents of nested model properties
	nestedModelTypeExprs []ast.Expr
	// Struct fields that don't use omitempty (or a pointer, or a slice, etc.).
	// These are bugs waiting to happen, since the Go zero value is probably
	// not what we want.
	// Note that we apply this to time.Time and datastore.GeoPt because
	// datastore's special handling of them is also bad.  They happen to be
	// structs underneath so we don't need any special-casing.
	structsMissingOmitEmpty []ast.Expr
}

// _preSaveMethodInfo contains information about a specific model's PreSave
// method.
type _preSaveMethodInfo struct {
	// The ident of the method (for reporting the method position)
	funcDecl *ast.FuncDecl
	// The ident of the model type this method is defined on
	modelTypeIdent *ast.Ident
	// Whether this PreSave definition calls CheckTransactionSafetyForPut
	callsCheckTransactionSafetyForPut bool
}

// _maybeGetStructSpec returns information about a struct type declaration if
// the given declaration is such a declaration.
func _maybeGetStructSpec(decl ast.Decl) (*ast.TypeSpec, *ast.StructType, bool) {
	genDecl, ok := decl.(*ast.GenDecl)
	if !ok {
		return nil, nil, false
	}
	if genDecl.Tok != token.TYPE {
		return nil, nil, false
	}
	spec, _ := genDecl.Specs[0].(*ast.TypeSpec)
	structType, ok := spec.Type.(*ast.StructType)
	if !ok {
		return nil, nil, false
	}

	return spec, structType, true
}

// _maybeGetStructInfo determines if the passed declaration is a struct
// declaration, and if so, returns the struct type ident, the idents of any
// embedded structs and the idents of field types that could would be
// structured property fields if the struct is determined to be a model.
func _maybeGetStructInfo(decl ast.Decl, typesInfo *types.Info) (*_structInfo, bool) {
	// A struct is a general declaration with a type of token.Type.
	// Declarations with this type have one type spec, and the type of that
	// spec is *ast.StructType.
	spec, structType, ok := _maybeGetStructSpec(decl)
	if !ok {
		return nil, false
	}
	info := &_structInfo{typeIdent: spec.Name}

	for _, field := range structType.Fields.List {
		// Embedded fields are field without names.
		if len(field.Names) == 0 {
			info.embeddedTypeExprs = append(info.embeddedTypeExprs, field.Type)

			continue
		}
		// Next, look for fields that have struct types. These are nested
		// models unless the field is ignored because it is non-exported or
		// ignored with a datastore tag.
		baseExpr := _baseExpr(field.Type)
		obj := lintutil.ObjectFor(baseExpr, typesInfo)
		if obj == nil {
			continue
		}
		// Basic types like string, int, etc don't have a package.
		if obj.Pkg() == nil {
			continue
		}
		// Filter out non-struct fields.
		_, ok := obj.Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}

		// Filter out non-exported fields.
		if !field.Names[0].IsExported() {
			continue
		}

		// Filter out fields ignored via a datastore tag, and check for
		// omitempty.
		omitempty := false
		if field.Tag != nil {
			tagString, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				continue
			}
			tag := reflect.StructTag(tagString)
			datastoreTag := tag.Get("datastore")
			if datastoreTag == "-" {
				continue
			}

			if datastoreTag != "" {
				for _, opt := range strings.Split(datastoreTag, ",")[1:] {
					if opt == "omitempty" {
						omitempty = true
					}
				}
			}
		}

		if !omitempty && baseExpr == field.Type {
			// If the struct is not a pointer/slice/etc., and doesn't have
			// omitempty, note that.
			info.structsMissingOmitEmpty = append(
				info.structsMissingOmitEmpty, baseExpr)
		}

		// Filter out struct types that have special handling by the datastore
		// client. See:
		// https://pkg.go.dev/cloud.google.com/go/datastore?tab=doc#pkg-overview
		if obj.Pkg().Path() == "time" && obj.Name() == "Time" {
			continue
		}
		if obj.Pkg().Path() == "cloud.google.com/go/civil" {
			continue
		}
		if obj.Pkg().Path() == _datastorePkg && obj.Name() == "Key" {
			continue
		}
		if obj.Pkg().Path() == _datastorePkg && obj.Name() == "GeoPoint" {
			continue
		}
		// Looks like we have a nested model struct!
		info.nestedModelTypeExprs = append(info.nestedModelTypeExprs, baseExpr)
	}

	return info, true
}

// _baseExpr returns the expression for the type that is the basis of a more
// complex type, e.g. the base type of []MyType is MyType and the base type of
// *MyType is MyType.
func _baseExpr(fieldType ast.Expr) ast.Expr {
	switch v := fieldType.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return v
	case *ast.StarExpr:
		return _baseExpr(v.X)
	// Unwrap slices. ArrayType is the ast type for slices.
	case *ast.ArrayType:
		return _baseExpr(v.Elt)
	default:
		return nil
	}
}

// _getUntaggedFields accepts an ast.Expr representing a datastore model, and
// returns any fields of that model that don't have explicit datastore tags.
//
// It recurses on embedded fields, but does *not* recurse on nested model
// types, as it's assumed those are handled separately.
//
// This is basically parallel to dev/linters.JSONTagAnalyzer, but datastore
// models are slightly different.
func _getUntaggedFields(model types.Type) []string {
	typ, ok := model.Underlying().(*types.Struct)
	if !ok {
		// Non-struct models need to have custom serialization, so presumably
		// if you do that you know what you are doing!  (In practice, we don't
		// use such models at all.)
		return nil
	}

	var retval []string

	for i := 0; i < typ.NumFields(); i++ {
		field := typ.Field(i)
		_, isStruct := field.Type().Underlying().(*types.Struct)
		if isStruct && field.Embedded() {
			// Embedded struct fields don't need to have a datastore tag, but
			// their fields do!
			retval = append(retval, _getUntaggedFields(field.Type())...)

			continue
		}

		if !field.Exported() {
			continue
		}

		tag := reflect.StructTag(typ.Tag(i))
		datastoreTag := tag.Get("datastore")
		// We disallow tags that just have options (`datastore:",omitempty"`)
		// since that's basically the same as not having a tag.
		if datastoreTag == "" || strings.HasPrefix(datastoreTag, ",") {
			retval = append(retval, field.Name())
		}
	}

	return retval
}

// _maybeGetPreSaveMethodInfo determines if the given declaration is a PreSave
// method declaration, and if it is, it returns various information about that
// method. See _preSaveMethodInfo for details.
func _maybeGetPreSaveMethodInfo(
	decl ast.Decl,
	getExprName func(expr ast.Expr) string,
) (*_preSaveMethodInfo, bool) {
	funcDecl, ok := decl.(*ast.FuncDecl)
	if !ok {
		return nil, false
	}
	if funcDecl.Recv == nil {
		return nil, false
	}
	// Note: we check that the type actually implements the PreSaver interface
	// in the main body of the linter. The type might not impelement the
	// PreSaver interface if the method signature is incorrect.
	if funcDecl.Name.Name != "PreSave" {
		return nil, false
	}
	var callsCheckTransactionSafetyForPut bool
	ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
		// We're looking for a call to CheckTransactionSafetyForPut
		var callExpr *ast.CallExpr
		if callExpr, ok = node.(*ast.CallExpr); ok {
			var sel *ast.SelectorExpr
			if sel, ok = callExpr.Fun.(*ast.SelectorExpr); ok {
				if getExprName(sel) == _checkTransactionSafetyForPutName {
					callsCheckTransactionSafetyForPut = true
				}
			}
		}
		// recurse if we haven't found a call
		return !callsCheckTransactionSafetyForPut
	})

	recvType := funcDecl.Recv.List[0].Type

	var modelTypeIdent *ast.Ident
	starExpr, ok := recvType.(*ast.StarExpr)
	// Check if the receiver is a pointer, and if so dereference it.
	if ok {
		modelTypeIdent, _ = starExpr.X.(*ast.Ident)
	} else {
		modelTypeIdent, _ = recvType.(*ast.Ident)
	}

	return &_preSaveMethodInfo{
		funcDecl:                          funcDecl,
		modelTypeIdent:                    modelTypeIdent,
		callsCheckTransactionSafetyForPut: callsCheckTransactionSafetyForPut,
	}, true
}
