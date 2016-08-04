package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"reflect"

	"golang.org/x/tools/go/buildutil"
	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/go/pointer"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func main() {
	var conf loader.Config

	// TODO: Check all reasonable arch/OS combos.

	// TODO: This would be so much easier and nicer if I could
	// just plug (path, AST)s into the loader, or at least slip in
	// between when the loader has parsed everything and when it
	// type-checks everything. Currently it's only possible to
	// provide ASTs for non-importable packages to the
	// loader.Config.

	newSources := make(map[string][]byte)
	for _, pkgName := range []string{"runtime", "runtime/internal/atomic"} {
		buildPkg, err := build.Import(pkgName, "", 0)
		if err != nil {
			log.Fatal(err)
		}
		rewriteSources(buildPkg, newSources)
	}

	ctxt := &build.Default
	ctxt = buildutil.OverlayContext(ctxt, newSources)

	conf.Build = ctxt
	conf.Import("runtime")

	lprog, err := conf.Load()
	if err != nil {
		log.Fatal("loading runtime: ", err)
	}
	fset := lprog.Fset

	prog := ssautil.CreateProgram(lprog, 0)
	prog.Build()
	runtimePkg := prog.ImportedPackage("runtime")

	// XXX Generate a synthetic "main" function that calls all of
	// the runtime entry points and exported functions. See
	// cmd/compile/internal/gc/builtin/runtime.go.

	// XXX Teach it that you can jump to sigprof at any point?

	// Prepare for pointer analysis.
	ptrConfig := pointer.Config{
		Mains:          []*ssa.Package{runtimePkg},
		BuildCallGraph: true,
		//Log:            os.Stderr,
	}

	// Register arguments to runtime.lock/unlock for PTA.
	registerLockQueries(runtimePkg, &ptrConfig)

	// Run pointer analysis.
	pta, err := pointer.Analyze(&ptrConfig)
	if err != nil {
		log.Fatal(err)
	}
	cg := pta.CallGraph

	// XXX We don't want the callgraph so much as the
	// inter-procedural flow graph. But the callgraph edges
	// indicate the ssa.CallInstruction, so we can use it.

	cg.DeleteSyntheticNodes() // ?
	// fmt.Println("digraph x {")
	// callgraph.GraphVisitEdges(pta.CallGraph, func(e *callgraph.Edge) error {
	// 	fmt.Printf("\"%s\" -> \"%s\";\n", e.Caller, e.Callee)
	// 	return nil
	// })
	// fmt.Println("}")

	{
		m := runtimePkg.Members["persistentalloc1"].(*ssa.Function)
		m.WriteTo(os.Stderr)
	}

	s := state{
		fset: fset,
		cg:   cg,
		pta:  pta,
		fns:  make(map[*ssa.Function]*funcInfo),
	}
	m := runtimePkg.Members["newobject"].(*ssa.Function)
	stringSpace := NewStringSpace()
	// TODO: Warn if any locks are held at return.
	exitLockSets := s.walkFunction(m, stringSpace.NewSet())
	log.Print("locks at return: ", exitLockSets)

	// fmt.Println("digraph x {")
	// for _, b := range m.Blocks {
	// 	fmt.Printf("%q;\n", fset.Position(b.Instrs[0].Pos()))
	// 	for _, s := range b.Succs {
	// 		fmt.Printf("%q -> %q;\n", fset.Position(b.Instrs[0].Pos()), fset.Position(s.Instrs[0].Pos()))
	// 	}
	// }
	// fmt.Println("}")
}

// rewriteSources rewrites all of the Go files in pkg to eliminate
// runtime-isms and make them easier for go/ssa to process. It fills
// rewritten with path -> new source mappings.
func rewriteSources(pkg *build.Package, rewritten map[string][]byte) {
	for _, fname := range pkg.GoFiles {
		path := filepath.Join(pkg.Dir, fname)

		// Parse source.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			log.Fatalf("%s: %s", path, err)
		}

		rewriteStubs(f)
		if pkg.Name == "runtime" {
			rewriteRuntime(f)
		}

		// Back to source.
		var buf bytes.Buffer
		if err := (&printer.Config{Mode: printer.SourcePos, Tabwidth: 8}).Fprint(&buf, fset, f); err != nil {
			log.Fatalf("outputting replacement %s: %s", path, err)
		}

		if pkg.Name == "runtime" && fname == "stubs.go" {
			// Add declarations and calls to runtime entry-points.
			//
			// TODO: Maybe put in where the original
			// declaration is so we don't mess up line
			// numbers.
			buf.Write([]byte(`
var _ = newobject(nil)
`))
		}

		rewritten[path] = buf.Bytes()
	}
}

