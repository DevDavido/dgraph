/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package schema

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dgraph-io/dgraph/x"
	"github.com/vektah/gqlparser/ast"
)

// Wrap the github.com/vektah/gqlparser/ast defintions so that the bulk of the GraphQL
// algorithm and interface is dependent on behaviours we expect from a GraphQL schema
// and validation, but not dependent the exact structure in the gqlparser.
//
// This also auto hooks up some bookkeeping that's otherwise no fun.  E.g. getting values for
// field arguments requires the variable map from the operation - so we'd need to carry vars
// through all the resolver functions.  Much nicer if they are resolved by magic here.

// QueryType is currently supported queries
type QueryType string

// MutationType is currently supported mutations
type MutationType string

// Query/Mutation types and arg names
const (
	GetQuery             QueryType    = "get"
	FilterQuery          QueryType    = "query"
	SchemaQuery          QueryType    = "schema"
	NotSupportedQuery    QueryType    = "notsupported"
	AddMutation          MutationType = "add"
	UpdateMutation       MutationType = "update"
	DeleteMutation       MutationType = "delete"
	NotSupportedMutation MutationType = "notsupported"
	IDType                            = "ID"
	IDArgName                         = "id"
	InputArgName                      = "input"
)

// Schema represents a valid GraphQL schema
type Schema interface {
	Operation(r *Request) (Operation, error)
}

// An Operation is a single valid GraphQL operation.  It contains either
// Queries or Mutations, but not both.  Subscriptions are not yet supported.
type Operation interface {
	Queries() []Query
	Mutations() []Mutation
	Schema() Schema
	IsQuery() bool
	IsMutation() bool
	IsSubscription() bool
}

// A Field is one field from an Operation.
type Field interface {
	Name() string
	Alias() string
	ResponseName() string
	ArgValue(name string) interface{}
	IDArgValue() (uint64, error)
	Skip() bool
	Include() bool
	Type() Type
	SelectionSet() []Field
	Location() x.Location
	DgraphPredicate() string
	Operation() Operation
	// InterfaceType tells us whether this field represents a GraphQL Interface.
	InterfaceType() bool
	ConcreteType(types []interface{}) string
}

// A Mutation is a field (from the schema's Mutation type) from an Operation
type Mutation interface {
	Field
	MutationType() MutationType
	MutatedType() Type
	QueryField() Field
}

// A Query is a field (from the schema's Query type) from an Operation
type Query interface {
	Field
	QueryType() QueryType
}

// A Type is a GraphQL type like: Float, T, T! and [T!]!.  If it's not a list, then
// ListType is nil.  If it's an object type then Field gets field definitions by
// name from the definition of the type; IDField gets the ID field of the type.
type Type interface {
	Field(name string) FieldDefinition
	IDField() FieldDefinition
	Name() string
	DgraphPredicate(fld string) string
	Nullable() bool
	ListType() Type
	Interfaces() []string
	fmt.Stringer
}

// A FieldDefinition is a field as defined in some Type in the schema.  As opposed
// to a Field, which is an instance of a query or mutation asking for a field
// (which in turn must have a FieldDefinition of the right type in the schema.)
type FieldDefinition interface {
	Name() string
	Type() Type
	IsID() bool
	Inverse() (Type, FieldDefinition)
}

type astType struct {
	typ             *ast.Type
	inSchema        *ast.Schema
	dgraphPredicate map[string]map[string]string
}

type schema struct {
	schema *ast.Schema
	// dgraphPredicate gives us the dgraph predicate corresponding to a typeName + fieldName.
	// It is pre-computed so that runtime queries and mutations can look it
	// up quickly.
	// The key for the first map are the type names. The second map has a mapping of the
	// fieldName => dgraphPredicate.
	dgraphPredicate map[string]map[string]string
	// Map of mutation field name to mutated type.
	mutatedType map[string]*astType
}

type operation struct {
	op   *ast.OperationDefinition
	vars map[string]interface{}

	// The fields below are used by schema introspection queries.
	query    string
	doc      *ast.QueryDocument
	inSchema *schema
}

