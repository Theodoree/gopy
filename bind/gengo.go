// Copyright 2015 The go-python Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bind

import (
	"fmt"
	"go/token"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/types"
)

const (
	goPreamble = `// Package main is an autogenerated binder stub for package %[1]s.
// gopy gen -lang=go %[1]s
//
// File is generated by gopy gen. Do not edit.
package main

//#cgo pkg-config: python2 --cflags --libs
//#include <stdlib.h>
//#include <string.h>
import "C"

import (
	"sync"
	"unsafe"

	%[2]q
)

var _ = unsafe.Pointer(nil)

// --- begin cgo helpers ---

//export CGoPy_GoString
func CGoPy_GoString(str *C.char) string { 
	return C.GoString(str)
}

//export CGoPy_CString
func CGoPy_CString(s string) *C.char {
	return C.CString(s)
}

//export CGoPy_ErrorIsNil
func CGoPy_ErrorIsNil(err error) bool {
	return err == nil
}

//export CGoPy_ErrorString
func CGoPy_ErrorString(err error) *C.char {
	return C.CString(err.Error())
}

// --- end cgo helpers ---

// --- begin cref helpers ---

type cobject struct {
	ptr unsafe.Pointer
	cnt int32
}

// refs stores Go objects that have been passed to another language.
var refs struct {
	sync.Mutex
	next int32 // next reference number to use for Go object, always negative
	refs map[unsafe.Pointer]int32
	ptrs map[int32]cobject
}

//export cgopy_incref
func cgopy_incref(ptr unsafe.Pointer) {
	refs.Lock()
	num, ok := refs.refs[ptr]
	if ok {
		s := refs.ptrs[num]
		refs.ptrs[num] = cobject{s.ptr, s.cnt + 1}
	} else {
		num = refs.next
		refs.next--
		if refs.next > 0 {
			panic("refs.next underflow")
		}
		refs.refs[ptr] = num
		refs.ptrs[num] = cobject{ptr, 1}
	}
	refs.Unlock()
}

//export cgopy_decref
func cgopy_decref(ptr unsafe.Pointer) {
	refs.Lock()
	num, ok := refs.refs[ptr]
	if !ok {
		panic("cgopy: decref untracked object")
	}
	s := refs.ptrs[num]
	if s.cnt - 1 <= 0 {
		delete(refs.ptrs, num)
		delete(refs.refs, ptr)
		refs.Unlock()
		return
	}
	refs.ptrs[num] = cobject{s.ptr, s.cnt - 1}
	refs.Unlock()
}

func init() {
	refs.Lock()
	refs.next = -24 // Go objects get negative reference numbers. Arbitrary starting point.
	refs.refs = make(map[unsafe.Pointer]int32)
	refs.ptrs = make(map[int32]cobject)
	refs.Unlock()

	// make sure cgo is used and cgo hooks are run
	str := C.CString(%[1]q)
	C.free(unsafe.Pointer(str))
}

// --- end cref helpers ---
`
)

type goGen struct {
	*printer

	fset *token.FileSet
	pkg  *Package
	err  ErrorList
}

func (g *goGen) gen() error {

	g.genPreamble()

	for _, s := range g.pkg.structs {
		g.genStruct(s)
	}

	// expose ctors at module level
	// FIXME(sbinet): attach them to structs?
	// -> problem is if one has 2 or more ctors with exactly the same signature.
	for _, s := range g.pkg.structs {
		for _, ctor := range s.ctors {
			g.genFunc(ctor)
		}
	}

	for _, f := range g.pkg.funcs {
		g.genFunc(f)
	}

	for _, c := range g.pkg.consts {
		g.genConst(c)
	}

	for _, v := range g.pkg.vars {
		g.genVar(v)
	}

	g.Printf("// buildmode=c-shared needs a 'main'\nfunc main() {}\n")
	if len(g.err) > 0 {
		return g.err
	}

	return nil
}

func (g *goGen) genFunc(f Func) {
	sig := f.Signature()

	params := "(" + g.tupleString(sig.Params()) + ")"
	ret := g.tupleString(sig.Results())
	if len(sig.Results()) > 1 {
		ret = "(" + ret + ") "
	} else {
		ret += " "
	}

	//funcName := o.Name()
	g.Printf(`
//export GoPy_%[1]s
// GoPy_%[1]s wraps %[2]s.%[3]s
func GoPy_%[1]s%[4]v%[5]v{
`,
		f.ID(),
		f.Package().Name(),
		f.GoName(),
		params,
		ret,
	)

	g.Indent()
	g.genFuncBody(f)
	g.Outdent()
	g.Printf("}\n\n")
}

