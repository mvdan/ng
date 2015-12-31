// Copyright 2015 The Neugram Authors. All rights reserved.
// See the LICENSE file for rights to use this source code.

package eval

import (
	"fmt"
	goimporter "go/importer"
	gotypes "go/types"
	"math/big"
	"os"
	"runtime/debug"
	"sort"

	"neugram.io/eval/gowrap"
	"neugram.io/job"
	"neugram.io/lang/expr"
	"neugram.io/lang/stmt"
	"neugram.io/lang/tipe"
	"neugram.io/lang/token"
	"neugram.io/lang/typecheck"
)

// A Variable is an addressable Value.
type Variable struct {
	// Value has the type:
	//	nil
	//	int64
	//	float32
	//	float64
	//	*big.Int
	//	*big.Float
	//
	//	*expr.FuncLiteral
	//	*Closure
	//
	//	*GoFunc
	// 	*GoPkg
	// 	*GoValue
	Value interface{} // TODO introduce a Value type
}

func (v *Variable) Assign(val interface{}) { v.Value = val }

type Assignable interface {
	Assign(v interface{})
}

type mapKey struct {
	m map[interface{}]interface{}
	k interface{}
}

func (m mapKey) Assign(val interface{}) { m.m[m.k] = val }

type Scope struct {
	Parent *Scope
	Var    map[string]*Variable // variable name -> variable
}

func (s *Scope) Lookup(name string) *Variable {
	if v := s.Var[name]; v != nil {
		return v
	}
	if s.Parent != nil {
		return s.Parent.Lookup(name)
	}
	return nil
}

var universeScope = &Scope{Var: map[string]*Variable{
	"true":  &Variable{Value: true},
	"false": &Variable{Value: false},
	"env":   &Variable{Value: make(map[interface{}]interface{})},
}}

func environ() []string {
	// TODO come up with a way to cache this.
	// If Scope used an interface for Variable, we could update
	// a copy of the slice on each write to env.
	env := universeScope.Lookup("env").Value.(map[interface{}]interface{})
	var kv []string
	for k, v := range env {
		kv = append(kv, k.(string)+"="+v.(string))
	}
	sort.Strings(kv)
	return kv
}

func zeroVariable(t tipe.Type) *Variable {
	switch t := t.(type) {
	case tipe.Basic:
		// TODO: propogate specialization context for Num
		switch t {
		case tipe.Bool:
			return &Variable{Value: false}
		case tipe.Byte:
			return &Variable{Value: byte(0)}
		case tipe.Rune:
			return &Variable{Value: rune(0)}
		//case tipe.Integer:
		//case tipe.Float:
		//case tipe.Complex:
		case tipe.String:
			return &Variable{Value: ""}
		case tipe.Int64:
			return &Variable{Value: int64(0)}
		case tipe.Float32:
			return &Variable{Value: float32(0)}
		case tipe.Float64:
			return &Variable{Value: float64(0)}
		default:
			panic(fmt.Sprintf("TODO zero Basic: %s", t))
		}
	case *tipe.Func:
		return &Variable{Value: nil}
	case *tipe.Struct:
		s := &Struct{Fields: make([]*Variable, len(t.Fields))}
		for i, ft := range t.Fields {
			s.Fields[i] = zeroVariable(ft)
		}
		return &Variable{Value: s}
	case *tipe.Methodik:
		return &Variable{Value: nil}
	// TODO _ = Type((*Table)(nil))
	case *tipe.Pointer:
		return &Variable{Value: nil}
	default:
		panic(fmt.Sprintf("don't know the zero value of type %T", t))
	}
}

type Struct struct {
	Fields []*Variable
}

type Closure struct {
	Func  *expr.FuncLiteral
	Scope *Scope
}

func New() *Program {
	p := &Program{
		Pkg: map[string]*Scope{
			"main": &Scope{
				Parent: universeScope,
				Var:    map[string]*Variable{},
			},
		},
		Types: typecheck.New(),
	}
	p.Cur = p.Pkg["main"]
	p.Types.ImportGo = p.importGo
	return p
}

type Program struct {
	Pkg       map[string]*Scope // package -> scope
	Cur       *Scope
	Types     *typecheck.Checker
	Returning bool
	Breaking  bool
}

