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
	"github.com/couchbaselabs/query/expression"
	"github.com/couchbaselabs/query/value"
)

type sargableComp struct {
	predicate
}

func newSargableComp(expr expression.BinaryFunction) *sargableComp {
	rv := &sargableComp{}
	rv.test = func(expr2 expression.Expression) (bool, error) {
		var cv value.Value

		if expr.First().EquivalentTo(expr2) {
			cv = expr.Second().Value()
		} else if expr.Second().EquivalentTo(expr2) {
			cv = expr.First().Value()
		}

		return cv != nil && cv.Type() > value.NULL, nil
	}

	return rv
}