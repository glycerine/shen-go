package vm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sync"
	"unsafe"

	"github.com/tiancaiamao/shen-go/kl"
)

type VM struct {
	stack []kl.Obj
	top   int // stack top

	env []kl.Obj // environment

	pc        int       // pc register refer to the position in current code
	savedAddr []address // saved return address

	functionTable map[string]*Procedure
	symbolTable   map[string]kl.Obj

	nativeFunc map[string]*kl.ScmPrimitive

	// jumpBuf is used to implement exception, similar to setjmp/longjmp in C.
	cc []jumpBuf
}

type jumpBuf struct {
	address
	savedAddrPos  int
	savedStackTop int
	closure       kl.Obj
}

// Code is something executable to VM, it's immutable.
type Code struct {
	bc     []instruction
	consts []kl.Obj
}

// address is the information to be saved before apply a closure.
type address struct {
	pc   int
	code *Code
	env  []kl.Obj
}

type Procedure struct {
	scmHead int
	code    *Code
	env     []kl.Obj
}

const initStackSize = 128

type pool struct {
	sync.Mutex
	data []*VM
}

func (p *pool) Get() *VM {
	p.Lock()
	defer p.Unlock()

	if len(p.data) == 0 {
		return newVM()
	}
	ret := p.data[0]
	p.data = p.data[1:]
	return ret
}

func (p *pool) Put(v *VM) {
	p.Lock()
	p.data = append(p.data, v)
	p.Unlock()
}

var auxVM pool
var stackMarkDummyValue int
var stackMark = kl.MakeRaw(&stackMarkDummyValue)

func New() *VM {
	vm := newVM()
	vm.RegistNativeCall("load-bytecode", 1, vm.loadBytecode)
	vm.RegistNativeCall("load-file", 1, vm.loadFile)
	vm.RegistNativeCall("primitive?", 1, kl.NativeIsPrimitive)
	vm.RegistNativeCall("primitive-arity", 1, kl.NativePrimitiveArity)
	vm.RegistNativeCall("primitive-id", 1, kl.NativePrimitiveID)
	for k, v := range compiler.functionTable {
		vm.functionTable[k] = v
	}
	for k, v := range compiler.symbolTable {
		vm.symbolTable[k] = v
	}
	return vm
}

func newVM() *VM {
	vm := &VM{
		stack:         make([]kl.Obj, initStackSize),
		env:           make([]kl.Obj, 0, 200),
		functionTable: make(map[string]*Procedure),
		symbolTable:   make(map[string]kl.Obj),
		nativeFunc:    make(map[string]*kl.ScmPrimitive),
	}
	initSymbolTable(vm.symbolTable)
	return vm
}

func (vm *VM) RegistNativeCall(name string, arity int, f func(...kl.Obj) kl.Obj) {
	vm.nativeFunc[name] = kl.MakePrimitive(name, arity, f)
}

func initSymbolTable(symbolTable map[string]kl.Obj) {
	dir, _ := os.Getwd()
	symbolTable["*stinput*"] = kl.MakeStream(os.Stdin)
	symbolTable["*stoutput*"] = kl.MakeStream(os.Stdout)
	symbolTable["*home-directory*"] = kl.MakeString(dir)
	symbolTable["*language*"] = kl.MakeString("Go")
	symbolTable["*implementation*"] = kl.MakeString("bytecode")
	symbolTable["*relase*"] = kl.MakeString(runtime.Version())
	symbolTable["*os*"] = kl.MakeString(runtime.GOOS)
	symbolTable["*porters*"] = kl.MakeString("Arthur Mao")
	symbolTable["*port*"] = kl.MakeString("0.0.1")

	// Extended by shen-go implementation
	symbolTable["*package-path*"] = kl.MakeString(kl.PackagePath())
}

