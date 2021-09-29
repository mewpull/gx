package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Compiler struct {
	directoryPath string
	filePaths     []string

	fileSet *token.FileSet
	files   []*ast.File
	types   *types.Info

	fieldIndices map[*types.Var]int
	genTypeExprs map[types.Type]string
	genTypeDecls map[*ast.TypeSpec]string
	genTypeDefns map[*ast.TypeSpec]string
	genFuncDecls map[*ast.FuncDecl]string

	indent     int
	errors     *strings.Builder
	output     *strings.Builder
	atBlockEnd bool
}

//
// Error and writing utilities
//

func (c *Compiler) errorf(pos token.Pos, format string, args ...interface{}) {
	fmt.Fprintf(c.errors, "%s: ", c.fileSet.PositionFor(pos, true))
	fmt.Fprintf(c.errors, format, args...)
	fmt.Fprintln(c.errors)
}

func (c *Compiler) errored() bool {
	return c.errors.Len() != 0
}

func (c *Compiler) write(s string) {
	c.atBlockEnd = false
	if peek := c.output.String(); len(peek) > 0 && peek[len(peek)-1] == '\n' {
		for i := 0; i < 2*c.indent; i++ {
			c.output.WriteByte(' ')
		}
	}
	c.output.WriteString(s)
}

func trimFinalSpace(s string) string {
	if l := len(s); l > 0 && s[l-1] == ' ' {
		return s[0 : l-1]
	} else {
		return s
	}
}

//
// Types
//

func (c *Compiler) computeFieldIndices(typ *types.Struct) {
	nFields := typ.NumFields()
	if nFields == 0 {
		return
	}
	if _, ok := c.fieldIndices[typ.Field(0)]; ok {
		return
	}
	for i := 0; i < nFields; i++ {
		c.fieldIndices[typ.Field(i)] = i
	}
}

func (c *Compiler) genTypeExpr(typ types.Type, pos token.Pos) string {
	if result, ok := c.genTypeExprs[typ]; ok {
		return result
	} else {
		builder := &strings.Builder{}
		switch typ := typ.(type) {
		case *types.Basic:
			switch typ.Kind() {
			case types.Bool:
				builder.WriteString("bool")
			case types.Int:
				builder.WriteString("int")
			case types.Float32:
				builder.WriteString("float")
			case types.Float64:
				builder.WriteString("double")
			default:
				c.errorf(pos, "%s not supported", typ.String())
			}
			builder.WriteByte(' ')
		case *types.Pointer:
			builder.WriteString(c.genTypeExpr(typ.Elem(), pos))
			builder.WriteByte('*')
		case *types.Named:
			builder.WriteString(typ.Obj().Name())
			if typeArgs := typ.TypeArgs(); typeArgs != nil {
				builder.WriteString("<")
				for i, nTypeArgs := 0, typeArgs.Len(); i < nTypeArgs; i++ {
					if i > 0 {
						builder.WriteString(", ")
					}
					builder.WriteString(trimFinalSpace(c.genTypeExpr(typeArgs.At(i), pos)))
				}
				builder.WriteString(">")
			}
			builder.WriteByte(' ')
		case *types.TypeParam:
			builder.WriteString(typ.Obj().Name())
			builder.WriteByte(' ')
		case *types.Signature:
			builder.WriteString("auto &&")
		case *types.Array:
			builder.WriteString("gx::Array<")
			builder.WriteString(trimFinalSpace(c.genTypeExpr(typ.Elem(), pos)))
			builder.WriteString(", ")
			builder.WriteString(strconv.FormatInt(typ.Len(), 10))
			builder.WriteString(">")
			builder.WriteByte(' ')
		case *types.Slice:
			builder.WriteString("gx::Slice<")
			builder.WriteString(trimFinalSpace(c.genTypeExpr(typ.Elem(), pos)))
			builder.WriteString(">")
			builder.WriteByte(' ')
		default:
			c.errorf(pos, "%s not supported", typ.String())
		}
		result = builder.String()
		c.genTypeExprs[typ] = result
		return result
	}
}