var newStubs = make(map[string]map[string]*ast.FuncDecl)

func init() {
	var runtimeStubs = `
package runtime

// stubs.go
func getg() *g { return nil }
// Not mcall or systemstack
func memclr() { }
func memmove() { }
func fastrand1() uint32 { return 0 }
func memequal() bool { return false }
func noescape(p unsafe.Pointer) unsafe.Pointer { return p }
func cgocallback() { }
func gogo() { for { } }
func gosave() { }
func mincore() int32 { return 0 }
func jmpdefer() { for { } }
func exit1() { for { } }
func setg() { }
func breakpoint() { }
func reflectcall() { }
func procyield() { }
func cgocallback_gofunc() { }
func publicationBarrier() { }
func setcallerpc() { }
func getcallerpc() uintptr { return 0 }
func getcallersp() uintptr { return 0 }
func asmcgocall() int32 { return 0 }
func morestack() { newstack() }
func time_now() (int64, int32) { return 0, 0 }

// os_linux.go
func futex() int32 { return 0 }
func clone() int32 { return 0 }
func gettid() uint32 { return 0 }
func sigreturn() { for { } }
func rt_sigaction() int32 { return 0 }
func sigaltstack() { }
func setitimer() { }
func rtsigprocmask() { }
func getrlimit() int32 { return 0 }
func raise() { for { } }
func raiseproc() { for { } }
func sched_getaffinity() int32 { return 0 }
func osyield() { }

// stubs2.go
func read() { return 0 }
func closefd() { return 0 }
func exit() { for {} }
func nanotime() { return 0 }
func usleep() {}
func munmap() {}
func write() int32 { return 0 }
func open() int32 { return 0 }
func madvise() {}

// cputicks.go
func cputicks() { return 0 }

// cgo_mmap.go
func sysMmap() unsafe.Pointer { return nil }
func callCgoMmap() uintptr { return 0 }

// alg.go
func aeshash(p unsafe.Pointer, h, s uintptr) uintptr { return 0 }
func aeshash32(p unsafe.Pointer, h uintptr) uintptr { return 0 }
func aeshash64(p unsafe.Pointer, h uintptr) uintptr { return 0 }
func aeshashstr(p unsafe.Pointer, h uintptr) uintptr { return 0 }
`
	var atomicStubs = `
package atomic

// stubs.go
func Cas(ptr *uint32, old, new uint32) bool {
	if *ptr == old { *ptr = new; return true }
	return false
}
func Casp1(ptr *unsafe.Pointer, old, new unsafe.Pointer) bool {
	if *ptr == old { *ptr = new; return true }
	return false
}
func Casuintptr(ptr *uintptr, old, new uintptr) bool {
	if *ptr == old { *ptr = new; return true }
	return false
}
func Storeuintptr(ptr *uintptr, new uintptr) { *ptr = new }
func Loaduintptr(ptr *uintptr) uintptr { return *ptr }
func Loaduint(ptr *uint) uint { return *ptr }
func Loadint64(ptr *int64) int64 { return *ptr }
func Xaddint64(ptr *int64, delta int64) int64 {
	*ptr += delta
	return *ptr
}

// atomic_*.go
func Load(ptr *uint32) uint32 { return *ptr }
func Loadp(ptr unsafe.Pointer) unsafe.Pointer { return *(*unsafe.Pointer)(ptr) }
func Load64(ptr *uint64) uint64 { return *ptr }
func Xadd(ptr *uint32, delta int32) uint32 {
	*ptr += uint32(delta)
	return *ptr
}
func Xadd64(ptr *uint64, delta int64) uint64 {
	*ptr += uint64(delta)
	return *ptr
}
func Xadduintptr(ptr *uintptr, delta uintptr) uintptr {
	*ptr += delta
	return *ptr
}
func Xchg(ptr *uint32, new uint32) uint32 {
	old := *ptr
	*ptr = new
	return old
}
func Xchg64(ptr *uint64, new uint64) uint64 {
	old := *ptr
	*ptr = new
	return old
}
func Xchguintptr(ptr *uintptr, new uintptr) uintptr {
	old := *ptr
	*ptr = new
	return old
}
func And8(ptr *uint8, val uint8) { *ptr &= val }
func Or8(ptr *uint8, val uint8) { *ptr |= val }
func Cas64(ptr *uint64, old, new uint64) bool {
	if *ptr == old { *ptr = new; return true }
	return false
}
func Store(ptr *uint32, val uint32) { *ptr = val }
func Store64(ptr *uint64, val uint64) { *ptr = val }
func StorepNoWB(ptr unsafe.Pointer, val unsafe.Pointer) {
	*(*unsafe.Pointer)(ptr) = val
}
`

	for _, stubs := range []string{runtimeStubs, atomicStubs} {
		f, err := parser.ParseFile(token.NewFileSet(), "<newStubs>", stubs, 0)
		if err != nil {
			log.Fatal("parsing replacement stubs: ", err)
		}

		// Strip token.Pos information from stubs. It confuses
		// the printer, which winds up producing invalid Go code.
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				return true
			}
			rn := reflect.ValueOf(n).Elem()
			for i := 0; i < rn.NumField(); i++ {
				f := rn.Field(i)
				if _, ok := f.Interface().(token.Pos); ok {
					f.Set(reflect.Zero(f.Type()))
				}
			}
			return true
		})

		newMap := make(map[string]*ast.FuncDecl)
		for _, decl := range f.Decls {
			newMap[decl.(*ast.FuncDecl).Name.Name] = decl.(*ast.FuncDecl)
		}
		newStubs[f.Name.Name] = newMap
	}
}

