// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/terror"
)

// Optimize does optimization and creates a Plan.
// The node must be prepared first.
func Optimize(ctx context.Context, node ast.Node, sb SubQueryBuilder, is infoschema.InfoSchema) (Plan, error) {
	// We have to infer type again because after parameter is set, the expression type may change.
	if err := InferType(node); err != nil {
		return nil, errors.Trace(err)
	}
	if _, ok := node.(*ast.SelectStmt); !ok || !UseNewPlanner {
		if err := logicOptimize(ctx, node); err != nil {
			return nil, errors.Trace(err)
		}
	}
	builder := &planBuilder{
		sb:        sb,
		ctx:       ctx,
		is:        is,
		colMapper: make(map[*ast.ColumnNameExpr]int),
		allocator: new(idAllocator)}
	p := builder.build(node)
	if builder.err != nil {
		return nil, errors.Trace(builder.err)
	}
	if logic, ok := p.(LogicalPlan); UseNewPlanner && ok {
		var err error
		_, logic, err = logic.PredicatePushDown(nil)
		if err != nil {
			return nil, errors.Trace(err)
		}
		_, err = logic.PruneColumnsAndResolveIndices(p.GetSchema())
		if err != nil {
			return nil, errors.Trace(err)
		}
		_, res, _, err := logic.convert2PhysicalPlan(nil)
		if err != nil {
			return nil, errors.Trace(err)
		}
		p = res.p.PushLimit(nil)
		log.Debugf("[PLAN] %s", ToString(p))
		return p, nil
	}
	err := Refine(p)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return p, nil
}

// PrepareStmt prepares a raw statement parsed from parser.
// The statement must be prepared before it can be passed to optimize function.
// We pass InfoSchema instead of getting from Context in case it is changed after resolving name.
func PrepareStmt(is infoschema.InfoSchema, ctx context.Context, node ast.Node) error {
	ast.SetFlag(node)
	if err := Preprocess(node, is, ctx); err != nil {
		return errors.Trace(err)
	}
	if err := Validate(node, true); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// Optimizer error codes.
const (
	CodeOneColumn           terror.ErrCode = 1
	CodeSameColumns         terror.ErrCode = 2
	CodeMultiWildCard       terror.ErrCode = 3
	CodeUnsupported         terror.ErrCode = 4
	CodeInvalidGroupFuncUse terror.ErrCode = 5
	CodeIllegalReference    terror.ErrCode = 6
)

// Optimizer base errors.
var (
	ErrOneColumn           = terror.ClassOptimizer.New(CodeOneColumn, "Operand should contain 1 column(s)")
	ErrSameColumns         = terror.ClassOptimizer.New(CodeSameColumns, "Operands should contain same columns")
	ErrMultiWildCard       = terror.ClassOptimizer.New(CodeMultiWildCard, "wildcard field exist more than once")
	ErrUnSupported         = terror.ClassOptimizer.New(CodeUnsupported, "unsupported")
	ErrInvalidGroupFuncUse = terror.ClassOptimizer.New(CodeInvalidGroupFuncUse, "Invalid use of group function")
	ErrIllegalReference    = terror.ClassOptimizer.New(CodeIllegalReference, "Illegal reference")
)

func init() {
	mySQLErrCodes := map[terror.ErrCode]uint16{
		CodeOneColumn:           mysql.ErrOperandColumns,
		CodeSameColumns:         mysql.ErrOperandColumns,
		CodeMultiWildCard:       mysql.ErrParse,
		CodeInvalidGroupFuncUse: mysql.ErrInvalidGroupFuncUse,
		CodeIllegalReference:    mysql.ErrIllegalReference,
	}
	terror.ErrClassToMySQLCodes[terror.ClassOptimizer] = mySQLErrCodes
}