func (vm *VM) Run(code *Code) kl.Obj {
	vm.pc = 0
	vm.savedAddr = append(vm.savedAddr, address{pc: len(code.bc) - 1, code: code})
	vm.stackPush(stackMark)

	halt := false
	for !halt {
		inst := code.bc[vm.pc]
		vm.pc++

		exception := false
		switch instructionCode(inst) {
		case iSetJmp:
			n := instructionOPN(inst)
			vm.top--
			cc := jumpBuf{
				address: address{
					pc:   vm.pc + n,
					code: code,
					env:  vm.env,
				},
				savedAddrPos:  len(vm.savedAddr),
				savedStackTop: vm.top,
				closure:       vm.stack[vm.top],
			}
			vm.cc = append(vm.cc, cc)
		case iClearJmp:
			vm.cc = vm.cc[:len(vm.cc)-1]
		case iConst:
			n := instructionOPN(inst)
			vm.stackPush(code.consts[n])
		case iAccess:
			n := instructionOPN(inst)
			// get value from environment
			vm.stackPush(vm.env[len(vm.env)-1-n])
		case iFreeze:
			// create closure directly
			// nearly the same with grab, but if need zero arguments.
			n := instructionOPN(inst)
			tmp := &Procedure{
				code: &Code{
					bc:     code.bc[vm.pc:],
					consts: code.consts,
				},
			}
			raw := kl.MakeRaw(&tmp.scmHead)
			if len(vm.env) > 0 {
				tmp.env = make([]kl.Obj, len(vm.env))
				copy(tmp.env, vm.env)
			}
			vm.stackPush(raw)
			vm.pc += n
		case iMark:
			vm.stackPush(stackMark)
		case iGrab:
			vm.top--
			if v := vm.stack[vm.top]; v == stackMark {
				// make closure if there are not enough arguments
				tmp := Procedure{
					code: &Code{
						bc:     code.bc[vm.pc-1:],
						consts: code.consts,
					},
				}
				raw := kl.MakeRaw(&tmp.scmHead)
				tmp.env = vm.env
				vm.stackPush(raw)

				// return to saved address
				savedAddr := vm.savedAddr[len(vm.savedAddr)-1]
				vm.savedAddr = vm.savedAddr[:len(vm.savedAddr)-1]
				code = savedAddr.code
				vm.pc = savedAddr.pc
				vm.env = savedAddr.env
			} else {
				// grab data from stack to env
				vm.env = append(vm.env, v)
			}
		case iReturn:
			// stack[top-1] is the result, so should check top-2
			if vm.stack[vm.top-2] == stackMark {
				savedAddr := vm.savedAddr[len(vm.savedAddr)-1]
				vm.savedAddr = vm.savedAddr[:len(vm.savedAddr)-1]

				code = savedAddr.code
				vm.pc = savedAddr.pc
				vm.env = savedAddr.env
				vm.top--
				vm.stack[vm.top-1] = vm.stack[vm.top]
			} else {
				// more arguments, continue the beta-reduce.
				// similar to tail apply
				vm.top--
				obj := vm.stack[vm.top]
				// TODO: panic if obj is not a closure
				closure := (*Procedure)(unsafe.Pointer(obj))
				code = closure.code
				vm.pc = 0
				vm.env = closure.env
			}
		case iTailApply:
			vm.top--
			obj := vm.stack[vm.top]
			// TODO: panic if obj is not a closure
			closure := (*Procedure)(unsafe.Pointer(obj))
			// The only different with Apply is that TailApply doesn't save return address.
			code = closure.code
			vm.pc = 0
			vm.env = closure.env
		case iApply:
			vm.top--
			obj := vm.stack[vm.top]
			// TODO: panic if obj is not a closure
			closure := (*Procedure)(unsafe.Pointer(obj))
			// save return address
			vm.savedAddr = append(vm.savedAddr, address{vm.pc, code, vm.env})
			// set pc to closure code
			code = closure.code
			vm.pc = 0
			vm.env = closure.env
		case iPop:
			vm.top--
		case iDefun:
			symbol := kl.GetSymbol(vm.stack[vm.top-1])
			function := (*Procedure)(unsafe.Pointer(vm.stack[vm.top-2]))
			vm.functionTable[symbol] = function
			vm.top--
			vm.stack[vm.top-1] = vm.stack[vm.top]
		case iGetF:
			symbol := kl.GetSymbol(vm.stack[vm.top-1])
			if function, ok := vm.functionTable[symbol]; ok {
				vm.stack[vm.top-1] = kl.MakeRaw(&function.scmHead)
			} else {
				vm.stack[vm.top-1] = kl.MakeError("unknown function:" + symbol)
				exception = true
			}
		case iJF:
			switch vm.stack[vm.top-1] {
			case kl.False:
				n := instructionOPN(inst)
				vm.top--
				vm.pc += n
			case kl.True:
				vm.top--
			default:
				// TODO: So what?
				vm.stack[vm.top-1] = kl.MakeError("test condition need to be boolean")
				exception = true
			}
		case iJMP:
			n := instructionOPN(inst)
			vm.pc += n
		case iHalt:
			halt = true
		case iPrimCall:
			id := instructionOPN(inst)
			prim := kl.GetPrimitiveByID(id)
			args := vm.stack[vm.top-prim.Required : vm.top]

			var result kl.Obj
			// Ugly hack: set function should not be global.
			switch prim.Name {
			case "set":
				result = kl.PrimSet(vm.symbolTable, args[0], args[1])
			case "value":
				result = kl.PrimValue(vm.symbolTable, args[0])
			case "eval-kl":
				tmp := auxVM.Get()
				tmp.symbolTable = vm.symbolTable
				tmp.functionTable = vm.functionTable
				tmp.nativeFunc = vm.nativeFunc
				result = tmp.Eval(args[0])
				auxVM.Put(tmp)
			default:
				result = prim.Function(args...)
			}

			vm.stack[vm.top-prim.Required] = result
			vm.top = vm.top - prim.Required + 1
			if kl.IsError(result) {
				exception = true
			}
		case iNativeCall:
			arity := instructionOPN(inst)
			method := kl.GetSymbol(vm.stack[vm.top-arity])
			proc, ok := vm.nativeFunc[method]
			if !ok {
				vm.stack[vm.top-1] = kl.MakeError("unknown native function:" + method)
				exception = true
				break
			}
			// Note the invariance arity = len(method + args), so arity-1 = proc.Required
			if arity-1 != proc.Required {
				vm.stack[vm.top-1] = kl.MakeError("wrong arity for native " + method)
				exception = true
				break
			}
			args := vm.stack[vm.top-proc.Required : vm.top]
			result := proc.Function(args...)

			vm.stack[vm.top-arity] = result
			vm.top = vm.top - proc.Required
			if kl.IsError(result) {
				exception = true
			}
		default:
			panic(fmt.Sprintf("unknown instruction %d", inst))
		}

		if exception {
			if len(vm.cc) == 0 {
				err := vm.stack[vm.top-1]
				vm.Reset()
				return err
			}

			// clear jmpBuf
			jmpBuf := vm.cc[len(vm.cc)-1]
			vm.cc = vm.cc[:len(vm.cc)-1]
			// pop trap-error handler, prepare for call.
			value := vm.stack[vm.top-1]
			vm.top = jmpBuf.savedStackTop
			vm.stackPush(stackMark)
			vm.stackPush(value)
			// recover savedAddr
			vm.savedAddr = vm.savedAddr[:jmpBuf.savedAddrPos]
			vm.savedAddr = append(vm.savedAddr, jmpBuf.address)
			// longjmp... tail apply
			closure := (*Procedure)(unsafe.Pointer(jmpBuf.closure))
			code = closure.code
			vm.pc = 0
			vm.env = closure.env
		}
	}

	if vm.top != 1 || len(vm.savedAddr) != 0 {
		vm.Debug()
		panic("vm in wrong status")
	}
	vm.top--
	return vm.stack[vm.top]
}