func rewriteStubs(f *ast.File) {
	// Replace declaration bodies.
	for _, decl := range f.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Body != nil {
				continue
			}
			if newDecl, ok := newStubs[f.Name.Name][decl.Name.Name]; ok {
				decl.Body = newDecl.Body
			}
		}
	}
}

func rewriteRuntime(f *ast.File) {
	// TODO: Rewrite new/make/etc to calls to built-ins.
	Rewrite(func(node ast.Node) ast.Node {
		switch node := node.(type) {
		case *ast.CallExpr:
			id, ok := node.Fun.(*ast.Ident)
			if !ok {
				break
			}
			switch id.Name {
			case "systemstack":
				// TODO: Clean up func() { x }() -> { x }
				return &ast.CallExpr{Fun: node.Args[0], Args: []ast.Expr{}}
			case "mcall":
				return &ast.CallExpr{Fun: node.Args[0], Args: []ast.Expr{&ast.Ident{Name: "nil"}}}
			case "gopark":
				if cb, ok := node.Args[0].(*ast.Ident); ok && cb.Name == "nil" {
					break
				}
				return &ast.CallExpr{
					Fun: node.Args[0],
					Args: []ast.Expr{
						&ast.Ident{Name: "nil"},
						node.Args[1],
					},
				}
			case "goparkunlock":
				return &ast.CallExpr{
					Fun:  &ast.Ident{Name: "unlock"},
					Args: []ast.Expr{node.Args[0]},
				}
			}
		}
		return node
	}, f)
}

var lockFn, unlockFn *ssa.Function

func registerLockQueries(pkg *ssa.Package, ptrConfig *pointer.Config) {
	lockFn = pkg.Members["lock"].(*ssa.Function)
	unlockFn = pkg.Members["unlock"].(*ssa.Function)
	for _, member := range pkg.Members {
		fn, ok := member.(*ssa.Function)
		if !ok {
			continue
		}
		for _, block := range fn.Blocks {
			for _, inst := range block.Instrs {
				call, ok := inst.(ssa.CallInstruction)
				if !ok {
					continue
				}
				target := call.Common().StaticCallee()
				if target == lockFn || target == unlockFn {
					log.Print("found ", inst, " in ", member)
					ptrConfig.AddQuery(call.Common().Args[0])
				}
			}
		}
	}
}

