package compiler

import (
	"fmt"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	llvmTypes "github.com/llir/llvm/ir/types"
	llvmValue "github.com/llir/llvm/ir/value"
	"github.com/zegl/tre/compiler/compiler/internal"
	"github.com/zegl/tre/compiler/compiler/name"
	"github.com/zegl/tre/compiler/compiler/types"
	"github.com/zegl/tre/compiler/compiler/value"
	"github.com/zegl/tre/compiler/parser"
)

func funcType(params, returnTypes []parser.TypeNode) (retType types.Type, treReturnTypes []types.Type, argTypes []*ir.Param, treParams []types.Type, isVariadicFunc bool, argumentReturnValuesCount int) {
	llvmParams := make([]*ir.Param, len(params))
	treParams = make([]types.Type, len(params))

	for k, par := range params {
		paramType := parserTypeToType(par)

		// Variadic arguments are converted into a slice
		// The function takes a slice as the argument, the caller has to convert
		// the arguments to a slice before calling
		if par.Variadic() {
			paramType = &types.Slice{
				Type:     paramType,
				LlvmType: internal.Slice(paramType.LLVM()),
			}
		}

		param := ir.NewParam(name.Var("p"), paramType.LLVM())

		if par.Variadic() {
			if k < len(params)-1 {
				panic("Only the last parameter can be varadic")
			}
			isVariadicFunc = true
		}

		llvmParams[k] = param
		treParams[k] = paramType
	}

	var funcRetType types.Type = types.Void

	// Amount of values returned via argument pointers
	// var argumentReturnValuesCount int
	// var treReturnTypes []types.Type

	// Use LLVM function return value if there's only one return value
	if len(returnTypes) == 1 {
		funcRetType = parserTypeToType(returnTypes[0])
		treReturnTypes = []types.Type{funcRetType}
	} else if len(returnTypes) > 0 {
		// Return values via argument pointers
		// The return values goes first
		var llvmReturnTypesParams []*ir.Param

		for _, ret := range returnTypes {
			t := parserTypeToType(ret)
			treReturnTypes = append(treReturnTypes, t)
			llvmReturnTypesParams = append(llvmReturnTypesParams, ir.NewParam(name.Var("ret"), llvmTypes.NewPointer(t.LLVM())))
		}

		// Add return values to the start
		treParams = append(treReturnTypes, treParams...)
		llvmParams = append(llvmReturnTypesParams, llvmParams...)

		argumentReturnValuesCount = len(returnTypes)
	}

	return funcRetType, treReturnTypes, llvmParams, treParams, isVariadicFunc, argumentReturnValuesCount
}