func (vm *VM) stackPush(o kl.Obj) {
	if vm.top == len(vm.stack) {
		stack := make([]kl.Obj, len(vm.stack)*2)
		copy(stack, vm.stack)
		vm.stack = stack
	}
	vm.stack[vm.top] = o
	vm.top++
}

func (vm *VM) Reset() {
	vm.stack = vm.stack[:initStackSize]
	vm.top = 0
	vm.env = vm.env[:0]
	vm.savedAddr = vm.savedAddr[:0]
	vm.cc = nil
}

func (vm *VM) Debug() {
	fmt.Println("pc:", vm.pc)
	fmt.Println("top:", vm.top)
	fmt.Println("stack:")
	for i := vm.top - 1; i >= 0; i-- {
		if vm.stack[i] == stackMark {
			fmt.Println("MARK")
		} else {
			fmt.Println(kl.ObjString(vm.stack[i]))
		}
	}
	fmt.Println("function:", len(vm.functionTable))
}

var compiler = newVM()

func klToSexpByteCode(klambda kl.Obj) kl.Obj {
	// TODO: Better way to do it?
	// tailcall (kl->bytecode klambda)
	var a Assember
	a.CONST(klambda)
	a.CONST(kl.MakeSymbol("kl->bytecode"))
	a.GetF()
	a.TAILAPPLY()
	a.HALT()

	code := a.Comiple()
	return compiler.Run(code)
}

func klToByteCode(klambda kl.Obj) (*Code, error) {
	bc := klToSexpByteCode(klambda)
	if kl.IsError(bc) {
		return nil, errors.New("klToByteCode return some thing wrong:" + kl.ObjString(bc))
	}
	var a Assember
	err := a.FromSexp(bc)
	if err != nil {
		return nil, err
	}
	code := a.Comiple()
	return code, nil
}

