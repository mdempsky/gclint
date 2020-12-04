// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/analysis/singlechecker"
	"golang.org/x/tools/go/ast/inspector"
)

var flagAssign = flag.Bool("assign", false, "warn about assignments of *ir.Name to ir.Node (beware: many false positives)")

const Doc = `check for suspicious cmd/compile constructs

The gclint analyzer reports reports about uses of ir.Node that are
suspect and likely need changes to allow introducing ir.IdentExpr.`

var Analyzer = &analysis.Analyzer{
	Name:     "gclint",
	Doc:      Doc,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func main() {
	singlechecker.Main(Analyzer)
}

func run(pass *analysis.Pass) (interface{}, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inspect.WithStack(nil, func(n ast.Node, push bool, stack []ast.Node) (proceed bool) {
		if !push {
			return true
		}

		info := pass.TypesInfo

		switch n := n.(type) {
		case *ast.MapType:
			if isNodeType(info.Types[n.Key].Type) {
				pass.Reportf(n.Pos(), "map with ir.Node key (likely change key type to *ir.Name)")
			}

		case *ast.BinaryExpr:
			if n.Op == token.EQL || n.Op == token.NEQ {
				xtv := info.Types[n.X]
				ytv := info.Types[n.Y]

				// Comparison with nil is always safe.
				if xtv.IsNil() || ytv.IsNil() {
					break
				}

				// Quick hack. What we really want to
				// do here is check if *ir.Name is
				// *not* assignable to either operand.
				if isPtrToFuncType(xtv.Type) || isPtrToFuncType(ytv.Type) {
					break
				}

				if isNodeType(xtv.Type) {
					pass.Reportf(n.Pos(), "comparison of ir.Node values (replace with ir.Uses or ir.SameSource)")
				}
			}
		}

		if *flagAssign {
			if n, ok := n.(ast.Expr); ok {
				tv := info.Types[n]
				if tv.IsValue() && isPtrToNameType(tv.Type) && assignedToNode(stack, info) {
					pass.Reportf(n.Pos(), "*ir.Name assigned to ir.Node (maybe change destination to *ir.Name too)")
				}
			}
		}

		return true
	})
	return nil, nil
}

func assignedToNode(stack []ast.Node, info *types.Info) bool {
	n := stack[len(stack)-1]
	parent := stack[len(stack)-2]

	switch parent := parent.(type) {
	case *ast.AssignStmt:
		for i, rhs := range parent.Rhs {
			if rhs == n {
				lval := parent.Lhs[i]

				// Assignments to Name.Defn are okay
				// for now; they're used for linking
				// up closure variables to the outer
				// context.
				if isNameDefnField(lval, info) {
					return false
				}

				return isNodeType(info.Types[lval].Type)
			}
		}

	case *ast.CallExpr:
		tv := info.Types[parent.Fun]
		if !tv.IsValue() {
			return false
		}
		sig := tv.Type.(*types.Signature)
		nparams := sig.Params().Len()

		for i, arg := range parent.Args {
			if arg == n {
				if sig.Variadic() && !parent.Ellipsis.IsValid() && i >= nparams-1 {
					return isNodeType(sig.Params().At(nparams - 1).Type().(*types.Slice).Elem())
				}
				return isNodeType(sig.Params().At(i).Type())
			}
		}
		panic(fmt.Sprintf("didn't find %v in %v", n, parent))

	case *ast.ReturnStmt:
		typ := funcScope(stack, info).(*types.Signature).Results()
		for i, res := range parent.Results {
			if res == n {
				return isNodeType(typ.At(i).Type())
			}
		}
		panic(fmt.Sprintf("didn't find %v in %v", n, parent))
	}

	return false
}

func funcScope(stack []ast.Node, info *types.Info) types.Type {
	for i := len(stack) - 1; i >= 0; i-- {
		switch n := stack[i].(type) {
		case *ast.FuncLit:
			return info.Types[n].Type
		case *ast.FuncDecl:
			return info.Defs[n.Name].Type()
		}
	}
	panic(fmt.Sprintf("no enclosing function declaration or literal"))
}

const irPkgPath = "cmd/compile/internal/ir"

func isNameDefnField(n ast.Expr, info *types.Info) bool {
	if sel, ok := n.(*ast.SelectorExpr); ok {
		obj, ok := info.Uses[sel.Sel]
		// TODO(mdempsky): This is imprecise. Should really
		// check that obj is a field declared within ir.Name.
		return ok && isNamedObject(obj, irPkgPath, "Defn")
	}
	return false
}

func isPtrToFuncType(typ types.Type) bool { return isPtrToNamedType(typ, irPkgPath, "Func") }
func isPtrToNameType(typ types.Type) bool { return isPtrToNamedType(typ, irPkgPath, "Name") }
func isNodeType(typ types.Type) bool      { return isNamedType(typ, irPkgPath, "Node") }

func isPtrToNamedType(typ types.Type, pkgPath, name string) bool {
	ptr, ok := typ.(*types.Pointer)
	return ok && isNamedType(ptr.Elem(), pkgPath, name)
}

func isNamedType(typ types.Type, pkgPath, name string) bool {
	named, ok := typ.(*types.Named)
	return ok && isNamedObject(named.Obj(), pkgPath, name)
}

func isNamedObject(obj types.Object, pkgPath, name string) bool {
	pkg := obj.Pkg()
	return pkg != nil && pkg.Path() == pkgPath && obj.Name() == name
}