func (p *Program) importGo(path string) (*gotypes.Package, error) {
	if gowrap.Pkgs[path] == nil {
		return nil, fmt.Errorf("neugram: Go package %q not known", path)
	}
	pkg, err := goimporter.Default().Import(path)
	if err != nil {
		return nil, err
	}
	return pkg, err
}

type CmdState struct {
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
	RunCmd func(argv []string, state CmdState) error
}

func (p *Program) evalShell(cmd interface{}, state CmdState) error {
	switch cmd := cmd.(type) {
	case *expr.ShellList:
		switch cmd.Segment {
		case expr.SegmentSemi:
			var err error
			for _, s := range cmd.List {
				err = p.evalShell(s, state)
			}
			return err
		case expr.SegmentAnd:
			for _, s := range cmd.List {
				if err := p.evalShell(s, state); err != nil {
					return err
				}
			}
			return nil
		//case expr.SegmentPipe:
		default:
			panic(fmt.Sprintf("unknown segment type %s", cmd.Segment))
		}
		// TODO SegmentPipe
		// TODO SegmentOut
		// TODO SegmentIn
	case *expr.ShellCmd:
		return state.RunCmd(cmd.Argv, state)
	default:
		panic(fmt.Sprintf("impossible shell command type: %T", cmd))
	}
}

func (p *Program) EvalShellList(s *expr.ShellList, state CmdState) error {
	return p.evalShell(s, state)
}

func (p *Program) evalPipeline(argv []string) error {
	return nil
}

func (p *Program) EvalCmd(argv []string, state CmdState) error {
	switch argv[0] {
	case "cd":
		dir := ""
		if len(argv) == 1 {
			dir = os.Getenv("HOME")
		} else {
			dir = argv[1]
		}
		if err := os.Chdir(dir); err != nil {
			return err
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "%s\n", wd)
		return nil
	case "exit", "logout":
		return fmt.Errorf("ng does not know %q, try $$", argv[0])
	default:
		j := &job.Job{
			Argv:   argv,
			Env:    environ(),
			Stdin:  state.Stdin,
			Stdout: state.Stdout,
			Stderr: state.Stderr,
		}
		if err := j.Start(); err != nil {
			return err
		}
		j.Wait() // TODO error prop?
		return nil
	}
}

func (p *Program) Eval(s stmt.Stmt) (res []interface{}, resType tipe.Type, err error) {
	defer func() {
		if x := recover(); x != nil {
			err = fmt.Errorf("ng eval panic: %v", x)
			fmt.Fprintf(os.Stderr, "%v\n", err)
			debug.PrintStack()
			res = nil
		}
	}()

	p.Types.Errs = p.Types.Errs[:0]
	resType = p.Types.Add(s)
	if len(p.Types.Errs) > 0 {
		return nil, nil, fmt.Errorf("typecheck: %v\n", p.Types.Errs[0])
	}

	res, err = p.evalStmt(s)
	if err != nil {
		return nil, nil, err
	}
	for i, v := range res {
		res[i], err = p.readVar(v)
		if err != nil {
			return nil, nil, err
		}
	}
	return res, resType, nil
}

func (p *Program) pushScope() {
	p.Cur = &Scope{
		Parent: p.Cur,
		Var:    make(map[string]*Variable),
	}
}
func (p *Program) popScope() {
	p.Cur = p.Cur.Parent
}