func (c *Compiler) compileDefineFuncNode(v *parser.DefineFuncNode) value.Value {
	var compiledName string

	if v.IsMethod {
		var methodOnType parser.TypeNode = v.MethodOnType

		if v.IsPointerReceiver {
			methodOnType = &parser.PointerTypeNode{ValueType: methodOnType}
		}

		// Add the type that we're a method on as the first argument
		v.Arguments = append([]*parser.NameNode{
			{
				Name: v.InstanceName,
				Type: methodOnType,
			},
		}, v.Arguments...)

		// Change the name of our function
		compiledName = c.currentPackageName + "_method_" + v.MethodOnType.TypeName + "_" + v.Name
	} else if v.IsNamed {
		compiledName = c.currentPackageName + "_" + v.Name
	} else {
		compiledName = c.currentPackageName + "_" + name.AnonFunc()
	}

	argTypes := make([]parser.TypeNode, len(v.Arguments))
	for k, v := range v.Arguments {
		argTypes[k] = v.Type
	}

	retTypes := make([]parser.TypeNode, len(v.ReturnValues))
	for k, v := range v.ReturnValues {
		retTypes[k] = v.Type
	}

	funcRetType, treReturnTypes, llvmParams, treParams, isVariadicFunc, argumentReturnValuesCount := funcType(argTypes, retTypes)

	if c.currentPackageName == "main" && v.Name == "main" {
		if len(v.ReturnValues) != 0 {
			panic("main func can not have a return type")
		}

		funcRetType = types.I32
		compiledName = "main"
	}

	fn := c.module.NewFunc(compiledName, funcRetType.LLVM(), llvmParams...)

	typesFunc := &types.Function{
		LlvmFunction:   fn,
		LlvmReturnType: funcRetType,
		ReturnTypes:    treReturnTypes,
		IsVariadic:     isVariadicFunc,
		ArgumentTypes:  treParams,
	}

	// Save as a method on the type
	if v.IsMethod {
		if t, ok := typeConvertMap[v.MethodOnType.TypeName]; ok {
			t.AddMethod(v.Name, &types.Method{
				Function:        typesFunc,
				PointerReceiver: v.IsPointerReceiver,
				MethodName:      v.Name,
			})
		} else {
			panic("save method on type failed")
		}

		// Make this method available in interfaces via a jump function
		typesFunc.JumpFunction = c.compileInterfaceMethodJump(fn)
	} else if v.IsNamed {
		c.currentPackage.Funcs[v.Name] = typesFunc
	}

	entry := fn.NewBlock(name.Block())

	prevContextFunc := c.contextFunc
	prevContextBlock := c.contextBlock

	c.contextFunc = typesFunc
	c.contextBlock = entry
	c.pushVariablesStack()

	// Push to the return values stack
	if argumentReturnValuesCount > 0 {
		var retVals []value.Value

		for i, retType := range treParams[:argumentReturnValuesCount] {
			retVals = append(retVals, value.Value{
				Value: llvmParams[i],
				Type:  retType,
				IsVariable: false,
			})
		}

		c.contextFuncRetVals = append(c.contextFuncRetVals, retVals)
	}

	// Save all parameters in the block mapping
	// Return value arguments are ignored
	for i, param := range llvmParams {
		var paramName string
		var dataType types.Type

		// Named return values
		if i < argumentReturnValuesCount {
			paramName = v.ReturnValues[i].Name
			dataType = treReturnTypes[i]
		} else {
			paramName = v.Arguments[i-argumentReturnValuesCount].Name
			dataType = treParams[i-argumentReturnValuesCount]
		}

		// Structs needs to be pointer-allocated
		if _, ok := param.Type().(*llvmTypes.StructType); ok {
			paramPtr := entry.NewAlloca(dataType.LLVM())
			paramPtr.SetName(name.Var("paramPtr"))
			entry.NewStore(param, paramPtr)

			c.setVar(paramName, value.Value{
				Value:      paramPtr,
				Type:       dataType,
				IsVariable: true,
			})

			continue
		}

		c.setVar(paramName, value.Value{
			Value:      param,
			Type:       dataType,
			IsVariable: false,
		})
	}

	// Single return value (not via parameters)
	// Add to variable block
	if len(v.ReturnValues) == 1 {
		r := v.ReturnValues[0]
		all := c.contextBlock.NewAlloca(funcRetType.LLVM())
		retVar := value.Value{
			Value: all,
			Type: funcRetType,
			IsVariable: true,
		}
		c.setVar(r.Name, retVar)
		c.contextFuncRetVals = append(c.contextFuncRetVals, []value.Value{retVar})
	}

	c.compile(v.Body)

	// Return void if there is no return type explicitly set
	if len(v.ReturnValues) == 0 {
		c.contextBlock.NewRet(nil)
	} else {
		// Pop return variables context
		c.contextFuncRetVals = c.contextFuncRetVals[0 : len(c.contextFuncRetVals)-1]
	}

	// Return 0 by default in main func
	if v.Name == "main" {
		c.contextBlock.NewRet(constant.NewInt(llvmTypes.I32, 0))
	}

	c.contextFunc = prevContextFunc
	c.contextBlock = prevContextBlock

	c.popVariablesStack()

	return value.Value{
		Type:  typesFunc,
		Value: typesFunc.LlvmFunction,
	}
}

func (c *Compiler) compileInterfaceMethodJump(targetFunc *ir.Func) *ir.Func {
	// Copy parameter types so that we can modify them
	params := make([]*ir.Param, len(targetFunc.Sig.Params))
	for i, p := range targetFunc.Params {
		params[i] = ir.NewParam("", p.Type())
	}

	originalType := targetFunc.Params[0].Type()
	_, isPointerType := originalType.(*llvmTypes.PointerType)
	if !isPointerType {
		originalType = llvmTypes.NewPointer(originalType)
	}

	// Replace the first parameter type with an *i8
	// Will be bitcasted later to the target type
	params[0] = ir.NewParam("unsafe-ptr", llvmTypes.NewPointer(llvmTypes.I8))

	fn := c.module.NewFunc(targetFunc.Name()+"_jump", targetFunc.Sig.RetType, params...)
	block := fn.NewBlock(name.Block())

	var bitcasted llvmValue.Value = block.NewBitCast(params[0], originalType)

	// TODO: Don't do this if the method has a pointer receiver
	if !isPointerType {
		bitcasted = block.NewLoad(bitcasted)
	}

	callArgs := []llvmValue.Value{bitcasted}
	for _, p := range params[1:] {
		callArgs = append(callArgs, p)
	}

	resVal := block.NewCall(targetFunc, callArgs...)

	if _, ok := targetFunc.Sig.RetType.(*llvmTypes.VoidType); ok {
		block.NewRet(nil)
	} else {
		block.NewRet(resVal)
	}

	return fn
}