type field struct {
	field *ast.Field
	op    *operation
	sel   ast.Selection
	// arguments contains the computed values for arguments taking into account the values
	// for the GraphQL variables supplied in the query.
	arguments map[string]interface{}
}

type fieldDefinition struct {
	fieldDef        *ast.FieldDefinition
	inSchema        *ast.Schema
	dgraphPredicate map[string]map[string]string
}

type mutation field
type query field

func (o *operation) IsQuery() bool {
	return o.op.Operation == ast.Query
}

func (o *operation) IsMutation() bool {
	return o.op.Operation == ast.Mutation
}

func (o *operation) IsSubscription() bool {
	return o.op.Operation == ast.Subscription
}

func (o *operation) Schema() Schema {
	return o.inSchema
}

func (o *operation) Queries() (qs []Query) {
	if !o.IsQuery() {
		return
	}

	for _, s := range o.op.SelectionSet {
		if f, ok := s.(*ast.Field); ok {
			qs = append(qs, &query{field: f, op: o, sel: s})
		}
	}

	return
}

func (o *operation) Mutations() (ms []Mutation) {
	if !o.IsMutation() {
		return
	}

	for _, s := range o.op.SelectionSet {
		if f, ok := s.(*ast.Field); ok {
			ms = append(ms, &mutation{field: f, op: o})
		}
	}

	return
}

// parentInterface returns the name of an interface that a field belonging to a type definition
// typDef inherited from. If there is no such interface, then it returns an empty string.
//
// Given the following schema
// interface A {
//   name: String
// }
//
// type B implements A {
//	 name: String
//   age: Int
// }
//
// calling parentInterface on the fieldName name with type definition for B, would return A.
func parentInterface(sch *ast.Schema, typDef *ast.Definition, fieldName string) string {
	if len(typDef.Interfaces) == 0 {
		return ""
	}

	for _, iface := range typDef.Interfaces {
		interfaceDef := sch.Types[iface]
		for _, interfaceField := range interfaceDef.Fields {
			if fieldName == interfaceField.Name {
				return iface
			}
		}
	}
	return ""
}

func dgraphMapping(sch *ast.Schema) map[string]map[string]string {
	const (
		del     = "Delete"
		payload = "Payload"
	)

	dgraphPredicate := make(map[string]map[string]string)
	for _, inputTyp := range sch.Types {
		// We only want to consider input types (object and interface) defined by the user as part
		// of the schema hence we ignore BuiltIn, query and mutation types.
		if inputTyp.BuiltIn || inputTyp.Name == "query" || inputTyp.Name == "mutation" ||
			(inputTyp.Kind != ast.Object && inputTyp.Kind != ast.Interface) {
			continue
		}

		originalTyp := inputTyp
		dgraphPredicate[originalTyp.Name] = make(map[string]string)
		inputTypeName := inputTyp.Name

		if strings.HasPrefix(inputTypeName, del) && strings.HasSuffix(inputTypeName, payload) {
			// For DeleteTypePayload, inputTyp should be Type.
			inputTypeName = strings.TrimSuffix(strings.TrimPrefix(inputTypeName, del), payload)
			inputTyp = sch.Types[inputTypeName]
		}

		for _, fld := range inputTyp.Fields {
			edgeName := fld.Name
			// 1. For types the keys, value would be.
			//    typName,fldName => fldName
			// 2. For DeleteTypePayload type
			//    DeleteTypePayload,fldName => typName.fldName
			dgraphPredicate[originalTyp.Name][fld.Name] = edgeName
		}
	}
	return dgraphPredicate
}

