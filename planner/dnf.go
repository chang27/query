//  Copyright (c) 2014 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package planner

import (
	"github.com/couchbase/query/expression"
)

type DNF struct {
	expression.MapperBase
	expr         expression.Expression
	dnfTermCount int
}

func NewDNF(expr expression.Expression) *DNF {
	rv := &DNF{
		expr: expr,
	}
	rv.SetMapper(rv)
	return rv
}

func (this *DNF) VisitBetween(expr *expression.Between) (interface{}, error) {
	err := expr.MapChildren(this)
	if err != nil {
		return nil, err
	}

	return expression.NewAnd(expression.NewGE(expr.First(), expr.Second()),
		expression.NewLE(expr.First(), expr.Third())), nil
}

/*
Convert to Disjunctive Normal Form.

Convert ANDs of ORs to ORs of ANDs. For example:

(A OR B) AND C => (A AND C) OR (B AND C)

Also flatten and apply constant folding.
*/
func (this *DNF) VisitAnd(expr *expression.And) (interface{}, error) {
	// Flatten nested ANDs
	var truth bool
	expr, truth = flattenAnd(expr)
	if !truth {
		return expression.FALSE_EXPR, nil
	}

	err := expr.MapChildren(this)
	if err != nil {
		return nil, err
	}

	// Flatten nested ANDs
	expr, _ = flattenAnd(expr)

	if len(expr.Operands()) == 0 {
		return expression.TRUE_EXPR, nil
	}

	// DNF
	return this.applyDNF(expr), nil
}

/*
Flatten and apply constant folding.
*/
func (this *DNF) VisitOr(expr *expression.Or) (interface{}, error) {
	// Flatten nested ORs
	var truth bool
	expr, truth = flattenOr(expr)
	if truth {
		return expression.TRUE_EXPR, nil
	}

	err := expr.MapChildren(this)
	if err != nil {
		return nil, err
	}

	// Flatten nested ORs
	expr, _ = flattenOr(expr)

	if len(expr.Operands()) == 0 {
		return expression.FALSE_EXPR, nil
	}

	return expr, nil
}

/*
Apply DeMorgan's laws and other transformations.
*/
func (this *DNF) VisitNot(expr *expression.Not) (interface{}, error) {
	err := expr.MapChildren(this)
	if err != nil {
		return nil, err
	}

	var exp expression.Expression = expr

	switch operand := expr.Operand().(type) {
	case *expression.Not:
		return operand.Operand(), nil
	case *expression.And:
		operands := make(expression.Expressions, len(operand.Operands()))
		for i, op := range operand.Operands() {
			operands[i] = expression.NewNot(op)
		}

		or := expression.NewOr(operands...)
		return this.VisitOr(or)
	case *expression.Or:
		operands := make(expression.Expressions, len(operand.Operands()))
		for i, op := range operand.Operands() {
			operands[i] = expression.NewNot(op)
		}

		and := expression.NewAnd(operands...)
		return this.VisitAnd(and)
	case *expression.Eq:
		exp = expression.NewOr(expression.NewLT(operand.First(), operand.Second()),
			expression.NewLT(operand.Second(), operand.First()))
	case *expression.LT:
		exp = expression.NewLE(operand.Second(), operand.First())
	case *expression.LE:
		exp = expression.NewLT(operand.Second(), operand.First())
	default:
		return expr, nil
	}

	return exp, exp.MapChildren(this)
}

var _EMPTY_OBJECT_EXPR = expression.NewConstant(map[string]interface{}{})
var _MIN_BINARY_EXPR = expression.NewConstant([]byte{})

func (this *DNF) VisitFunction(expr expression.Function) (interface{}, error) {
	var exp expression.Expression = expr

	switch expr := expr.(type) {
	case *expression.IsBoolean:
		exp = expression.NewLE(expr.Operand(), expression.TRUE_EXPR)
	case *expression.IsNumber:
		exp = expression.NewAnd(
			expression.NewGT(expr.Operand(), expression.TRUE_EXPR),
			expression.NewLT(expr.Operand(), expression.EMPTY_STRING_EXPR))
	case *expression.IsString:
		exp = expression.NewAnd(
			expression.NewGE(expr.Operand(), expression.EMPTY_STRING_EXPR),
			expression.NewLT(expr.Operand(), expression.EMPTY_ARRAY_EXPR))
	case *expression.IsArray:
		exp = expression.NewAnd(
			expression.NewGE(expr.Operand(), expression.EMPTY_ARRAY_EXPR),
			expression.NewLT(expr.Operand(), _EMPTY_OBJECT_EXPR))
	case *expression.IsObject:
		// Not equivalent to IS OBJECT. Includes BINARY values.
		exp = expression.NewGE(expr.Operand(), _EMPTY_OBJECT_EXPR)
	}

	return exp, exp.MapChildren(this)
}

