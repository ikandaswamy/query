//  Copyright (c) 2014 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package algebra

import (
	"github.com/couchbase/query/auth"
	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/expression"
)

type SubqueryTerm struct {
	subquery *Select
	as       string
	joinHint JoinHint
	property uint32
}

/*
Constructor.
*/
func NewSubqueryTerm(subquery *Select, as string, joinHint JoinHint) *SubqueryTerm {
	return &SubqueryTerm{subquery, as, joinHint, 0}
}

/*
Visitor pattern.
*/
func (this *SubqueryTerm) Accept(visitor NodeVisitor) (interface{}, error) {
	return visitor.VisitSubqueryTerm(this)
}

/*
Apply mapping to all contained Expressions.
*/
func (this *SubqueryTerm) MapExpressions(mapper expression.Mapper) (err error) {
	return this.subquery.MapExpressions(mapper)
}

/*
   Returns all contained Expressions.
*/
func (this *SubqueryTerm) Expressions() expression.Expressions {
	return this.subquery.Expressions()
}

/*
Returns all required privileges.
*/
func (this *SubqueryTerm) Privileges() (*auth.Privileges, errors.Error) {
	return this.subquery.Privileges()
}

/*
   Representation as a N1QL string.
*/
func (this *SubqueryTerm) String() string {
	return "(" + this.subquery.String() + ") as " + this.as
}

/*
Qualify all identifiers for the parent expression. Checks for
duplicate aliases.
*/
func (this *SubqueryTerm) Formalize(parent *expression.Formalizer) (f *expression.Formalizer, err error) {
	alias := this.Alias()
	if alias == "" {
		err = errors.NewNoTermNameError("FROM Subquery", "semantics.subquery.requires_name_or_alias")
		return
	}

	_, ok := parent.Allowed().Field(alias)
	if ok {
		err = errors.NewDuplicateAliasError("subquery", alias, "semantics.subquery.duplicate_alias")
		return nil, err
	}

	f = expression.NewFormalizer(alias, parent)
	if this.IsAnsiJoinOp() {
		// If on right-hand side of ANSI JOIN, check correlation
		err = this.subquery.FormalizeSubquery(f)
		if err != nil {
			return
		}
	} else {
		err = this.subquery.Formalize()
		if err != nil {
			return
		}
	}

	f.SetAlias(this.Alias())
	return
}

/*
Return the primary term in the from clause.
*/
func (this *SubqueryTerm) PrimaryTerm() FromTerm {
	return this
}

/*
Returns the Alias string.
*/
func (this *SubqueryTerm) Alias() string {
	return this.as
}

/*
Returns the inner subquery.
*/
func (this *SubqueryTerm) Subquery() *Select {
	return this.subquery
}

/*
Returns the join hint
*/
func (this *SubqueryTerm) JoinHint() JoinHint {
	return this.joinHint
}

/*
Join hint prefers hash join
*/
func (this *SubqueryTerm) PreferHash() bool {
	return this.joinHint == USE_HASH_BUILD || this.joinHint == USE_HASH_PROBE
}

/*
Join hint prefers nested loop join
*/
func (this *SubqueryTerm) PreferNL() bool {
	return this.joinHint == USE_NL
}

/*
Returns the property.
*/
func (this *SubqueryTerm) Property() uint32 {
	return this.property
}

/*
Returns whether this subquery term is for an ANSI JOIN
*/
func (this *SubqueryTerm) IsAnsiJoin() bool {
	return (this.property & TERM_ANSI_JOIN) != 0
}

/*
Returns whether this subquery term is for an ANSI NEST
*/
func (this *SubqueryTerm) IsAnsiNest() bool {
	return (this.property & TERM_ANSI_NEST) != 0
}

/*
Returns whether this subquery term is for an ANSI JOIN or ANSI NEST
*/
func (this *SubqueryTerm) IsAnsiJoinOp() bool {
	return (this.property & (TERM_ANSI_JOIN | TERM_ANSI_NEST)) != 0
}

/*
Set join hint
*/
func (this *SubqueryTerm) SetJoinHint(joinHint JoinHint) {
	this.joinHint = joinHint
}

/*
Set ANSI JOIN property
*/
func (this *SubqueryTerm) SetAnsiJoin() {
	this.property |= TERM_ANSI_JOIN
}

/*
Set ANSI NEST property
*/
func (this *SubqueryTerm) SetAnsiNest() {
	this.property |= TERM_ANSI_NEST
}