// TODO: As an experiment, implement lock-set checking. Track down the
// value passed to lock/unlock to a types.Var or types.Object or
// something. Can I use points-to analysis for that?

type StringSpace struct {
	m map[string]int
	s []string
}

func NewStringSpace() *StringSpace {
	return &StringSpace{m: make(map[string]int)}
}

func (sp *StringSpace) Intern(str string) int {
	if id, ok := sp.m[str]; ok {
		return id
	}
	id := len(sp.s)
	sp.s = append(sp.s, str)
	sp.m[str] = id
	return id
}

func (sp *StringSpace) NewSet() *LockSet {
	return &LockSet{sp: sp}
}

type LockSet struct {
	sp   *StringSpace
	bits big.Int
}

type LockSetKey string

func (set *LockSet) Key() LockSetKey {
	return LockSetKey(set.bits.Text(16))
}

func (set *LockSet) Plus(s pointer.PointsToSet) *LockSet {
	// TODO: Using the label strings is a hack. Internally, the
	// pointer package already represents PointsToSet as a sparse
	// integer set, but that isn't exposed. :(
	var out *LockSet
	for _, label := range s.Labels() {
		id := set.sp.Intern(label.String())
		if out == nil {
			if set.bits.Bit(id) == 0 {
				out = &LockSet{sp: set.sp}
				out.bits.SetBit(&set.bits, id, 1)
			}
		} else {
			out.bits.SetBit(&out.bits, id, 1)
		}
	}
	if out == nil {
		return set
	}
	return out
}

func (set *LockSet) Minus(s pointer.PointsToSet) *LockSet {
	var out *LockSet
	for _, label := range s.Labels() {
		id := set.sp.Intern(label.String())
		if out == nil {
			if set.bits.Bit(id) != 0 {
				out = &LockSet{sp: set.sp}
				out.bits.SetBit(&set.bits, id, 0)
			}
		} else {
			out.bits.SetBit(&out.bits, id, 0)
		}
	}
	if out == nil {
		return set
	}
	return out
}

func (set *LockSet) String() string {
	b := []byte("{")
	first := true
	for i := 0; i < set.bits.BitLen(); i++ {
		if set.bits.Bit(i) != 0 {
			if !first {
				b = append(b, ',')
			}
			first = false
			b = append(b, set.sp.s[i]...)
		}
	}
	return string(append(b, '}'))
}

type LockSetSet struct {
	M map[LockSetKey]*LockSet
}

func NewLockSetSet() *LockSetSet {
	return &LockSetSet{make(map[LockSetKey]*LockSet)}
}

func (lss *LockSetSet) Add(ss *LockSet) {
	lss.M[ss.Key()] = ss
}

func (lss *LockSetSet) Union(lss2 *LockSetSet) {
	if lss2 == nil {
		return
	}
	for k, ss := range lss2.M {
		lss.M[k] = ss
	}
}

func (lss *LockSetSet) ToSlice() []*LockSet {
	// TODO: Make deterministic?
	slice := make([]*LockSet, 0, len(lss.M))
	for _, ss := range lss.M {
		slice = append(slice, ss)
	}
	return slice
}

func (lss *LockSetSet) String() string {
	b := []byte("{")
	first := true
	for _, ss := range lss.M {
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, ss.String()...)
	}
	return string(append(b, '}'))
}

type funcInfo struct {
	// exitLockSets maps from entry lock set to set of exit lock
	// sets. It memoizes the result of walkFunction.
	exitLockSets map[LockSetKey]*LockSetSet

	// ifDeps records the set of control-flow dependencies for
	// each ssa.BasicBlock of this function. These are the values
	// at entry to each block that may affect future control flow
	// decisions.
	ifDeps []map[ssa.Instruction]struct{}
}

type state struct {
	fset  *token.FileSet
	cg    *callgraph.Graph
	pta   *pointer.Result
	fns   map[*ssa.Function]*funcInfo
	stack []*ssa.Function
}