func (g *goGen) genFuncBody(f Func) {
	sig := f.Signature()
	results := sig.Results()
	for i := range results {
		if i > 0 {
			g.Printf(", ")
		}
		g.Printf("_gopy_%03d", i)
	}
	if len(results) > 0 {
		g.Printf(" := ")
	}

	g.Printf("%s.%s(", g.pkg.Name(), f.GoName())

	args := sig.Params()
	for i, arg := range args {
		tail := ""
		if i+1 < len(args) {
			tail = ", "
		}
		head := arg.Name()
		if arg.needWrap() {
			head = fmt.Sprintf(
				"*(*%s)(unsafe.Pointer(%s))",
				types.TypeString(
					arg.GoType(),
					func(*types.Package) string { return g.pkg.Name() },
				),
				arg.Name(),
			)
		}
		g.Printf("%s%s", head, tail)
	}
	g.Printf(")\n")

	if len(results) <= 0 {
		return
	}

	for i, res := range results {
		if !res.needWrap() {
			continue
		}
		g.Printf("cgopy_incref(unsafe.Pointer(&_gopy_%03d))\n", i)
	}

	g.Printf("return ")
	for i, res := range results {
		if i > 0 {
			g.Printf(", ")
		}
		// if needWrap(res.GoType()) {
		// 	g.Printf("")
		// }
		if res.needWrap() {
			g.Printf("%s(unsafe.Pointer(&", res.dtype.cgotype)
		}
		g.Printf("_gopy_%03d", i)
		if res.needWrap() {
			g.Printf("))")
		}
	}
	g.Printf("\n")
}

func (g *goGen) genStruct(s Struct) {
	//fmt.Printf("obj: %#v\ntyp: %#v\n", obj, typ)
	typ := s.Struct()
	pkgname := s.Package().Name()
	g.Printf("//export GoPy_%[1]s\n", s.ID())
	g.Printf("type GoPy_%[1]s unsafe.Pointer\n\n", s.ID())

	for i := 0; i < typ.NumFields(); i++ {
		f := typ.Field(i)
		if !f.Exported() {
			continue
		}

		ft := f.Type()
		ftname := g.qualifiedType(ft)
		if needWrapType(ft) {
			ftname = fmt.Sprintf("GoPy_%[1]s_field_%d", s.ID(), i+1)
			g.Printf("//export %s\n", ftname)
			g.Printf("type %s unsafe.Pointer\n\n", ftname)
		}

		// -- getter --

		g.Printf("//export GoPy_%[1]s_getter_%[2]d\n", s.ID(), i+1)
		g.Printf("func GoPy_%[1]s_getter_%[2]d(self GoPy_%[1]s) %[3]s {\n",
			s.ID(), i+1,
			ftname,
		)
		g.Indent()
		g.Printf(
			"ret := (*%[1]s)(unsafe.Pointer(self))\n",
			pkgname+"."+s.GoName(),
		)

		if needWrapType(ft) {
			g.Printf("cgopy_incref(unsafe.Pointer(&ret.%s))\n", f.Name())
			g.Printf("return %s(unsafe.Pointer(&ret.%s))\n", ftname, f.Name())
		} else {
			g.Printf("return ret.%s\n", f.Name())
		}
		g.Outdent()
		g.Printf("}\n\n")

		// -- setter --
		g.Printf("//export GoPy_%[1]s_setter_%[2]d\n", s.ID(), i+1)
		g.Printf("func GoPy_%[1]s_setter_%[2]d(self GoPy_%[1]s, v %[3]s) {\n",
			s.ID(), i+1, ftname,
		)
		g.Indent()
		fset := "v"
		if needWrapType(ft) {
			fset = fmt.Sprintf("*(*%s.%s)(unsafe.Pointer(v))",
				f.Pkg().Name(),
				types.TypeString(f.Type(), types.RelativeTo(f.Pkg())),
			)
		}
		g.Printf(
			"(*%[1]s)(unsafe.Pointer(self)).%[2]s = %[3]s\n",
			pkgname+"."+s.GoName(),
			f.Name(),
			fset,
		)
		g.Outdent()
		g.Printf("}\n\n")
	}

	for _, m := range s.meths {
		g.genMethod(s, m)
	}

	g.Printf("//export GoPy_%[1]s_new\n", s.ID())
	g.Printf("func GoPy_%[1]s_new() GoPy_%[1]s {\n", s.ID())
	g.Indent()
	g.Printf("o := %[1]s.%[2]s{}\n", pkgname, s.GoName())
	g.Printf("cgopy_incref(unsafe.Pointer(&o))\n")
	g.Printf("return (GoPy_%[1]s)(unsafe.Pointer(&o))\n", s.ID())
	g.Outdent()
	g.Printf("}\n\n")
}