func (vm *VM) Eval(sexp kl.Obj) (res kl.Obj) {
	defer func() {
		if r := recover(); r != nil {
			vm.Reset()
			var buf [4096]byte
			n := runtime.Stack(buf[:], false)
			fmt.Println("Recovered in Eval:", kl.ObjString(sexp))
			fmt.Println(string(buf[:n]))
			res = kl.MakeError("panic")
		}
	}()

	code, err := klToByteCode(sexp)
	if err != nil {
		return kl.MakeError(err.Error())
	}
	res = vm.Run(code)
	return
}

func Bootstrap() {
	compiler.RegistNativeCall("primitive?", 1, kl.NativeIsPrimitive)
	compiler.RegistNativeCall("primitive-arity", 1, kl.NativePrimitiveArity)
	compiler.RegistNativeCall("primitive-id", 1, kl.NativePrimitiveID)

	compiler.mustLoadBytecode(kl.MakeString("primitive.bc"))
	compiler.mustLoadBytecode(kl.MakeString("de-bruijn.bc"))
	compiler.mustLoadBytecode(kl.MakeString("compile.bc"))

	// compiler.loadBytecode(kl.MakeString("toplevel.bc"))
	// compiler.loadBytecode(kl.MakeString("core.bc"))
	// compiler.loadBytecode(kl.MakeString("sys.bc"))
	// compiler.loadBytecode(kl.MakeString("sequent.bc"))
	// compiler.loadBytecode(kl.MakeString("yacc.bc"))
	// compiler.loadBytecode(kl.MakeString("reader.bc"))
	// compiler.loadBytecode(kl.MakeString("reader.bc"))
	// compiler.loadBytecode(kl.MakeString("prolog.bc"))
	// compiler.loadBytecode(kl.MakeString("track.bc"))
	// compiler.loadBytecode(kl.MakeString("load.bc"))
	// compiler.loadBytecode(kl.MakeString("writer.bc"))
	// compiler.loadBytecode(kl.MakeString("macros.bc"))
	// compiler.loadBytecode(kl.MakeString("declarations.bc"))
	// compiler.loadBytecode(kl.MakeString("t-star.bc"))
	// compiler.loadBytecode(kl.MakeString("types.bc"))
}

var Debug bool

func (vm *VM) mustLoadBytecode(args ...kl.Obj) {
	res := vm.loadBytecode(args...)
	if kl.IsError(res) {
		panic(kl.ObjString(res))
	}
}

func (vm *VM) loadBytecode(args ...kl.Obj) kl.Obj {
	fileName := kl.GetString(args[0])
	var f io.ReadCloser
	var err error
	if Debug {
		filePath := path.Join(kl.PackagePath(), "bytecode", fileName)
		f, err = os.Open(filePath)
	} else {
		filePath := path.Join("/bytecode", fileName)
		f, err = FS(false).Open(filePath)
	}
	if err != nil {
		return kl.MakeError(err.Error())
	}
	defer f.Close()

	r := kl.NewSexpReader(f)
	obj, err := r.Read()
	for err == nil {
		var a Assember
		if err1 := a.FromSexp(obj); err1 != nil {
			return kl.MakeError(err1.Error())
		}
		code := a.Comiple()

		tmp := auxVM.Get()
		tmp.symbolTable = vm.symbolTable
		tmp.functionTable = vm.functionTable
		tmp.nativeFunc = vm.nativeFunc
		res := tmp.Run(code)
		auxVM.Put(tmp)

		if kl.IsError(res) {
			return res
		}
		obj, err = r.Read()
	}
	if err != io.EOF {
		return kl.MakeError(err.Error())
	}
	return args[0]
}

func (vm *VM) loadFile(args ...kl.Obj) kl.Obj {
	file := kl.GetString(args[0])
	var filePath string
	if _, err := os.Stat(file); err == nil {
		filePath = file
	} else {
		filePath = path.Join(kl.PackagePath(), file)
		if _, err := os.Stat(filePath); err != nil {
			return kl.MakeError(err.Error())
		}
	}

	f, err := os.Open(filePath)
	if err != nil {
		return kl.MakeError(err.Error())
	}
	defer f.Close()

	r := kl.NewSexpReader(f)
	for {
		exp, err := r.Read()
		if err != nil {
			if err != io.EOF {
				return kl.MakeError(err.Error())
			}
			break
		}

		tmp := auxVM.Get()
		tmp.symbolTable = vm.symbolTable
		tmp.functionTable = vm.functionTable
		tmp.nativeFunc = vm.nativeFunc
		res := tmp.Eval(exp)
		auxVM.Put(tmp)

		if kl.IsError(res) {
			return res
		}
	}
	return args[0]
}
