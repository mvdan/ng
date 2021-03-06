// Copyright 2017 The Neugram Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package ngcore presents a Neugram interpreter interface and
// the associated machinery that depends on the state of the
// interpreter, such as code completion.
//
// This package is designed for embedding Neugram into a program.
package ngcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"

	"neugram.io/ng/eval"
	"neugram.io/ng/eval/environ"
	"neugram.io/ng/eval/shell"
	"neugram.io/ng/format"
	"neugram.io/ng/parser"
)

type Neugram struct {
	// TODO: Universe *eval.Scope for session initialization

	mu       sync.Mutex // guards map, not interior of *Session obj
	sessions map[string]*Session
}

func New() *Neugram {
	return &Neugram{
		sessions: make(map[string]*Session),
	}
}

type Session struct {
	Parser     *parser.Parser
	Program    *eval.Program
	ShellState *shell.State

	// Stdin, Stdout, and Stderr are the stdio to hook up to evaluator.
	// Nominally Stdout and Stderr are io.Writers.
	// If these interfaces have the concrete type *os.File the underlying
	// file descriptor is passed directly to shell jobs.
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File

	ExecCount int // number of statements executed
	// TODO: record execution statement history here

	name    string
	neugram *Neugram
}

func (n *Neugram) NewSession(ctx context.Context, name string) (*Session, error) {
	s := n.newSession(ctx, name)

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.sessions[name] != nil {
		return nil, fmt.Errorf("neugram: session %q already exists", name)
	}
	n.sessions[name] = s
	return s, nil
}

func (n *Neugram) GetSession(name string) *Session {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sessions[name]
}

func (n *Neugram) newSession(ctx context.Context, name string) *Session {
	// TODO: default shell state
	shellState := &shell.State{
		Env:   environ.New(),
		Alias: environ.New(),
	}

	// TODO: wire ctx into *eval.Program for cancellation (replace sigint channel)
	return &Session{
		Parser:     parser.New(name),
		Program:    eval.New("session-"+name, shellState),
		ShellState: shellState,
		name:       name,
		neugram:    n,
	}
}

func (n *Neugram) GetOrNewSession(ctx context.Context, name string) *Session {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s := n.sessions[name]; s != nil {
		return s
	}
	s := n.newSession(ctx, name)
	n.sessions[name] = s
	return s
}

// Exec returns the evaluation of the content of src and an error, if any.
// If src contains multiple statements, Exec returns the value of the last one.
func (s *Session) Exec(src []byte) ([]reflect.Value, error) {
	var err error
	stdout := s.Stdout
	if stdout == nil {
		stdout, err = os.Create(os.DevNull)
		if err != nil {
			return nil, err
		}
	}
	stderr := s.Stderr
	if stderr == nil {
		stdout, err = os.Create(os.DevNull)
		if err != nil {
			return nil, err
		}
	}

	s.ExecCount++

	res := s.Parser.ParseLine(src)
	if len(res.Errs) > 0 {
		errs := make([]error, len(res.Errs))
		for i, err := range res.Errs {
			errs[i] = err
		}
		return nil, Error{Phase: "parser", List: errs}
	}
	var out []reflect.Value
	for _, stmt := range res.Stmts {
		v, err := s.Program.Eval(stmt, nil)
		if err != nil {
			str := err.Error()
			if strings.HasPrefix(str, "typecheck: ") { // TODO: gross
				return nil, Error{
					Phase: "typecheck",
					List: []error{
						errors.New(strings.TrimPrefix(str, "typecheck: ")),
					},
				}
			}
			return nil, Error{Phase: "eval", List: []error{err}}
		}
		out = v
	}
	for _, cmd := range res.Cmds {
		j := &shell.Job{
			State:  s.ShellState,
			Cmd:    cmd,
			Params: s.Program,
			Stdin:  s.Stdin,
			Stdout: stdout,
			Stderr: stderr,
		}
		if err := j.Start(); err != nil {
			fmt.Fprintln(stdout, err)
			continue
		}
		done, err := j.Wait()
		if err != nil {
			return nil, Error{Phase: "shell", List: []error{err}}
		}
		if !done {
			break // TODO not right, instead we should just have one cmd, not Cmds here.
		}
	}
	return out, nil
}

// Display displays the results of an execution to w.
func (s *Session) Display(w io.Writer, vals []reflect.Value) {
	if len(vals) > 1 {
		fmt.Fprint(w, "(")
	}
	for i, val := range vals {
		if i > 0 {
			fmt.Fprint(w, ", ")
		}
		if val == (reflect.Value{}) {
			fmt.Fprint(w, "<nil>")
			continue
		}
		switch v := val.Interface().(type) {
		case eval.UntypedInt:
			fmt.Fprint(w, v.String())
		case eval.UntypedFloat:
			fmt.Fprint(w, v.String())
		case eval.UntypedComplex:
			fmt.Fprint(w, v.String())
		case eval.UntypedString:
			fmt.Fprint(w, v.String)
		case eval.UntypedRune:
			fmt.Fprintf(w, "%v", v.Rune)
		case eval.UntypedBool:
			fmt.Fprint(w, v.Bool)
		default:
			fmt.Fprint(w, format.Debug(v))
		}
	}
	if len(vals) > 1 {
		fmt.Fprintln(w, ")")
	} else if len(vals) == 1 {
		fmt.Fprintln(w, "")
	}
}

func (s *Session) Close() {
	s.neugram.mu.Lock()
	delete(s.neugram.sessions, s.name)
	s.neugram.mu.Unlock()

	// TODO: clean up Program
}

type Error struct {
	Phase string
	List  []error
}

func (e Error) Error() string {
	listStr := ""
	switch len(e.List) {
	case 0:
		listStr = "empty error list"
	case 1:
		listStr = e.List[0].Error()
	default:
		listStr = fmt.Sprintf("%v (and %d more)", e.List[0].Error(), len(e.List)-1)
	}
	return fmt.Sprintf("ng: %s: %v", e.Phase, listStr)
}