func (c *Compiler) genTypeDecl(typeSpec *ast.TypeSpec) string {
	if result, ok := c.genTypeDecls[typeSpec]; ok {
		return result
	} else {
		builder := &strings.Builder{}
		if typeSpec.TypeParams != nil {
			builder.WriteString("template<")
			first := true
			for _, typeParam := range typeSpec.TypeParams.List {
				for _, name := range typeParam.Names {
					if !first {
						builder.WriteString(", ")
					}
					first = false
					builder.WriteString("typename ")
					builder.WriteString(name.String())
				}
			}
			builder.WriteString(">\n")
		}
		switch typeSpec.Type.(type) {
		case *ast.StructType:
			builder.WriteString("struct ")
			builder.WriteString(typeSpec.Name.String())
		case *ast.InterfaceType:
			// Nothing to do -- only used as generic constraint during typecheck
		default:
			builder.WriteString("using ")
			builder.WriteString(typeSpec.Name.String())
			builder.WriteString(" = ")
			typ := c.types.TypeOf(typeSpec.Type)
			builder.WriteString(trimFinalSpace(c.genTypeExpr(typ, typeSpec.Type.Pos())))
		}
		result = builder.String()
		c.genTypeDecls[typeSpec] = result
		return result
	}
}

func (c *Compiler) genTypeDefn(typeSpec *ast.TypeSpec) string {
	if result, ok := c.genTypeDefns[typeSpec]; ok {
		return result
	} else {
		builder := &strings.Builder{}
		switch typ := typeSpec.Type.(type) {
		case *ast.StructType:
			builder.WriteString(c.genTypeDecl(typeSpec))
			builder.WriteString(" {\n")
			for _, field := range typ.Fields.List {
				if typ := c.types.TypeOf(field.Type); typ != nil {
					typeExpr := c.genTypeExpr(typ, field.Type.Pos())
					for _, fieldName := range field.Names {
						builder.WriteString("  ")
						builder.WriteString(typeExpr)
						builder.WriteString(fieldName.String())
						builder.WriteString(";\n")
					}
				}
			}
			builder.WriteByte('}')
		case *ast.InterfaceType:
			// Nothing to do -- only used as generic constraint during typecheck
		default:
			// Nothing to do -- alias declaration is a definition
		}
		result = builder.String()
		c.genTypeDefns[typeSpec] = result
		return result
	}
}

//
// Functions
//

var sizes = types.SizesFor("gc", "wasm")

func sizeOf(typ types.Type) int64 {
	defer func() { recover() }()
	return sizes.Sizeof(typ)
}

func (c *Compiler) genFuncDecl(decl *ast.FuncDecl) string {
	if result, ok := c.genFuncDecls[decl]; ok {
		return result
	} else {
		obj := c.types.Defs[decl.Name]
		sig := obj.Type().(*types.Signature)
		recv := sig.Recv()

		builder := &strings.Builder{}

		// Type parameters
		addTypeParams := func(typeParams *types.TypeParamList) {
			if typeParams != nil {
				builder.WriteString("template<")
				for i, nTypeParams := 0, typeParams.Len(); i < nTypeParams; i++ {
					if i > 0 {
						builder.WriteString(", ")
					}
					builder.WriteString("typename ")
					builder.WriteString(typeParams.At(i).Obj().Name())
				}
				builder.WriteString(">\n")
			}
		}
		if recv != nil {
			switch recvType := recv.Type().(type) {
			case *types.Named:
				addTypeParams(recvType.TypeParams())
			case *types.Pointer:
				switch elemType := recvType.Elem().(type) {
				case *types.Named:
					addTypeParams(elemType.TypeParams())
				}
			}
		}
		addTypeParams(sig.TypeParams())

		// Return type
		if rets := sig.Results(); rets.Len() > 1 {
			c.errorf(decl.Type.Results.Pos(), "multiple return values not supported")
		} else if rets.Len() == 1 {
			ret := rets.At(0)
			builder.WriteString(c.genTypeExpr(ret.Type(), ret.Pos()))
		} else {
			if obj.Pkg().Name() == "main" && decl.Name.String() == "main" && recv == nil {
				builder.WriteString("int ")
			} else {
				builder.WriteString("void ")
			}
		}

		// Name
		builder.WriteString(decl.Name.String())

		// Parameters
		builder.WriteByte('(')
		addParam := func(param *types.Var) {
			typ := param.Type()
			switch typ.Underlying().(type) {
			case *types.Array:
				c.errorf(param.Pos(), "cannot pass array by value, use pointer to array *%s instead", typ)
			case *types.Slice:
				c.errorf(param.Pos(), "cannot pass slice by value, use pointer to slice *%s instead", typ)
			}
			if size := sizeOf(typ); size > 96 {
				c.errorf(param.Pos(), "%s is large (%d bytes), consider passing by pointer instead", typ, size)
			}
			builder.WriteString(c.genTypeExpr(typ, param.Pos()))
			builder.WriteString(param.Name())
		}
		if recv != nil {
			addParam(recv)
		}
		for i, nParams := 0, sig.Params().Len(); i < nParams; i++ {
			if i > 0 || recv != nil {
				builder.WriteString(", ")
			}
			addParam(sig.Params().At(i))
		}
		builder.WriteByte(')')

		result = builder.String()
		c.genFuncDecls[decl] = result
		return result
	}
}