func (p *Program) evalStmt(s stmt.Stmt) ([]interface{}, error) {
	switch s := s.(type) {
	case *stmt.Assign:
		vars := make([]Assignable, len(s.Left))
		if s.Decl {
			for i, lhs := range s.Left {
				v := new(Variable)
				vars[i] = v
				p.Cur.Var[lhs.(*expr.Ident).Name] = v
			}
		} else {
			// TODO: order of evaluation, left-then-right,
			// or right-then-left?
			for i, lhs := range s.Left {
				v, err := p.evalExprAsAssignable(lhs)
				if err != nil {
					return nil, err
				}
				vars[i] = v
			}
		}
		vals := make([]interface{}, 0, len(s.Left))
		for _, rhs := range s.Right {
			v, err := p.evalExprAndReadVars(rhs)
			if err != nil {
				return nil, err
			}
			vals = append(vals, v...)
		}
		for i := range vars {
			vars[i].Assign(vals[i])
		}
		return nil, nil
	case *stmt.Simple:
		return p.evalExpr(s.Expr)
	case *stmt.Block:
		p.pushScope()
		defer p.popScope()
		for _, s := range s.Stmts {
			res, err := p.evalStmt(s)
			if err != nil {
				return nil, err
			}
			if p.Returning || p.Breaking {
				return res, nil
			}
		}
		return nil, nil
	case *stmt.If:
		if s.Init != nil {
			p.pushScope()
			defer p.popScope()
			if _, err := p.evalStmt(s.Init); err != nil {
				return nil, err
			}
		}
		cond, err := p.evalExprAndReadVar(s.Cond)
		if err != nil {
			return nil, err
		}
		if cond.(bool) {
			return p.evalStmt(s.Body)
		} else if s.Else != nil {
			return p.evalStmt(s.Else)
		}
		return nil, nil
	case *stmt.For:
		if s.Init != nil {
			p.pushScope()
			defer p.popScope()
			if _, err := p.evalStmt(s.Init); err != nil {
				return nil, err
			}
		}
		for {
			cond, err := p.evalExprAndReadVar(s.Cond)
			if err != nil {
				return nil, err
			}
			if !cond.(bool) {
				break
			}
			if _, err := p.evalStmt(s.Body); err != nil {
				return nil, err
			}
			if s.Post != nil {
				if _, err := p.evalStmt(s.Post); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	case *stmt.Return:
		var err error
		var res []interface{}
		if len(s.Exprs) == 1 {
			res, err = p.evalExprAndReadVars(s.Exprs[0])
		} else {
			res = make([]interface{}, len(s.Exprs))
			for i, e := range s.Exprs {
				res[i], err = p.evalExprAndReadVar(e)
				if err != nil {
					break
				}
			}
		}
		p.Returning = true
		if err != nil {
			return nil, err
		}
		return res, nil
	case *stmt.Import:
		typ := p.Types.Lookup(s.Name).Type.(*tipe.Package)
		p.Cur.Var[s.Name] = &Variable{
			Value: &GoPkg{
				Type:  typ,
				GoPkg: p.Types.GoPkgs[typ],
			},
		}
		return nil, nil
	case *stmt.TypeDecl:
		return nil, nil
	}
	if s == nil {
		return nil, fmt.Errorf("Parser.evalStmt: statement is nil")
	}
	panic(fmt.Sprintf("TODO evalStmt: %T: %s", s, s.Sexp()))
}

func (p *Program) evalExprAsAssignable(e expr.Expr) (Assignable, error) {
	switch e := e.(type) {
	case *expr.Index:
		container, err := p.evalExpr(e.Expr)
		if err != nil {
			return nil, err
		}
		if _, isMap := p.Types.Types[e.Expr].(*tipe.Map); isMap {
			kvar, err := p.evalExpr(e.Index)
			if err != nil {
				return nil, err
			}
			k, err := p.readVar(kvar[0])
			if err != nil {
				return nil, err
			}
			m := container[0].(*Variable).Value.(map[interface{}]interface{})
			return mapKey{m: m, k: k}, nil
		}
		panic("evalExprAsAssignable Index TODO")
	default:
		res, err := p.evalExpr(e)
		if err != nil {
			return nil, err
		}
		if len(res) > 1 {
			return nil, fmt.Errorf("eval: too many results for assignable %v", res)
		}
		if v, ok := res[0].(*Variable); ok {
			return v, nil
		}
		return nil, fmt.Errorf("eval: %s not assignable", e)
	}
}

func (p *Program) evalExprAndReadVars(e expr.Expr) ([]interface{}, error) {
	res, err := p.evalExpr(e)
	if err != nil {
		return nil, err
	}
	for i, v := range res {
		res[i], err = p.readVar(v)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (p *Program) evalExprAndReadVar(e expr.Expr) (interface{}, error) {
	res, err := p.evalExpr(e)
	if err != nil {
		return nil, err
	}
	if len(res) != 1 { // TODO these kinds of invariants are the job of the type checker
		return nil, fmt.Errorf("multi-valued (%d) expression in single-value context", len(res))
	}
	return p.readVar(res[0])
}

func (p *Program) readVar(e interface{}) (interface{}, error) {
	switch v := e.(type) {
	case *expr.FuncLiteral, *GoFunc, *Closure:
		// lack of symmetry with BasicLiteral is unfortunate
		return v, nil
	case *expr.BasicLiteral:
		return v.Value, nil
	case *Variable:
		return v.Value, nil
	case bool, int64, float32, float64, *big.Int, *big.Float:
		return v, nil
	case int, string: // TODO: are these all GoValues now?
		return v, nil
	case *GoValue:
		return v.Value, nil
	case *Struct:
		return v, nil
	case map[interface{}]interface{}:
		return v, nil
	default:
		return nil, fmt.Errorf("unexpected type %T for value", v)
	}
}

func (p *Program) callClosure(c *Closure, args []interface{}) ([]interface{}, error) {
	oldCur := p.Cur
	p.Cur = c.Scope
	defer func() { p.Cur = oldCur }()
	return p.callFuncLiteral(c.Func, args)
}

func (p *Program) callFuncLiteral(f *expr.FuncLiteral, args []interface{}) ([]interface{}, error) {
	p.pushScope()
	defer p.popScope()

	if f.Type.Variadic {
		return nil, fmt.Errorf("TODO call FuncLiteral with variadic args")
	} else {
		for i, name := range f.ParamNames {
			v := &Variable{Value: args[i]}
			p.Cur.Var[name] = v
		}
	}

	res, err := p.evalStmt(f.Body.(*stmt.Block))
	if err != nil {
		return nil, err
	}
	if p.Returning {
		p.Returning = false
	} else if len(f.ResultNames) > 0 {
		return nil, fmt.Errorf("missing return %v", f.ResultNames)
	}
	return res, nil
}

func (p *Program) evalExpr(e expr.Expr) ([]interface{}, error) {
	switch e := e.(type) {
	case *expr.BasicLiteral:
		return []interface{}{e}, nil
	case *expr.CompLiteral:
		switch t := e.Type.(type) {
		case *tipe.Struct:
			s := &Struct{
				Fields: make([]*Variable, len(t.Fields)),
			}
			if len(e.Keys) > 0 {
				named := make(map[string]expr.Expr)
				for i, elem := range e.Elements {
					named[e.Keys[i].(*expr.Ident).Name] = elem
				}
				for i, ft := range t.Fields {
					expr, ok := named[t.FieldNames[i]]
					if ok {
						v, err := p.evalExprAndReadVar(expr)
						if err != nil {
							return nil, err
						}
						s.Fields[i] = &Variable{Value: v}
					} else {
						s.Fields[i] = zeroVariable(ft)
					}
				}
				return []interface{}{s}, nil
			}
			for i, expr := range e.Elements {
				v, err := p.evalExprAndReadVar(expr)
				if err != nil {
					return nil, err
				}
				s.Fields[i] = &Variable{Value: v}
			}
			return []interface{}{s}, nil
		}
	case *expr.MapLiteral:
		//t := e.Type.(*tipe.Map)
		m := make(map[interface{}]interface{}, len(e.Keys))
		for i, kexpr := range e.Keys {
			k, err := p.evalExprAndReadVar(kexpr)
			if err != nil {
				return nil, err
			}
			v, err := p.evalExprAndReadVar(e.Values[i])
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		return []interface{}{m}, nil
	case *expr.FuncLiteral:
		if len(e.Type.FreeVars) == 0 {
			return []interface{}{e}, nil
		}
		c := &Closure{
			Func: e,
			Scope: &Scope{
				Parent: p.Cur,
				Var:    make(map[string]*Variable),
			},
		}
		for _, name := range e.Type.FreeVars {
			c.Scope.Var[name] = p.Cur.Lookup(name)
		}
		return []interface{}{c}, nil
	case *expr.Ident:
		if v := p.Cur.Lookup(e.Name); v != nil {
			return []interface{}{v}, nil
		}
		return nil, fmt.Errorf("eval: undefined identifier: %q", e.Name)
	case *expr.Unary:
		switch e.Op {
		case token.LeftParen:
			return p.evalExpr(e.Expr)
		case token.Not:
			v, err := p.evalExprAndReadVar(e.Expr)
			if err != nil {
				return nil, err
			}
			if v, ok := v.(bool); ok {
				return []interface{}{!v}, nil
			}
			return nil, fmt.Errorf("negation operator expects boolean expression, not %T", v)
		case token.Sub:
			rhs, err := p.evalExprAndReadVar(e.Expr)
			if err != nil {
				return nil, err
			}
			var lhs interface{}
			switch rhs.(type) {
			case int64:
				lhs = int64(0)
			case float32:
				lhs = float32(0)
			case float64:
				lhs = float64(0)
			case *big.Int:
				lhs = big.NewInt(0)
			case *big.Float:
				lhs = big.NewFloat(0)
			}
			v, err := binOp(token.Sub, lhs, rhs)
			if err != nil {
				return nil, err
			}
			return []interface{}{v}, nil
		}
	case *expr.Binary:
		lhs, err := p.evalExprAndReadVar(e.Left)
		if err != nil {
			return nil, err
		}

		switch e.Op {
		case token.LogicalAnd, token.LogicalOr:
			if e.Op == token.LogicalAnd && !lhs.(bool) {
				return []interface{}{false}, nil
			}
			if e.Op == token.LogicalOr && lhs.(bool) {
				return []interface{}{true}, nil
			}
			rhs, err := p.evalExprAndReadVar(e.Right)
			if err != nil {
				return nil, err
			}
			return []interface{}{rhs.(bool)}, nil
		}

		rhs, err := p.evalExprAndReadVar(e.Right)
		if err != nil {
			return nil, err
		}

		v, err := binOp(e.Op, lhs, rhs)
		if err != nil {
			return nil, err
		}
		return []interface{}{v}, nil
	case *expr.Call:
		res, err := p.evalExprAndReadVar(e.Func)
		if err != nil {
			return nil, err
		}

		args := make([]interface{}, len(e.Args))
		for i, arg := range e.Args {
			// TODO calling g(f()) where:
			//	g(T, U) and f() (T, U)
			v, err := p.evalExprAndReadVar(arg)
			if err != nil {
				return nil, err
			}
			args[i] = v
		}

		switch fn := res.(type) {
		case *Closure:
			return p.callClosure(fn, args)
		case *expr.FuncLiteral:
			return p.callFuncLiteral(fn, args)
		case *GoFunc:
			res, err := fn.call(args)
			if err != nil {
				return nil, err
			}
			return res, nil
		default:
			return nil, fmt.Errorf("do not know how to call %T", fn)
		}
	case *expr.Shell:
		for _, cmd := range e.Cmds {
			state := CmdState{
				RunCmd: p.EvalCmd,
				Stdin:  os.Stdin,
				Stdout: os.Stdout,
				Stderr: os.Stderr,
			}
			if err := p.EvalShellList(cmd, state); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case *expr.Selector:
		lhs, err := p.evalExprAndReadVar(e.Left)
		if err != nil {
			return nil, err
		}
		switch lhs := lhs.(type) {
		case *Struct:
			t := p.Types.Types[e.Left].(*tipe.Struct)
			name := e.Right.Name
			for i, n := range t.FieldNames {
				if n == name {
					return []interface{}{lhs.Fields[i]}, nil
				}
			}
			return nil, fmt.Errorf("unknown field %s in %s", name, t)
		case *GoPkg:
			v := lhs.Type.Exports[e.Right.Name]
			if v == nil {
				return nil, fmt.Errorf("%s not found in Go package %s", e, e.Left)
			}
			switch v := v.(type) {
			case *tipe.Func:
				res := &GoFunc{
					Type: v,
					Func: gowrap.Pkgs[lhs.Type.Path].Exports[e.Right.Name],
					// TODO
				}
				return []interface{}{res}, nil
			}
			return nil, fmt.Errorf("TODO GoPkg: %#+v\n", lhs)
		}

		return nil, fmt.Errorf("unexpected selector LHS: %s", e.Left.Sexp())
	case *expr.Index:
		container, err := p.evalExprAndReadVar(e.Expr)
		if err != nil {
			return nil, err
		}
		if _, isMap := p.Types.Types[e.Expr].(*tipe.Map); isMap {
			k, err := p.evalExprAndReadVar(e.Index)
			if err != nil {
				return nil, err
			}
			v := container.(map[interface{}]interface{})[k]
			return []interface{}{v}, nil
		}
	}
	return nil, fmt.Errorf("TODO evalExpr(%s), %T", e.Sexp(), e)
}