func (c *Compiler) compileReturnNode(v *parser.ReturnNode) {
	// Single variable return
	if len(v.Vals) == 1 {
		// Set value and jump to return block
		val := c.compileValue(v.Vals[0])

		// Type cast if necessary
		val = c.valueToInterfaceValue(val, c.contextFunc.LlvmReturnType)

		if val.IsVariable {
			c.contextBlock.NewRet(c.contextBlock.NewLoad(val.Value))
			return
		}

		c.contextBlock.NewRet(val.Value)
		return
	}

	// Multiple value returns
	if len(v.Vals) > 1 {
		for i, val := range v.Vals {
			compVal := c.compileValue(val)

			// TODO: Type cast if necessary
			// compVal = c.valueToInterfaceValue(compVal, c.contextFunc.ReturnType)

			retVal := compVal.Value

			if compVal.IsVariable {
				retVal = c.contextBlock.NewLoad(retVal)
			}

			// Assign to ptr
			retValPtr := c.contextFuncRetVals[len(c.contextFuncRetVals)-1][i]

			c.contextBlock.NewStore(retVal, retValPtr.Value)
		}

		c.contextBlock.NewRet(nil)
		return
	}


	// Naked return, func has one named return variable
	if len(v.Vals) == 0 {
		retVals := c.contextFuncRetVals[len(c.contextFuncRetVals)-1]
		if len(retVals) == 1 {
			val := retVals[0].Value
			if retVals[0].IsVariable {
				val = c.contextBlock.NewLoad(val)
			}
			c.contextBlock.NewRet(val)
			return
		}
	}

	// Return void in LLVM function
	c.contextBlock.NewRet(nil)
}