func mutatedTypeMapping(s *ast.Schema,
	dgraphPredicate map[string]map[string]string) map[string]*astType {
	if s.Mutation == nil {
		return nil
	}

	m := make(map[string]*astType, len(s.Mutation.Fields))
	for _, field := range s.Mutation.Fields {
		mutatedTypeName := ""
		switch {
		case strings.HasPrefix(field.Name, "add"):
			mutatedTypeName = strings.TrimPrefix(field.Name, "add")
		case strings.HasPrefix(field.Name, "update"):
			mutatedTypeName = strings.TrimPrefix(field.Name, "update")
		case strings.HasPrefix(field.Name, "delete"):
			mutatedTypeName = strings.TrimPrefix(field.Name, "delete")
		default:
		}
		// This is a convoluted way of getting the type for mutatedTypeName. We get the definition
		// for UpdateTPayload and get the type from the first field. There is no direct way to get
		// the type from the definition of an object. We use Update and not Add here because
		// Interfaces only have Update.
		def := s.Types["Update"+mutatedTypeName+"Payload"]

		if def == nil {
			continue
		}

		// Accessing 0th element should be safe to do as according to the spec an object must define
		// one or more fields.
		typ := def.Fields[0].Type
		// This would contain mapping of mutation field name to the Type()
		// for e.g. addPost => astType for Post
		m[field.Name] = &astType{typ, s, dgraphPredicate}
	}
	return m
}

// AsSchema wraps a github.com/vektah/gqlparser/ast.Schema.
func AsSchema(s *ast.Schema) Schema {
	dgraphPredicate := dgraphMapping(s)
	return &schema{
		schema:          s,
		dgraphPredicate: dgraphPredicate,
		mutatedType:     mutatedTypeMapping(s, dgraphPredicate),
	}
}

func responseName(f *ast.Field) string {
	if f.Alias == "" {
		return f.Name
	}
	return f.Alias
}

func (f *field) Name() string {
	return f.field.Name
}

func (f *field) Alias() string {
	return f.field.Alias
}

func (f *field) ResponseName() string {
	return responseName(f.field)
}

func (f *field) ArgValue(name string) interface{} {
	if f.arguments == nil {
		// Compute and cache the map first time this function is called for a field.
		f.arguments = f.field.ArgumentMap(f.op.vars)
	}
	return f.arguments[name]
}

func (f *field) Skip() bool {
	dir := f.field.Directives.ForName("skip")
	if dir == nil {
		return false
	}
	return dir.ArgumentMap(f.op.vars)["if"].(bool)
}

func (f *field) Include() bool {
	dir := f.field.Directives.ForName("include")
	if dir == nil {
		return true
	}
	return dir.ArgumentMap(f.op.vars)["if"].(bool)
}

func (f *field) IDArgValue() (uint64, error) {
	idArg := f.ArgValue(IDArgName)
	if idArg == nil {
		pos := f.field.GetPosition()
		return 0,
			x.GqlErrorf("ID argument not available on field %s", f.Name()).
				WithLocations(x.Location{Line: pos.Line, Column: pos.Column})
	}

	id, ok := idArg.(string)
	uid, err := strconv.ParseUint(id, 0, 64)

	if !ok || err != nil {
		pos := f.field.GetPosition()
		err = x.GqlErrorf("ID argument (%s) of %s was not able to be parsed", id, f.Name()).
			WithLocations(x.Location{Line: pos.Line, Column: pos.Column})
	}

	return uid, err
}

func (f *field) Type() Type {
	return &astType{
		typ:             f.field.Definition.Type,
		inSchema:        f.op.inSchema.schema,
		dgraphPredicate: f.op.inSchema.dgraphPredicate,
	}
}

func (f *field) InterfaceType() bool {
	return f.op.inSchema.schema.Types[f.field.Definition.Type.Name()].Kind == ast.Interface
}

func (f *field) SelectionSet() (flds []Field) {
	for _, s := range f.field.SelectionSet {
		if fld, ok := s.(*ast.Field); ok {
			flds = append(flds, &field{
				field: fld,
				op:    f.op,
			})
		}
		if fragment, ok := s.(*ast.InlineFragment); ok {
			// This is the case where an inline fragment is defined within a query
			// block. Usually this is for requesting some fields for a concrete type
			// within a query for an interface.
			for _, s := range fragment.SelectionSet {
				if fld, ok := s.(*ast.Field); ok {
					flds = append(flds, &field{
						field: fld,
						op:    f.op,
					})
				}
			}
		}
	}

	return
}