//
// Expressions
//

func (c *Compiler) writeIdent(ident *ast.Ident) {
	if c.types.Types[ident].IsBuiltin() {
		c.write("gx::")
	}
	c.write(ident.Name)
}

func (c *Compiler) writeBasicLit(lit *ast.BasicLit) {
	switch lit.Kind {
	case token.INT, token.FLOAT, token.STRING:
		c.write(lit.Value)
	default:
		c.errorf(lit.Pos(), "unsupported literal kind")
	}
}

func (c *Compiler) writeFuncLit(lit *ast.FuncLit) {
	sig := c.types.TypeOf(lit).(*types.Signature)
	c.write("[&](")
	for i, nParams := 0, sig.Params().Len(); i < nParams; i++ {
		if i > 0 {
			c.write(", ")
		}
		param := sig.Params().At(i)
		c.write(c.genTypeExpr(param.Type(), param.Pos()))
		c.write(param.Name())
	}
	c.write(") ")
	c.writeBlockStmt(lit.Body)
	c.atBlockEnd = false
}

func (c *Compiler) writeCompositeLit(lit *ast.CompositeLit) {
	c.write(c.genTypeExpr(c.types.TypeOf(lit), lit.Pos()))
	c.write("{")
	if len(lit.Elts) > 0 {
		if _, ok := lit.Elts[0].(*ast.KeyValueExpr); ok {
			if typ, ok := c.types.TypeOf(lit.Type).Underlying().(*types.Struct); ok {
				c.computeFieldIndices(typ)
				lastIndex := 0
				for _, elt := range lit.Elts {
					field := c.types.ObjectOf(elt.(*ast.KeyValueExpr).Key.(*ast.Ident)).(*types.Var)
					if index := c.fieldIndices[field]; index < lastIndex {
						c.errorf(lit.Pos(), "struct literal fields must appear in definition order")
						break
					} else {
						lastIndex = index
					}
				}
			}
		}
		if c.fileSet.Position(lit.Pos()).Line == c.fileSet.Position(lit.Elts[0].Pos()).Line {
			c.write(" ")
			for i, elt := range lit.Elts {
				if i > 0 {
					c.write(", ")
				}
				c.writeExpr(elt)
			}
			c.write(" ")
		} else {
			c.write("\n")
			c.indent++
			for _, elt := range lit.Elts {
				c.writeExpr(elt)
				c.write(",\n")
			}
			c.indent--
		}
	}
	c.write("}")
}

func (c *Compiler) writeParenExpr(bin *ast.ParenExpr) {
	c.write("(")
	c.writeExpr(bin.X)
	c.write(")")
}

func (c *Compiler) writeSelectorExpr(sel *ast.SelectorExpr) {
	c.writeExpr(sel.X)
	if _, ok := c.types.TypeOf(sel.X).(*types.Pointer); ok {
		c.write("->")
	} else {
		c.write(".")
	}
	c.writeIdent(sel.Sel)
}

func (c *Compiler) writeIndexExpr(ind *ast.IndexExpr) {
	if _, ok := c.types.TypeOf(ind.X).(*types.Pointer); ok {
		c.write("(*(")
		c.writeExpr(ind.X)
		c.write("))")
	} else {
		c.writeExpr(ind.X)
	}
	c.write("[")
	c.writeExpr(ind.Index)
	c.write("]")
}