// walkFunction explores f, given locks held on entry to f. It returns
// the set of locksets that can be held on exit from f.
//
// This implements the lockset algorithm from Engler and Ashcroft,
// SOSP 2003, plus simple path sensitivity to reduce mistakes from
// correlated control flow.
//
// TODO: Does this also have to return all locks acquired, even those
// that are released by the time f returns?
//
// TODO: A lot of call trees simply don't take locks. We could record
// that fact and fast-path the entry locks to the exit locks.
func (s *state) walkFunction(f *ssa.Function, locks *LockSet) *LockSetSet {
	fInfo := s.fns[f]
	if fInfo == nil {
		// First visit of this function.

		// Compute control-flow dependencies.
		//
		// TODO: Figure out which control flow decisions
		// actually affect locking and only track those. Right
		// now we hit a lot of simple increment loops that
		// cause path aborts, but don't involve any locking.
		var ifInstrs []ssa.Instruction
		for _, b := range f.Blocks {
			if len(b.Instrs) == 0 {
				continue
			}
			instr, ok := b.Instrs[len(b.Instrs)-1].(*ssa.If)
			if !ok {
				continue
			}
			ifInstrs = append(ifInstrs, instr)
		}
		ifDeps := livenessFor(f, ifInstrs)
		if false { // Debug
			f.WriteTo(os.Stderr)
			for bid, vals := range ifDeps {
				fmt.Fprintf(os.Stderr, "%d: ", bid)
				for dep := range vals {
					fmt.Fprintf(os.Stderr, " %s", dep.(ssa.Value).Name())
				}
				fmt.Fprintf(os.Stderr, "\n")
			}
		}

		fInfo = &funcInfo{
			exitLockSets: make(map[LockSetKey]*LockSetSet),
			ifDeps:       ifDeps,
		}
		s.fns[f] = fInfo

		if f.Blocks == nil {
			log.Println("warning: external function", f.Name())
		}
	}

	if f.Blocks == nil {
		// External function. Assume it doesn't affect locks.
		lss1 := NewLockSetSet()
		lss1.Add(locks)
		return lss1
	}

	// Check memoization cache.
	locksKey := locks.Key()
	if memo, ok := fInfo.exitLockSets[locksKey]; ok {
		return memo
	}

	// Resolve function cycles by returning an empty set of
	// locksets.
	//
	// TODO: RacerX detects cycles *without* regard to the entry
	// lock set. We could do that, but it doesn't seem to be an
	// issue to include the lock set. However, since we have the
	// lock set, maybe if we have a cycle with a non-empty lock
	// set we should report a self-deadlock.
	fInfo.exitLockSets[locksKey] = nil

	s.stack = append(s.stack, f)
	defer func() { s.stack = s.stack[:len(s.stack)-1] }()

	blockCache := make(map[blockCacheKey][]blockCacheKey2)
	exitLockSets := NewLockSetSet()
	s.walkBlock(f, f.Blocks[0], blockCache, nil, locks, exitLockSets)
	fInfo.exitLockSets[locksKey] = exitLockSets
	log.Printf("%s: %s -> %s", f.Name(), locks, exitLockSets)
	return exitLockSets
}

type blockCacheKey struct {
	block   *ssa.BasicBlock
	lockset LockSetKey
}

type blockCacheKey2 struct {
	vs *ValState
}

