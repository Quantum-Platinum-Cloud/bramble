package bramble

import (
	"fmt"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

func MergeSchemas(schemas ...*ast.Schema) (*ast.Schema, error) {
	if len(schemas) < 1 {
		return nil, fmt.Errorf("no source schemas")
	}
	if len(schemas) == 1 {
		// if we have only one schema we append a minimal schema so that we can
		// still go through the merging logic and prune special types (e.g.
		// Service)
		schemas = append(schemas, gqlparser.MustLoadSchema(&ast.Source{Name: "empty schema", Input: `
		type Service {
			name: String!
			version: String!
			schema: String!
		}

		type Query {
			service: Service!
		}
		`}))
	}

	merged := ast.Schema{
		Types:         make(map[string]*ast.Definition),
		Directives:    make(map[string]*ast.DirectiveDefinition),
		PossibleTypes: make(map[string][]*ast.Definition),
	}

	merged.Types = schemas[0].Types
	for _, schema := range schemas[1:] {
		mergedTypes, err := mergeTypes(merged.Types, schema.Types)
		if err != nil {
			return nil, err
		}
		merged.Types = mergedTypes
	}

	merged.Implements = mergeImplements(schemas)
	merged.PossibleTypes = mergePossibleTypes(schemas, merged.Types)
	merged.Directives = mergeDirectives(schemas)

	merged.Query = merged.Types[queryObjectName]
	merged.Mutation = merged.Types[mutationObjectName]
	merged.Subscription = merged.Types[subscriptionObjectName]

	return &merged, nil
}

func buildFieldURLMap(services ...*Service) FieldURLMap {
	result := FieldURLMap{}
	for _, rs := range services {
		for _, t := range rs.Schema.Types {
			if t.Kind != ast.Object || isGraphQLBuiltinName(t.Name) || t.Name == serviceObjectName {
				continue
			}
			for _, f := range mergeableFields(t) {
				if isBoundaryObject(t) && isIDField(f) {
					continue
				}

				// namespace objects live only on the graph
				fieldType := rs.Schema.Types[f.Type.Name()]
				if isNamespaceObject(fieldType) {
					continue
				}

				result.RegisterURL(t.Name, f.Name, rs.ServiceURL)
			}
		}
	}
	return result
}

func buildIsBoundaryMap(services ...*Service) map[string]bool {
	result := map[string]bool{}
	for _, rs := range services {
		for _, t := range rs.Schema.Types {
			if t.Kind != ast.Object || isGraphQLBuiltinName(t.Name) || t.Name == serviceObjectName {
				continue
			}
			result[t.Name] = isBoundaryObject(t)
		}
	}
	return result
}

func mergeTypes(a, b map[string]*ast.Definition) (map[string]*ast.Definition, error) {
	result := make(map[string]*ast.Definition)
	for k, v := range a {
		if k == nodeInterfaceName || k == serviceObjectName {
			continue
		}
		v.Interfaces = cleanInterfaces(v.Interfaces)
		v.Directives = cleanDirectives(v.Directives)
		result[k] = v
	}

	if b == nil {
		return result, nil
	}

	for k, vb := range b {
		if isGraphQLBuiltinName(k) || k == nodeInterfaceName || k == serviceObjectName {
			continue
		}

		va, found := result[k]
		if !found {
			vb.Interfaces = cleanInterfaces(vb.Interfaces)
			vb.Directives = cleanDirectives(vb.Directives)
			result[k] = vb
			continue
		}

		if vb.Kind != va.Kind {
			return nil, fmt.Errorf("name collision: %s(%s) conflicts with %s(%s)", vb.Name, vb.Kind, va.Name, va.Kind)
		}

		if vb.Kind == ast.Scalar {
			result[k] = vb
			continue
		}

		if !hasFederationDirectives(vb) || !hasFederationDirectives(va) {
			if k != queryObjectName && k != mutationObjectName {
				if vb.Kind == ast.Interface {
					return nil, fmt.Errorf("conflicting interface: %s (interfaces may not span multiple services)", k)
				}
				return nil, fmt.Errorf("conflicting non boundary type: %s", k)
			}
		}

		if isBoundaryObject(va) != isBoundaryObject(vb) || isNamespaceObject(va) != isNamespaceObject(vb) {
			return nil, fmt.Errorf("conflicting object directives, merged objects %q should both be boundary or namespaces", vb.Name)
		}

		// now, either it's boundary type, namespace type or the Query/Mutation type

		if va.Kind != ast.Object {
			return nil, fmt.Errorf("non object boundary type")
		}

		if isNamespaceObject(vb) || k == queryObjectName || k == mutationObjectName || k == subscriptionObjectName {
			mergedObject, err := mergeNamespaceObjects(a, b, vb, va)
			if err != nil {
				return nil, err
			}
			if len(mergedObject.Fields) == 0 {
				delete(result, k)
				continue
			}
			result[k] = mergedObject
			continue
		}

		mergedBoundaryObject, err := mergeBoundaryObjects(a, b, vb, va)
		if err != nil {
			return nil, err
		}

		var newInterfaces []string
		for _, i := range mergedBoundaryObject.Interfaces {
			if i == nodeInterfaceName {
				continue
			}
			newInterfaces = append(newInterfaces, i)
		}
		mergedBoundaryObject.Interfaces = newInterfaces

		result[k] = mergedBoundaryObject
	}

	return result, nil
}

func mergeImplements(sources []*ast.Schema) map[string][]*ast.Definition {
	result := map[string][]*ast.Definition{}
	for _, schema := range sources {
		for typeName, interfaces := range schema.Implements {
			for _, i := range interfaces {
				if i.Name != nodeInterfaceName {
					result[typeName] = append(result[typeName], i)
				}
			}
		}
	}
	return result
}

func mergeDirectives(sources []*ast.Schema) map[string]*ast.DirectiveDefinition {
	result := map[string]*ast.DirectiveDefinition{}
	for _, schema := range sources {
		for directive, definition := range schema.Directives {
			if allowedDirective(directive) {
				result[directive] = definition
			}
		}
	}
	return result
}

func mergePossibleTypes(sources []*ast.Schema, mergedTypes map[string]*ast.Definition) map[string][]*ast.Definition {
	result := map[string][]*ast.Definition{}
	for _, schema := range sources {
		for typeName, interfaces := range schema.PossibleTypes {
			if typeName != serviceObjectName && typeName != nodeInterfaceName {
				if _, ok := mergedTypes[typeName]; !ok {
					continue
				}
				for _, i := range interfaces {
					if i.Name != nodeInterfaceName {
						if ast.DefinitionList(result[typeName]).ForName(i.Name) == nil {
							result[typeName] = append(result[typeName], i)
						}
					}
				}
			}
		}
	}
	return result
}

func mergeNamespaceObjects(aTypes, bTypes map[string]*ast.Definition, a, b *ast.Definition) (*ast.Definition, error) {
	var fields ast.FieldList
	for _, f := range a.Fields {
		if isQueryType(a) && (isNodeField(f) || isServiceField(f) || isGraphQLBuiltinName(f.Name)) {
			continue
		}
		fields = append(fields, f)
	}
	for _, f := range mergeableFields(b) {
		if rf := fields.ForName(f.Name); rf != nil {
			if f.Type.String() == rf.Type.String() && f.Type.NonNull &&
				isNamespaceObject(aTypes[rf.Type.Name()]) && isNamespaceObject(bTypes[f.Type.Name()]) &&
				!hasIDField(aTypes[rf.Type.Name()]) && !hasIDField(bTypes[f.Type.Name()]) &&
				len(f.Arguments) == 0 && len(rf.Arguments) == 0 {
				continue
			}

			return nil, fmt.Errorf("overlapping namespace fields %s : %s", a.Name, f.Name)
		}
		fields = append(fields, f)
	}

	return &ast.Definition{
		Kind:        ast.Object,
		Description: mergeDescriptions(a, b),
		Name:        a.Name,
		Directives:  a.Directives.ForNames(namespaceDirectiveName),
		Interfaces:  append(a.Interfaces, b.Interfaces...),
		Fields:      fields,
	}, nil
}

func mergeBoundaryObjects(aTypes, bTypes map[string]*ast.Definition, a, b *ast.Definition) (*ast.Definition, error) {
	result := &ast.Definition{
		Kind:        ast.Object,
		Description: mergeDescriptions(a, b),
		Name:        a.Name,
		Directives:  a.Directives.ForNames(boundaryDirectiveName),
		Interfaces:  append(a.Interfaces, b.Interfaces...),
		Fields:      nil,
	}

	mergedFields, err := mergeBoundaryObjectFields(aTypes, bTypes, a, b)
	if err != nil {
		return nil, err
	}

	result.Fields = mergedFields
	return result, nil
}

func mergeBoundaryObjectFields(aTypes, bTypes map[string]*ast.Definition, a, b *ast.Definition) (ast.FieldList, error) {
	var result ast.FieldList
	for _, f := range a.Fields {
		if isQueryType(a) && (isNodeField(f) || isServiceField(f)) {
			continue
		}
		result = append(result, f)
	}
	for _, f := range mergeableFields(b) {
		if isIDField(f) {
			continue
		}
		if rf := result.ForName(f.Name); rf != nil {
			return nil, fmt.Errorf("overlapping fields %s : %s", a.Name, f.Name)
		}
		result = append(result, f)
	}

	return result, nil
}

func mergeableFields(t *ast.Definition) ast.FieldList {
	result := ast.FieldList{}
	for _, f := range t.Fields {
		if isGraphQLBuiltinName(f.Name) {
			continue
		}
		if isQueryType(t) && (isNodeField(f) || isServiceField(f)) {
			continue
		}
		result = append(result, f)
	}
	return result
}

func mergeDescriptions(a, b *ast.Definition) string {
	if a.Description == "" {
		return b.Description
	}
	if b.Description == "" {
		return a.Description
	}
	return a.Description + "\n\n" + b.Description
}

func cleanInterfaces(interfaces []string) []string {
	var res []string
	for _, i := range interfaces {
		if i == nodeInterfaceName {
			continue
		}
		res = append(res, i)
	}

	return res
}

func cleanDirectives(directives ast.DirectiveList) ast.DirectiveList {
	var res ast.DirectiveList
	for _, d := range directives {
		if allowedDirective(d.Name) {
			res = append(res, d)
		}
	}

	return res
}

func allowedDirective(name string) bool {
	switch name {
	case boundaryDirectiveName, namespaceDirectiveName, "skip", "include", "deprecated":
		return true
	default:
		return false
	}
}

func hasIDField(t *ast.Definition) bool {
	for _, f := range t.Fields {
		if isIDField(f) {
			return true
		}
	}

	return false
}

func isNodeField(f *ast.FieldDefinition) bool {
	if f.Name != nodeRootFieldName || len(f.Arguments) != 1 {
		return false
	}
	arg := f.Arguments[0]
	return arg.Name == idFieldName &&
		isIDType(arg.Type) &&
		isNullableTypeNamed(f.Type, nodeInterfaceName)
}

func isIDField(f *ast.FieldDefinition) bool {
	return f.Name == idFieldName && len(f.Arguments) == 0 && isIDType(f.Type)
}

func isServiceField(f *ast.FieldDefinition) bool {
	return f.Name == serviceRootFieldName &&
		len(f.Arguments) == 0 &&
		isNonNullableTypeNamed(f.Type, serviceObjectName)
}

func isQueryType(a *ast.Definition) bool {
	return a.Name == queryObjectName
}

func isBoundaryObject(a *ast.Definition) bool {
	return a.Directives.ForName(boundaryDirectiveName) != nil
}

func isNamespaceObject(a *ast.Definition) bool {
	return a.Directives.ForName(namespaceDirectiveName) != nil
}

func hasFederationDirectives(o *ast.Definition) bool {
	return isBoundaryObject(o) || isNamespaceObject(o)
}