func (c *Compiler) writeCallExpr(call *ast.CallExpr) {
	method := false
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if sig, ok := c.types.Uses[sel.Sel].Type().(*types.Signature); ok && sig.Recv() != nil {
			method = true
			c.writeIdent(sel.Sel)
			c.write("(")
			_, xPtr := c.types.TypeOf(sel.X).(*types.Pointer)
			_, recvPtr := sig.Recv().Type().(*types.Pointer)
			if xPtr && !recvPtr {
				c.write("*(")
				c.writeExpr(sel.X)
				c.write(")")
			} else if !xPtr && recvPtr {
				c.write("&(")
				c.writeExpr(sel.X)
				c.write(")")
			} else {
				c.writeExpr(sel.X)
			}
		}
	}
	if !method {
		c.writeExpr(call.Fun)
		if ident, ok := call.Fun.(*ast.Ident); ok {
			if typeArgs := c.types.Instances[ident].TypeArgs; typeArgs != nil {
				c.write("<")
				for i, nTypeArgs := 0, typeArgs.Len(); i < nTypeArgs; i++ {
					if i > 0 {
						c.write(", ")
					}
					c.write(trimFinalSpace(c.genTypeExpr(typeArgs.At(i), call.Fun.Pos())))
				}
				c.write(">")
			}
		}
		c.write("(")
	}
	for i, arg := range call.Args {
		if i > 0 || method {
			c.write(", ")
		}
		c.writeExpr(arg)
	}
	c.write(")")
}

func (c *Compiler) writeStarExpr(star *ast.StarExpr) {
	c.write("*")
	c.writeExpr(star.X)
}

func (c *Compiler) writeUnaryExpr(un *ast.UnaryExpr) {
	switch op := un.Op; op {
	case token.ADD, token.SUB, token.NOT:
		c.write(op.String())
	case token.AND:
		if !c.types.Types[un.X].Addressable() {
			c.errorf(un.OpPos, "cannot take address of a temporary object")
		}
		c.write(op.String())
	default:
		c.errorf(un.OpPos, "unsupported unary operator")
	}
	c.writeExpr(un.X)
}

func (c *Compiler) writeBinaryExpr(bin *ast.BinaryExpr) {
	c.writeExpr(bin.X)
	c.write(" ")
	switch op := bin.Op; op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ,
		token.ADD, token.SUB, token.MUL, token.QUO, token.REM,
		token.AND, token.OR, token.XOR, token.SHL, token.SHR:
		c.write(op.String())
	default:
		c.errorf(bin.OpPos, "unsupported binary operator")
	}
	c.write(" ")
	c.writeExpr(bin.Y)
}

func (c *Compiler) writeKeyValueExpr(kv *ast.KeyValueExpr) {
	if name, ok := kv.Key.(*ast.Ident); !ok {
		c.errorf(kv.Pos(), "unsupported literal key")
	} else {
		c.write(".")
		c.write(name.String())
		c.write(" = ")
		c.writeExpr(kv.Value)
	}
}

func (c *Compiler) writeExpr(expr ast.Expr) {
	switch expr := expr.(type) {
	case *ast.Ident:
		c.writeIdent(expr)
	case *ast.BasicLit:
		c.writeBasicLit(expr)
	case *ast.FuncLit:
		c.writeFuncLit(expr)
	case *ast.CompositeLit:
		c.writeCompositeLit(expr)
	case *ast.ParenExpr:
		c.writeParenExpr(expr)
	case *ast.SelectorExpr:
		c.writeSelectorExpr(expr)
	case *ast.IndexExpr:
		c.writeIndexExpr(expr)
	case *ast.CallExpr:
		c.writeCallExpr(expr)
	case *ast.StarExpr:
		c.writeStarExpr(expr)
	case *ast.UnaryExpr:
		c.writeUnaryExpr(expr)
	case *ast.BinaryExpr:
		c.writeBinaryExpr(expr)
	case *ast.KeyValueExpr:
		c.writeKeyValueExpr(expr)
	default:
		c.errorf(expr.Pos(), "unsupported expression type")
	}
}

//
// Statements
//

func (c *Compiler) writeExprStmt(exprStmt *ast.ExprStmt) {
	c.writeExpr(exprStmt.X)
}