func (s *state) walkBlock(f *ssa.Function, b *ssa.BasicBlock, blockCache map[blockCacheKey][]blockCacheKey2, vs *ValState, enterLockSet *LockSet, exitLockSets *LockSetSet) {
	bck := blockCacheKey{b, enterLockSet.Key()}
	if bck2s, ok := blockCache[bck]; ok {
		for _, bck2 := range bck2s {
			if bck2.vs.EqualAt(vs, s.fns[f].ifDeps[b.Index]) {
				// Terminate recursion. Some other
				// path has already visited here with
				// this lock set and value state.
				return
			}
		}
		if len(bck2s) > 10 {
			log.Print("warning: too many states at ", f.Name(), " block ", b.Index, "; giving up on path")
			// for _, bck2 := range bck2s {
			// 	log.Print("next ", f.Name(), ":", b.Index)
			// 	bck2.vs.WriteTo(os.Stderr)
			// }
			return
		}
	}
	blockCache[bck] = append(blockCache[bck], blockCacheKey2{vs})

	// For each instruction, compute the effect of that
	// instruction on all possible lock sets at that point.
	lockSets := NewLockSetSet()
	lockSets.Add(enterLockSet)
	var outs [10]*ssa.Function
	var ifCond ssa.Value
	for _, instr := range b.Instrs {
		// Update value state with the effect of this
		// instruction.
		vs = vs.Do(instr)

		switch instr := instr.(type) {
		case *ssa.If:
			// We'll bind ifCond to true or false when we
			// visit successors.
			ifCond = instr.Cond

		case *ssa.Call:
			// TODO: There are other types of
			// ssa.CallInstructions, but they have different
			// control flow.
			outs := outs[:0]
			if out := instr.Call.StaticCallee(); out != nil {
				outs = append(outs, out)
			} else if cnode := s.cg.Nodes[b.Parent()]; cnode != nil {
				// TODO: Build an index in walkFunction?
				for _, o := range cnode.Out {
					if o.Site != instr {
						continue
					}
					outs = append(outs, o.Callee.Func)
				}
			} else {
				// TODO: This happens for print and
				// println, which we should turn into
				// calls to their implementation
				// functions.
				log.Print("no call graph for ", instr, " in ", b.Parent(), " at ", s.fset.Position(instr.Pos()))
			}

			nextLockSets := NewLockSetSet()
			for _, o := range outs {
				fmt.Printf("%q -> %q;\n", f.String(), o.String())
				// TODO: _Gscan locks, misc locks
				if o == lockFn {
					lock := s.pta.Queries[instr.Call.Args[0]].PointsTo()
					for _, ls := range lockSets.M {
						ls = ls.Plus(lock)
						nextLockSets.Add(ls)
					}
					//log.Print("call to runtime.lock: ", s.pta.Queries[instr.Call.Args[0]].PointsTo())
				} else if o == unlockFn {
					lock := s.pta.Queries[instr.Call.Args[0]].PointsTo()
					for _, ls := range lockSets.M {
						// TODO: Warn on
						// unlock of unlocked
						// lock.
						ls = ls.Minus(lock)
						nextLockSets.Add(ls)
					}
				} else {
					for _, ls := range lockSets.M {
						nextLockSets.Union(s.walkFunction(o, ls))
					}
				}
			}
			lockSets = nextLockSets

		case *ssa.Return:
			// We've reached function exit. Add the
			// current lock set to exitLockSets.
			//
			// TODO: Handle defers.
			exitLockSets.Union(lockSets)
		}
	}

	// Annoyingly, the last instruction in an ssa.BasicBlock
	// doesn't have a location, even if it obviously corresponds
	// to a source statement. exitPos guesses one.
	exitPos := func(b *ssa.BasicBlock) token.Pos {
		for b != nil {
			for i := len(b.Instrs) - 1; i >= 0; i-- {
				if pos := b.Instrs[i].Pos(); pos != 0 {
					return pos
				}
			}
			if len(b.Preds) == 0 {
				break
			}
			b = b.Preds[0]
		}
		return 0
	}

	// If this is an "if", see if we have enough information to
	// determine its direction.
	succs := b.Succs
	if ifCond != nil {
		x := vs.Get(ifCond)
		if x != nil {
			log.Printf("determined control flow at %s: %v", s.fset.Position(exitPos(b)), x)
			if constant.BoolVal(x.(DynConst).c) {
				// Take true path.
				succs = succs[:1]
			} else {
				// Take false path.
				succs = succs[1:]
			}
		}
	}

	// Process block successors.
	for _, succLockSet := range lockSets.M {
		for i, b2 := range succs {
			vs2 := vs
			if ifCond != nil {
				// TODO: We could back-propagate this
				// in simple cases, like when ifCond
				// is a == BinOp. (And we could
				// forward-propagate that! Hmm.)
				vs2 = vs2.Extend(ifCond, DynConst{constant.MakeBool(i == 0)})
			}

			// Propagate values over phis at the beginning
			// of b2.
			for _, instr := range b2.Instrs {
				instr, ok := instr.(*ssa.Phi)
				if !ok {
					break
				}
				for i, inval := range instr.Edges {
					if b2.Preds[i] == b {
						x := vs2.Get(inval)
						if x != nil {
							vs2 = vs2.Extend(instr, x)
						}
					}
				}
			}

			s.walkBlock(f, b2, blockCache, vs2, succLockSet, exitLockSets)
		}
	}
}