func (c *Compiler) compileCallNode(v *parser.CallNode) value.Value {
	var args []value.Value

	name, isNameNode := v.Function.(*parser.NameNode)

	// len() functions
	if isNameNode && name.Name == "len" {
		return c.lenFuncCall(v)
	}

	// cap() function
	if isNameNode && name.Name == "cap" {
		return c.capFuncCall(v)
	}

	// append() function
	if isNameNode && name.Name == "append" {
		return c.appendFuncCall(v)
	}

	var fnType *types.Function
	var fn llvmValue.Named

	if isNameNode {

		// Check if it's a function
		namedFn := c.varByName(name.Name)
		if ft, ok := namedFn.Type.(*types.Function); ok {
			fnType = ft

			// TODO: Get rid of this difference
			// The function should always be in the value, not the type
			if ft.LlvmFunction.Name() == "UNNAMEDFUNC" {
				fn = namedFn.Value.(llvmValue.Named)
			} else {
				fn = ft.LlvmFunction
			}

		} else {
			funcByVal := c.compileValue(v.Function)
			if checkIfFunc, ok := funcByVal.Type.(*types.Function); ok {
				fnType = checkIfFunc
				fn = checkIfFunc.LlvmFunction
			} else {
				panic(fmt.Sprintf("no such function: %v", v))
			}
		}

	} else {
		funcByVal := c.compileValue(v.Function)
		if checkIfFunc, ok := funcByVal.Type.(*types.Function); ok {
			fnType = checkIfFunc
			fn = checkIfFunc.LlvmFunction
		} else if checkIfMethod, ok := funcByVal.Type.(*types.Method); ok {
			fnType = checkIfMethod.Function
			fn = checkIfMethod.Function.LlvmFunction

			var methodCallArgs []value.Value

			// Should be loaded if the method is not a pointer receiver
			funcByVal.IsVariable = !checkIfMethod.PointerReceiver

			// Add instance as the first argument
			methodCallArgs = append(methodCallArgs, funcByVal)
			methodCallArgs = append(methodCallArgs, args...)
			args = methodCallArgs
		} else if ifaceMethod, ok := funcByVal.Type.(*types.InterfaceMethod); ok {

			ifaceInstance := c.contextBlock.NewGetElementPtr(funcByVal.Value, constant.NewInt(llvmTypes.I32, 0), constant.NewInt(llvmTypes.I32, 0))
			ifaceInstanceLoad := c.contextBlock.NewLoad(ifaceInstance)

			// Add instance as the first argument
			var methodCallArgs []value.Value
			methodCallArgs = append(methodCallArgs, value.Value{
				Value: ifaceInstanceLoad,
			})
			methodCallArgs = append(methodCallArgs, args...)
			args = methodCallArgs

			var returnType types.Type
			if len(ifaceMethod.ReturnTypes) > 0 {
				returnType = ifaceMethod.ReturnTypes[0]
			} else {
				returnType = types.Void
			}

			fnType = &types.Function{
				// TODO: We probably need to add more fields here?
				LlvmFunction:   ifaceMethod.LlvmJumpFunction,
				LlvmReturnType: returnType,
			}
			fn = ifaceMethod.LlvmJumpFunction
		} else {
			panic("expected function or method, got something else")
		}
	}

	// If the last argument is a slice that is "de variadicified"
	// Eg: foo...
	// When this is the case we don't have to convert the arguments to a slice when calling the func
	lastIsVariadicSlice := false

	// Compile all values
	for _, vv := range v.Arguments {
		if devVar, ok := vv.(*parser.DeVariadicSliceNode); ok {
			lastIsVariadicSlice = true
			args = append(args, c.compileValue(devVar.Item))
			continue
		}
		args = append(args, c.compileValue(vv))
	}

	// Convert variadic arguments to a slice when needed
	if fnType.IsVariadic && !lastIsVariadicSlice {
		// Only the last argument can be variadic
		variadicArgIndex := len(fnType.ArgumentTypes) - 1
		variadicType := fnType.ArgumentTypes[variadicArgIndex].(*types.Slice)

		// Convert last argument to a slice.
		variadicSlice := c.compileInitializeSliceWithValues(variadicType.Type, args[variadicArgIndex:]...)

		// Remove "pre-sliceified" arguments from the list of arguments
		args = args[0:variadicArgIndex]
		args = append(args, variadicSlice)
	}

	// Convert all values to LLVM values
	// Load the variable if needed
	llvmArgs := make([]llvmValue.Value, len(args))
	for i, v := range args {

		// Convert type to interface type if needed
		if len(fnType.ArgumentTypes) > i {
			v = c.valueToInterfaceValue(v, fnType.ArgumentTypes[i])
		}

		val := v.Value

		if v.IsVariable {
			val = c.contextBlock.NewLoad(val)
		}

		// Convert strings and arrays to i8* when calling external functions
		if fnType.IsExternal {
			if v.Type.Name() == "string" {
				llvmArgs[i] = c.contextBlock.NewExtractValue(val, 1)
				continue
			}

			if v.Type.Name() == "array" {
				llvmArgs[i] = c.contextBlock.NewExtractValue(val, 1)
				continue
			}
		}

		llvmArgs[i] = val
	}

	// Functions with multiple return values are using pointers via arguments
	// Alloc the values here and add pointers to the list of arguments
	var multiValues []value.Value
	if len(fnType.ReturnTypes) > 1 {
		var retValAllocas []llvmValue.Value

		for _, retType := range fnType.ReturnTypes {
			alloca := c.contextBlock.NewAlloca(retType.LLVM())
			retValAllocas = append(retValAllocas, alloca)

			multiValues = append(multiValues, value.Value{
				Type:       retType,
				Value:      alloca,
				IsVariable: true,
			})
		}

		// Add to start of argument list
		llvmArgs = append(retValAllocas, llvmArgs...)
	}

	funcCallRes := c.contextBlock.NewCall(fn, llvmArgs...)

	// 0 or 1 return variables
	if len(fnType.ReturnTypes) < 2 {
		return value.Value{
			Value: funcCallRes,
			Type:  fnType.LlvmReturnType,
		}
	}

	// 2 or more return variables
	return value.Value{
		Type:        &types.MultiValue{Types: fnType.ReturnTypes},
		MultiValues: multiValues,
	}
}