func (f *field) Location() x.Location {
	return x.Location{
		Line:   f.field.Position.Line,
		Column: f.field.Position.Column}
}

func (f *field) Operation() Operation {
	return f.op
}

func (f *field) DgraphPredicate() string {
	return f.op.inSchema.dgraphPredicate[f.field.ObjectDefinition.Name][f.Name()]
}

func (f *field) ConcreteType(dgraphTypes []interface{}) string {
	// Given a list of dgraph types, we query the schema and find the one which is an ast.Object
	// and not an Interface object.
	for _, typ := range dgraphTypes {
		styp, ok := typ.(string)
		if !ok {
			continue
		}
		if f.op.inSchema.schema.Types[styp].Kind == ast.Object {
			return styp
		}
	}
	return ""
}

func (q *query) Name() string {
	return (*field)(q).Name()
}

func (q *query) Alias() string {
	return (*field)(q).Alias()
}

func (q *query) ArgValue(name string) interface{} {
	return (*field)(q).ArgValue(name)
}

func (q *query) Skip() bool {
	return false
}

func (q *query) Include() bool {
	return true
}

func (q *query) IDArgValue() (uint64, error) {
	return (*field)(q).IDArgValue()
}

func (q *query) Type() Type {
	return (*field)(q).Type()
}

func (q *query) SelectionSet() []Field {
	return (*field)(q).SelectionSet()
}

func (q *query) Location() x.Location {
	return (*field)(q).Location()
}

func (q *query) ResponseName() string {
	return (*field)(q).ResponseName()
}

func (q *query) QueryType() QueryType {
	switch {
	case strings.HasPrefix(q.Name(), "get"):
		return GetQuery
	case q.Name() == "__schema" || q.Name() == "__type":
		return SchemaQuery
	case strings.HasPrefix(q.Name(), "query"):
		return FilterQuery
	default:
		return NotSupportedQuery
	}
}

func (q *query) Operation() Operation {
	return (*field)(q).Operation()
}

func (q *query) DgraphPredicate() string {
	return (*field)(q).DgraphPredicate()
}

func (q *query) InterfaceType() bool {
	return (*field)(q).InterfaceType()
}

func (q *query) ConcreteType(dgraphTypes []interface{}) string {
	return (*field)(q).ConcreteType(dgraphTypes)
}

func (m *mutation) Name() string {
	return (*field)(m).Name()
}

func (m *mutation) Alias() string {
	return (*field)(m).Alias()
}

func (m *mutation) ArgValue(name string) interface{} {
	return (*field)(m).ArgValue(name)
}

func (m *mutation) Skip() bool {
	return false
}

func (m *mutation) Include() bool {
	return true
}

func (m *mutation) Type() Type {
	return (*field)(m).Type()
}

func (m *mutation) InterfaceType() bool {
	return (*field)(m).InterfaceType()
}

func (m *mutation) IDArgValue() (uint64, error) {
	return (*field)(m).IDArgValue()
}

func (m *mutation) SelectionSet() []Field {
	return (*field)(m).SelectionSet()
}

func (m *mutation) QueryField() Field {
	// TODO: All our mutations currently have exactly 1 field, but that will change
	// - need a better way to get the right field by convention.
	return m.SelectionSet()[0]
}

func (m *mutation) Location() x.Location {
	return (*field)(m).Location()
}

func (m *mutation) ResponseName() string {
	return (*field)(m).ResponseName()
}

// MutatedType returns the underlying type that gets mutated by m.
//
// It's not the same as the response type of m because mutations don't directly
// return what they mutate.  Mutations return a payload package that includes
// the actual node mutated as a field.
func (m *mutation) MutatedType() Type {
	// ATM there's a single field in the mutation payload.
	return m.op.inSchema.mutatedType[m.Name()]
}