func (g *goGen) genMethod(s Struct, m Func) {
	sig := m.Signature()
	params := "(self GoPy_" + s.ID()
	if len(sig.Params()) > 0 {
		params += ", " + g.tupleString(sig.Params())
	}
	params += ")"

	ret := g.tupleString(sig.Results())
	if len(sig.Results()) > 1 {
		ret = "(" + ret + ")"
	} else {
		ret += " "
	}

	g.Printf("//export GoPy_%[1]s\n", m.ID())
	g.Printf("func GoPy_%[1]s%[2]s%[3]s{\n",
		m.ID(),
		params,
		ret,
	)
	g.Indent()
	g.genMethodBody(s, m)
	g.Outdent()
	g.Printf("}\n\n")
}

func (g *goGen) genMethodBody(s Struct, m Func) {
	sig := m.Signature()
	results := sig.Results()
	for i := range results {
		if i > 0 {
			g.Printf(", ")
		}
		g.Printf("_gopy_%03d", i)
	}
	if len(results) > 0 {
		g.Printf(" := ")
	}

	g.Printf("(*%s.%s)(unsafe.Pointer(self)).%s(",
		g.pkg.Name(), s.GoName(),
		m.GoName(),
	)

	args := sig.Params()
	for i, arg := range args {
		tail := ""
		if i+1 < len(args) {
			tail = ", "
		}
		g.Printf("%s%s", arg.Name(), tail)
	}
	g.Printf(")\n")

	if len(results) <= 0 {
		return
	}

	g.Printf("return ")
	for i, res := range results {
		if i > 0 {
			g.Printf(", ")
		}
		// if needWrap(res.GoType()) {
		// 	g.Printf("")
		// }
		if res.needWrap() {
			g.Printf("%s(unsafe.Pointer(&", res.dtype.cgotype)
		}
		g.Printf("_gopy_%03d", i)
		if res.needWrap() {
			g.Printf("))")
		}
	}
	g.Printf("\n")

}

func (g *goGen) genConst(o Const) {
	pkgname := o.obj.Pkg().Name()
	tname := types.TypeString(o.obj.Type(), types.RelativeTo(o.obj.Pkg()))
	if strings.HasPrefix(tname, "untyped ") {
		tname = string(tname[len("untyped "):])
	}
	g.Printf("//export GoPy_get_%s\n", o.id)
	g.Printf("func GoPy_get_%[1]s() %[2]s {\n", o.id, tname)
	g.Indent()
	g.Printf("return %s.%s\n", pkgname, o.obj.Name())
	g.Outdent()
	g.Printf("}\n\n")
}

func (g *goGen) genVar(o Var) {
	pkgname := o.pkg.Name()
	g.Printf("//export GoPy_get_%s\n", o.id)
	ret := g.qualifiedType(o.GoType())
	g.Printf("func GoPy_get_%[1]s() %[2]s {\n", o.id, ret)
	g.Indent()
	if o.needWrap() {
		g.Printf("cgopy_incref(unsafe.Pointer(&%s.%s))\n", pkgname, o.Name())
	}
	g.Printf("return ")
	if o.needWrap() {
		g.Printf("%s(unsafe.Pointer(&", o.dtype.cgotype)
	}
	g.Printf("%s.%s", pkgname, o.Name())
	if o.needWrap() {
		g.Printf("))")
	}
	g.Printf("\n")
	g.Outdent()
	g.Printf("}\n\n")

	g.Printf("//export GoPy_set_%s\n", o.id)
	g.Printf("func GoPy_set_%[1]s(v %[2]s) {\n", o.id, ret)
	g.Indent()
	vset := "v"
	typ := o.GoType()
	if needWrapType(typ) {
		vset = fmt.Sprintf("*(*%s)(unsafe.Pointer(v))",
			types.TypeString(typ, func(*types.Package) string {
				return pkgname
			}),
		)
	}
	g.Printf(
		"%[1]s.%[2]s = %[3]s\n",
		pkgname, o.Name(), vset,
	)
	g.Outdent()
	g.Printf("}\n\n")
}

func (g *goGen) genPreamble() {
	n := g.pkg.pkg.Name()
	g.Printf(goPreamble, n, g.pkg.pkg.Path(), filepath.Base(n))
}

func (g *goGen) tupleString(tuple []*Var) string {
	n := len(tuple)
	if n <= 0 {
		return ""
	}

	str := make([]string, 0, n)
	for _, v := range tuple {
		n := v.Name()
		typ := v.GoType()
		str = append(str, n+" "+g.qualifiedType(typ))
	}

	return strings.Join(str, ", ")
}

func (g *goGen) qualifiedType(typ types.Type) string {
	switch typ := typ.(type) {
	case *types.Basic:
		return typ.Name()
	case *types.Named:
		obj := typ.Obj()
		switch typ.Underlying().(type) {
		case *types.Struct:
			return "GoPy_" + obj.Pkg().Name() + "_" + obj.Name()
		case *types.Interface:
			if obj.Name() == "error" {
				return "error"
			}
			return "GoPy_" + obj.Name()
		default:
			return "GoPy_ooops_" + obj.Name()
		}
	}

	return fmt.Sprintf("%#T", typ)
}