func (c *Compiler) writeAssignStmt(assignStmt *ast.AssignStmt) {
	if len(assignStmt.Lhs) != 1 {
		c.errorf(assignStmt.Pos(), "multi-value assignment unsupported")
		return
	}
	if assignStmt.Tok == token.DEFINE {
		c.write("auto ")
	}
	c.writeExpr(assignStmt.Lhs[0])
	c.write(" ")
	switch op := assignStmt.Tok; op {
	case token.DEFINE:
		c.write("=")
	case token.ASSIGN,
		token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN, token.REM_ASSIGN,
		token.AND_ASSIGN, token.OR_ASSIGN, token.XOR_ASSIGN, token.SHL_ASSIGN, token.SHR_ASSIGN:
		c.write(op.String())
	default:
		c.errorf(assignStmt.TokPos, "unsupported assignment operator")
	}
	c.write(" ")
	c.writeExpr(assignStmt.Rhs[0])
}

func (c *Compiler) writeReturnStmt(retStmt *ast.ReturnStmt) {
	if len(retStmt.Results) > 1 {
		c.errorf(retStmt.Results[0].Pos(), "multiple return values not supported")
	} else if len(retStmt.Results) == 1 {
		c.write("return ")
		c.writeExpr(retStmt.Results[0])
	} else {
		c.write("return")
	}
}

func (c *Compiler) writeBranchStmt(branchStmt *ast.BranchStmt) {
	switch tok := branchStmt.Tok; tok {
	case token.BREAK, token.CONTINUE:
		c.write(tok.String())
	default:
		c.errorf(branchStmt.TokPos, "unsupported branch statement")
	}
}

func (c *Compiler) writeBlockStmt(block *ast.BlockStmt) {
	c.write("{\n")
	c.indent++
	c.writeStmtList(block.List)
	c.indent--
	c.write("}")
	c.atBlockEnd = true
}

func (c *Compiler) writeIfStmt(ifStmt *ast.IfStmt) {
	c.write("if (")
	if ifStmt.Init != nil {
		c.writeStmt(ifStmt.Init)
		c.write("; ")
	}
	c.writeExpr(ifStmt.Cond)
	c.write(") ")
	c.writeStmt(ifStmt.Body)
	if ifStmt.Else != nil {
		c.write(" else ")
		c.writeStmt(ifStmt.Else)
	}
}

func (c *Compiler) writeForStmt(forStmt *ast.ForStmt) {
	c.write("for (")
	if forStmt.Init != nil {
		c.writeStmt(forStmt.Init)
	}
	c.write("; ")
	if forStmt.Cond != nil {
		c.writeExpr(forStmt.Cond)
	}
	c.write("; ")
	if forStmt.Post != nil {
		c.writeStmt(forStmt.Post)
	}
	c.write(") ")
	c.writeStmt(forStmt.Body)
}

func (c *Compiler) writeStmt(stmt ast.Stmt) {
	switch stmt := stmt.(type) {
	case *ast.ExprStmt:
		c.writeExprStmt(stmt)
	case *ast.AssignStmt:
		c.writeAssignStmt(stmt)
	case *ast.ReturnStmt:
		c.writeReturnStmt(stmt)
	case *ast.BranchStmt:
		c.writeBranchStmt(stmt)
	case *ast.BlockStmt:
		c.writeBlockStmt(stmt)
	case *ast.IfStmt:
		c.writeIfStmt(stmt)
	case *ast.ForStmt:
		c.writeForStmt(stmt)
	default:
		c.errorf(stmt.Pos(), "unsupported statement type")
	}
}

func (c *Compiler) writeStmtList(list []ast.Stmt) {
	for _, stmt := range list {
		c.writeStmt(stmt)
		if !c.atBlockEnd {
			c.write(";")
		}
		c.write("\n")
	}
}

//
// Top-level
//