func (m *mutation) MutationType() MutationType {
	switch {
	case strings.HasPrefix(m.Name(), "add"):
		return AddMutation
	case strings.HasPrefix(m.Name(), "update"):
		return UpdateMutation
	case strings.HasPrefix(m.Name(), "delete"):
		return DeleteMutation
	default:
		return NotSupportedMutation
	}
}

func (m *mutation) Operation() Operation {
	return (*field)(m).Operation()
}

func (m *mutation) DgraphPredicate() string {
	return (*field)(m).DgraphPredicate()
}

func (m *mutation) ConcreteType(dgraphTypes []interface{}) string {
	return (*field)(m).ConcreteType(dgraphTypes)
}

func (t *astType) Field(name string) FieldDefinition {
	return &fieldDefinition{
		// this ForName lookup is a loop in the underlying schema :-(
		fieldDef:        t.inSchema.Types[t.Name()].Fields.ForName(name),
		inSchema:        t.inSchema,
		dgraphPredicate: t.dgraphPredicate,
	}
}

func (fd *fieldDefinition) Name() string {
	return fd.fieldDef.Name
}

func (fd *fieldDefinition) IsID() bool {
	return isID(fd.fieldDef)
}

func isID(fd *ast.FieldDefinition) bool {
	// this will change once we have more types of ID fields
	return fd.Type.Name() == "ID"
}

func (fd *fieldDefinition) Type() Type {
	return &astType{
		typ:             fd.fieldDef.Type,
		inSchema:        fd.inSchema,
		dgraphPredicate: fd.dgraphPredicate,
	}
}

func (fd *fieldDefinition) Inverse() (Type, FieldDefinition) {

	invDirective := fd.fieldDef.Directives.ForName(inverseDirective)
	if invDirective == nil {
		return nil, nil
	}

	invFieldArg := invDirective.Arguments.ForName(inverseArg)
	if invFieldArg == nil {
		return nil, nil // really not possible
	}

	// typ must exist if the schema passed GQL validation
	typ := fd.inSchema.Types[fd.Type().Name()]

	// fld must exist if the schema passed our validation
	fld := typ.Fields.ForName(invFieldArg.Value.Raw)

	return fd.Type(), &fieldDefinition{fieldDef: fld, inSchema: fd.inSchema}
}

func (t *astType) Name() string {
	if t.typ.NamedType == "" {
		return t.typ.Elem.NamedType
	}
	return t.typ.NamedType
}

func (t *astType) Nullable() bool {
	return !t.typ.NonNull
}

func (t *astType) ListType() Type {
	if t.typ.Elem == nil {
		return nil
	}
	return &astType{typ: t.typ.Elem}
}

// DgraphPredicate returns the name of the predicate in Dgraph that represents this
// type's field fld.  Mostly this will be type_name.field_name,.
func (t *astType) DgraphPredicate(fld string) string {
	return t.dgraphPredicate[t.Name()][fld]
}

func (t *astType) String() string {
	if t == nil {
		return ""
	}

	var sb strings.Builder
	// give it enough space in case it happens to be `[t.Name()!]!`
	sb.Grow(len(t.Name()) + 4)

	if t.ListType() == nil {
		sb.WriteString(t.Name())
	} else {
		// There's no lists of lists, so this needn't be recursive
		sb.WriteRune('[')
		sb.WriteString(t.Name())
		if !t.ListType().Nullable() {
			sb.WriteRune('!')
		}
		sb.WriteRune(']')
	}

	if !t.Nullable() {
		sb.WriteRune('!')
	}

	return sb.String()
}

func (t *astType) IDField() FieldDefinition {
	def := t.inSchema.Types[t.Name()]
	if def.Kind != ast.Object && def.Kind != ast.Interface {
		return nil
	}

	for _, fd := range def.Fields {
		if isID(fd) {
			return &fieldDefinition{
				fieldDef: fd,
				inSchema: t.inSchema,
			}
		}
	}

	return nil
}

func (t *astType) Interfaces() []string {
	return t.inSchema.Types[t.typ.Name()].Interfaces
}