func flattenOr(or *expression.Or) (*expression.Or, bool) {
	length, flatten, truth := orLength(or)
	if !flatten || truth {
		return or, truth
	}

	buffer := make(expression.Expressions, 0, length)
	return expression.NewOr(orTerms(or, buffer)...), false
}

func flattenAnd(and *expression.And) (*expression.And, bool) {
	length, flatten, truth := andLength(and)
	if !flatten || !truth {
		return and, truth
	}

	buffer := make(expression.Expressions, 0, length)
	return expression.NewAnd(andTerms(and, buffer)...), true
}

func orLength(or *expression.Or) (length int, flatten, truth bool) {
	l := 0
	for _, op := range or.Operands() {
		switch op := op.(type) {
		case *expression.Or:
			l, _, truth = orLength(op)
			if truth {
				return
			}
			length += l
			flatten = true
		default:
			val := op.Value()
			if val != nil {
				if val.Truth() {
					truth = true
					return
				}
			} else {
				length++
			}
		}
	}

	return
}

func andLength(and *expression.And) (length int, flatten, truth bool) {
	truth = true
	l := 0
	for _, op := range and.Operands() {
		switch op := op.(type) {
		case *expression.And:
			l, _, truth = andLength(op)
			if !truth {
				return
			}
			length += l
			flatten = true
		default:
			val := op.Value()
			if val != nil {
				if !val.Truth() {
					truth = false
					return
				}
			} else {
				length++
			}
		}
	}

	return
}

func orTerms(or *expression.Or, buffer expression.Expressions) expression.Expressions {
	for _, op := range or.Operands() {
		switch op := op.(type) {
		case *expression.Or:
			buffer = orTerms(op, buffer)
		default:
			val := op.Value()
			if val == nil || val.Truth() {
				buffer = append(buffer, op)
			}
		}
	}

	return buffer
}

func andTerms(and *expression.And, buffer expression.Expressions) expression.Expressions {
	for _, op := range and.Operands() {
		switch op := op.(type) {
		case *expression.And:
			buffer = andTerms(op, buffer)
		default:
			val := op.Value()
			if val == nil || !val.Truth() {
				buffer = append(buffer, op)
			}
		}
	}

	return buffer
}

/*
Bounded DNF, to avoid exponential worst-case.

Internally apply Disjunctive Normal Form.

Convert ANDs of ORs to ORs of ANDs. For example:

(A OR B) AND C => (A AND C) OR (B AND C)
*/
func (this *DNF) applyDNF(expr *expression.And) expression.Expression {
	if this.dnfTermCount >= _MAX_DNF_COMPLEXITY {
		return expr
	}

	complexity := dnfComplexity(expr, _MAX_DNF_COMPLEXITY-this.dnfTermCount)
	if complexity <= 1 || this.dnfTermCount+complexity > _MAX_DNF_COMPLEXITY {
		return expr
	}

	this.dnfTermCount += complexity

	matrix := _EXPRESSIONS_POOL.Get()
	defer _EXPRESSIONS_POOL.Put(matrix)
	matrix = append(matrix, make(expression.Expressions, 0, len(expr.Operands())))

	var exprs2 expression.Expressions
	for _, term := range expr.Operands() {
		switch term := term.(type) {
		case *expression.Or:
			matrix2 := _EXPRESSIONS_POOL.Get()
			defer _EXPRESSIONS_POOL.Put(matrix2)

			orTerms := term.Operands()
			for _, exprs := range matrix {
				for i, orTerm := range orTerms {
					if i == len(orTerms)-1 {
						exprs2 = exprs
					} else {
						exprs2 = make(expression.Expressions, len(exprs), cap(exprs))
						copy(exprs2, exprs)
					}

					switch orTerm := orTerm.(type) {
					case *expression.And:
						// flatten any nested AND
						for _, t := range orTerm.Operands() {
							exprs2 = append(exprs2, t)
						}
					default:
						exprs2 = append(exprs2, orTerm)
					}

					matrix2 = append(matrix2, exprs2)
				}
			}

			matrix = matrix2
		default:
			for i, _ := range matrix {
				matrix[i] = append(matrix[i], term)
			}
		}
	}

	terms := make(expression.Expressions, 0, len(matrix))
	for _, exprs := range matrix {
		if len(exprs) == 1 {
			terms = append(terms, exprs[0])
		} else {
			terms = append(terms, expression.NewAnd(exprs...))
		}
	}

	return expression.NewOr(terms...)
}

func dnfComplexity(expr *expression.And, max int) int {
	comp := 1
	for _, op := range expr.Operands() {
		switch op := op.(type) {
		case *expression.Or:
			comp *= len(op.Operands())
			if comp > max {
				break
			}
		}
	}

	return comp
}

const _MAX_DNF_COMPLEXITY = 1024

var _EXPRESSIONS_POOL = expression.NewExpressionsPool(_MAX_DNF_COMPLEXITY)