func (c *Compiler) compile() {
	// Collect file paths from directory
	if len(c.filePaths) == 0 && c.directoryPath != "" {
		fileInfos, err := ioutil.ReadDir(c.directoryPath)
		if err != nil {
			fmt.Fprintf(c.errors, "%s\n", err)
			return
		}
		for _, fileInfo := range fileInfos {
			c.filePaths = append(c.filePaths, filepath.Join(c.directoryPath, fileInfo.Name()))
		}
	}

	// Initialize maps
	c.fieldIndices = make(map[*types.Var]int)
	c.genTypeExprs = make(map[types.Type]string)
	c.genTypeDecls = make(map[*ast.TypeSpec]string)
	c.genTypeDefns = make(map[*ast.TypeSpec]string)
	c.genFuncDecls = make(map[*ast.FuncDecl]string)

	// Initialize builders
	c.errors = &strings.Builder{}
	c.output = &strings.Builder{}

	// Parse
	c.fileSet = token.NewFileSet()
	for _, filePath := range c.filePaths {
		file, err := parser.ParseFile(c.fileSet, filePath, nil, 0)
		if err != nil {
			fmt.Fprintf(c.errors, "%s\n", err)
			return
		}
		c.files = append(c.files, file)
	}

	// Type-check
	c.types = &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Instances:  make(map[*ast.Ident]types.Instance),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Scopes:     make(map[ast.Node]*types.Scope),
	}
	if _, err := (&types.Config{}).Check("", c.fileSet, c.files, c.types); err != nil {
		fmt.Fprintf(c.errors, "%s\n", err)
		return
	}

	// `#include`s
	c.write("#include \"gx.hh\"\n")

	// Collect type specs
	var typeSpecs []*ast.TypeSpec
	{
		typeSpecTopLevel := make(map[*ast.TypeSpec]bool)
		for _, file := range c.files {
			for _, decl := range file.Decls {
				if decl, ok := decl.(*ast.GenDecl); ok {
					for _, spec := range decl.Specs {
						if typeSpec, ok := spec.(*ast.TypeSpec); ok {
							typeSpecTopLevel[typeSpec] = true
						}
					}
				}
			}
		}
		typeSpecCollected := make(map[*ast.TypeSpec]bool)
		var collectTypeSpecs func(node ast.Node) bool
		collectTypeSpecs = func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && ident.Obj != nil && ident.Obj.Decl != nil {
				if typeSpec, ok := ident.Obj.Decl.(*ast.TypeSpec); ok {
					if typeSpecTopLevel[typeSpec] && !typeSpecCollected[typeSpec] {
						typeSpecCollected[typeSpec] = true
						ast.Inspect(typeSpec, collectTypeSpecs)
						typeSpecs = append(typeSpecs, typeSpec)
					}
				}
			}
			return true
		}
		for _, file := range c.files {
			for _, decl := range file.Decls {
				if decl, ok := decl.(*ast.FuncDecl); ok {
					ast.Inspect(decl, collectTypeSpecs)
				}
			}
		}
	}

	// Type declarations
	c.write("\n\n")
	for _, typeSpec := range typeSpecs {
		if typeDecl := c.genTypeDecl(typeSpec); typeDecl != "" {
			c.write(typeDecl)
			c.write(";\n")
		}
	}

	// Type definitions
	c.write("\n")
	for _, typeSpec := range typeSpecs {
		if typeDefn := c.genTypeDefn(typeSpec); typeDefn != "" {
			c.write("\n")
			c.write(typeDefn)
			c.write(";\n")
		}
	}

	// Function declarations
	c.write("\n\n")
	for _, file := range c.files {
		for _, decl := range file.Decls {
			if decl, ok := decl.(*ast.FuncDecl); ok {
				c.write(c.genFuncDecl(decl))
				c.write(";\n")
			}
		}
	}

	// Function definitions
	c.write("\n")
	for _, file := range c.files {
		for _, decl := range file.Decls {
			if decl, ok := decl.(*ast.FuncDecl); ok {
				if decl.Body != nil {
					c.write("\n")
					c.write(c.genFuncDecl(decl))
					c.write(" ")
					c.writeBlockStmt(decl.Body)
					c.write("\n")
				}
			}
		}
	}
}

//
// Main
//

func main() {
	// Arguments
	if len(os.Args) != 3 {
		fmt.Println("usage: gx <input_directory> <output_file>")
		return
	}
	directoryPath := os.Args[1]
	outputPath := os.Args[2]

	// Compile
	c := Compiler{directoryPath: directoryPath}
	c.compile()

	// Print output
	if c.errored() {
		fmt.Println(c.errors)
	} else {
		ioutil.WriteFile(outputPath, []byte(c.output.String()), 0644)
	}
}
