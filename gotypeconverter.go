package gotypeconverter

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fatih/structtag"
	"github.com/gostaticanalysis/codegen"
	"golang.org/x/tools/imports"
)

const doc = "gotypeconverter generates a function that converts two different named types."

var (
	flagOutput string

	flagSrc, flagDst, flagPkg, flagStructTag string

	tmpFilePath    string
	uniqueFuncName string

	ops uint64 = 0
)

func init() {
	Generator.Flags.StringVar(&flagOutput, "o", "", "output file; if nil, output stdout")
	Generator.Flags.StringVar(&flagSrc, "s", "", "source type")
	Generator.Flags.StringVar(&flagDst, "d", "", "destination type")
	Generator.Flags.StringVar(&flagPkg, "pkg", "", "output package; if nil, the directoryName and packageName must be same and will be used")
	Generator.Flags.StringVar(&flagStructTag, "structTag", "cvt", "")
}

func CreateTmpFile(path string) {
	ops = 0

	// tmpFilePath = path + "/tmp-001.go"
	rand.Seed(time.Now().UnixNano())
	tmpFilePath = fmt.Sprintf("%s/tmp%03d.go", path, rand.Int63n(1e3))
	fullPath, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}

	pkg := flagPkg
	if flagPkg == "" {
		pkg = filepath.Base(fullPath)
	}

	src := fmt.Sprintf("package %s\n", pkg)
	uniqueFuncName = fmt.Sprintf("unique%03d", rand.Int63n(1e3))
	src += fmt.Sprintf("func %s(){var (a %s\n b %s\n)\nfmt.Println(a, b)}\n",
		uniqueFuncName, flagSrc, flagDst)

	// goimports do not imports from go.mod
	res, err := imports.Process(tmpFilePath, []byte(src), &imports.Options{
		Fragment: true,
	})
	if err != nil {
		panic(err)
	}
	err = ioutil.WriteFile(tmpFilePath, res, 0755)
	if err != nil {
		panic(err)
	}

}

// Init 解析のための一時ファイルを作成する
func Init() {
	err := Generator.Flags.Parse(os.Args[1:])
	if err != nil {
		panic(err)
	}

	if Generator.Flags.NArg() == 0 {
		return
	}

	path := os.Args[len(os.Args)-1]
	CreateTmpFile(path)
}

var Generator = &codegen.Generator{
	Name:             "gotypeconverter",
	Doc:              doc,
	Run:              run,
	RunDespiteErrors: true,
}

func run(pass *codegen.Pass) error {
	// delete tmp file
	defer func() {
		os.Remove(tmpFilePath)
	}()

	var srcAST, dstAST ast.Expr
	existTargeFile := false
	for _, f := range pass.Files {
		// TODO read tmp*.go only
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fd.Name.Name != uniqueFuncName {
					continue
				}

				existTargeFile = true

				//ast.Inspect(fd, func(n ast.Node) bool {
				//ast.Print(pass.Fset, n)
				//fmt.Println() // \n したい...
				//return false
				//})

				ast.Inspect(fd, func(n ast.Node) bool {
					if gd, ok := n.(*ast.GenDecl); ok {
						for _, s := range gd.Specs {
							s, ok := s.(*ast.ValueSpec)
							if !ok {
								return false
							}
							switch s.Names[0].Name {
							case "a":
								srcAST = s.Type
							case "b":
								dstAST = s.Type
							}
						}
					}
					return true
				})
				break
			}
		}
		if existTargeFile {
			break
		}
	}

	if !existTargeFile {
		// 解析対象のpassでない
		return nil
	}

	if srcAST == nil || dstAST == nil {
		return errors.New("-s or -d are invalid")
	}
	if atomic.LoadUint64(&ops) != 0 {
		return nil
	}
	// ファイルを書くのは、一回のみ
	atomic.AddUint64(&ops, 1)

	buf := &bytes.Buffer{}

	srcType := pass.TypesInfo.TypeOf(srcAST)
	dstType := pass.TypesInfo.TypeOf(dstAST)
	// 生成
	fmt.Fprintf(buf, "// Code generated by gotypeconverter; DO NOT EDIT.\n")
	fmt.Fprintf(buf, "package %s\n", pass.Pkg.Name())

	funcMaker := &FuncMaker{
		buf:                new(bytes.Buffer),
		pkg:                pass.Pkg,
		dstWrittenSelector: map[string]struct{}{},
	}
	tmp := make([]*FuncMaker, 0, 10)
	funcMaker.childFunc = &tmp
	funcMaker.MakeFunc(Type{typ: dstType, name: flagDst}, Type{typ: srcType, name: flagSrc})

	if flagOutput == "" {
		buf.Write(funcMaker.WriteBytes())
		data, err := sortFunction(buf.Bytes(), tmpFilePath)
		if err != nil {
			return err
		}

		src, err := imports.Process(tmpFilePath, data, &imports.Options{
			Fragment: true,
			Comments: true,
		})
		if err != nil {
			return err
		}

		pass.Print(string(src))
		return nil
	}

	var src []byte
	if output, err := ioutil.ReadFile(flagOutput); err == nil {
		// already exist
		output = append(output, funcMaker.WriteBytes()...)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, flagOutput, output, parser.ParseComments)
		if err != nil {
			return err
		}

		// delete same name func
		funcDeclMap := make(map[string]*ast.FuncDecl)
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				funcDeclMap[fd.Name.Name] = fd
			}
		}
		newDecls := make([]ast.Decl, 0)
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if _, ok := funcDeclMap[fd.Name.Name]; ok {
					continue
				}
			}

			newDecls = append(newDecls, d)
		}
		for _, lastFd := range funcDeclMap {
			newDecls = append(newDecls, lastFd)
		}
		file.Decls = newDecls

		// sort function
		sort.Slice(file.Decls, func(i, j int) bool {
			fdi, iok := file.Decls[i].(*ast.FuncDecl)
			if !iok {
				return true
			}
			fdj, jok := file.Decls[j].(*ast.FuncDecl)
			if !jok {
				return false
			}
			return fdi.Name.Name < fdj.Name.Name
		})

		dst := new(bytes.Buffer)
		err = format.Node(dst, fset, file)
		if err != nil {
			return err
		}
		src = dst.Bytes()
	} else {
		buf.Write(funcMaker.WriteBytes())
		src, err = sortFunction(buf.Bytes(), flagOutput)
		if err != nil {
			return err
		}
	}
	src, err := imports.Process(flagOutput, src, &imports.Options{
		Fragment: true,
		Comments: true,
	})
	src, _ = format.Source(src)
	if err != nil {
		return err
	}

	f, err := os.Create(flagOutput)
	if err != nil {
		return err
	}

	fmt.Fprint(f, string(src))

	if err := f.Close(); err != nil {
		return err
	}

	return nil
}

func sortFunction(data []byte, fileName string) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fileName, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	// sort function
	sort.Slice(file.Decls, func(i, j int) bool {
		fdi, iok := file.Decls[i].(*ast.FuncDecl)
		if !iok {
			return true
		}
		fdj, jok := file.Decls[j].(*ast.FuncDecl)
		if !jok {
			return false
		}
		return fdi.Name.Name < fdj.Name.Name
	})

	dst := new(bytes.Buffer)
	err = format.Node(dst, fset, file)
	return dst.Bytes(), err
}

// FuncMaker generate function
type FuncMaker struct {
	funcName string
	buf      *bytes.Buffer
	// output package
	pkg *types.Package

	parentFunc *FuncMaker
	childFunc  *[]*FuncMaker

	// 同じselectorに対して書き込むのは一回のみ
	dstWrittenSelector map[string]struct{}
}

type Type struct {
	typ  types.Type
	name string
}

type TypeStruct struct {
	typ  *types.Struct
	name string
}

type TypeSlice struct {
	typ  *types.Slice
	name string
}

type TypePointer struct {
	typ  *types.Pointer
	name string
}

type TypeBasic struct {
	typ  *types.Basic
	name string
}

type TypeNamed struct {
	typ  *types.Named
	name string
}

// MakeFunc make function
// TODO fix only named type
func (fm *FuncMaker) MakeFunc(dstType, srcType Type) {
	dstName := fm.formatPkgType(dstType.typ)
	srcName := fm.formatPkgType(srcType.typ)

	fm.funcName = fm.getFuncName(dstType.typ, srcType.typ)

	fmt.Fprintf(fm.buf, "func %s(src %s) (dst %s) {\n",
		fm.funcName, srcName, dstName)
	fm.makeFunc(Type{typ: dstType.typ}, Type{typ: srcType.typ}, "dst", "src", "", nil)
	fmt.Fprintf(fm.buf, "return\n}\n\n")
}

// WriteBytes 全ての関数を書き出す。
func (fm *FuncMaker) WriteBytes() (out []byte) {
	out = fm.buf.Bytes()
	if fm.childFunc != nil {
		for _, child := range *fm.childFunc {
			out = append(out, child.WriteBytes()...)
		}
	}
	return
}

func (fm *FuncMaker) getFuncName(dstType, srcType types.Type) string {
	dstName := fm.formatPkgType(dstType)
	srcName := fm.formatPkgType(srcType)

	re := regexp.MustCompile(`\.`)
	srcName = string(re.ReplaceAll([]byte(srcName), []byte("")))
	dstName = string(re.ReplaceAll([]byte(dstName), []byte("")))

	re = regexp.MustCompile(`\[\]`)
	srcName = string(re.ReplaceAll([]byte(srcName), []byte("S")))
	dstName = string(re.ReplaceAll([]byte(dstName), []byte("S")))

	re = regexp.MustCompile(`\*`)
	srcName = string(re.ReplaceAll([]byte(srcName), []byte("P")))
	dstName = string(re.ReplaceAll([]byte(dstName), []byte("P")))

	return fmt.Sprintf("Conv%sTo%s", srcName, dstName)
}

func selectorGen(selector string, field *types.Var) string {
	return fmt.Sprintf("%s.%s", selector, field.Name())
}

type optionTag int

const (
	ignore optionTag = iota + 1
	readOnly
	writeOnly
)

func getTag(tag string) (name, readName, writeName string, option optionTag) {
	tags, err := structtag.Parse(tag)
	if err != nil {
		return
	}
	cvtTag, err := tags.Get(flagStructTag)
	if err != nil {
		return
	}

	for _, tag := range append(cvtTag.Options, cvtTag.Name) {
		tag = strings.Trim(tag, " ")

		if strings.HasPrefix(tag, "read:") {
			readName = tag[5:]
			continue
		}
		if strings.HasPrefix(tag, "write:") {
			writeName = tag[6:]
			continue
		}

		switch tag {
		case "-":
			option = ignore
		case "->":
			option = readOnly
		case "<-":
			option = writeOnly
		default:
			name = tag
		}
	}
	return
}

func (fm *FuncMaker) isAlreadyExist(funcName string) bool {
	// 1. rootまで遡る。
	var root *FuncMaker
	var goBackRoot func(*FuncMaker) *FuncMaker
	goBackRoot = func(fm *FuncMaker) *FuncMaker {
		if fm.parentFunc == nil {
			return fm
		}
		return goBackRoot(fm.parentFunc)
	}
	root = goBackRoot(fm)

	// 2. 存在しているか見る。
	var inspectSamaFuncName func(*FuncMaker) bool
	inspectSamaFuncName = func(fm *FuncMaker) bool {
		if fm.funcName == funcName {
			return true
		}
		if fm.childFunc != nil {
			for _, child := range *fm.childFunc {
				exist := inspectSamaFuncName(child)
				if exist {
					return true
				}
			}
		}
		return false
	}
	return inspectSamaFuncName(root)
}

// TODO *types.Var -> *types.Package
func (fm *FuncMaker) pkgVisiable(field *types.Var) bool {
	if fm.pkg.Path() == field.Pkg().Path() {
		return true
	}
	return field.Exported()
}

func (fm *FuncMaker) formatPkgString(str string) string {
	// TODO fix only pointer, slice and badic
	re := regexp.MustCompile(`[\w\./]*/`)
	last := string(re.ReplaceAll([]byte(str), []byte("")))

	tmp := strings.Split(last, ".")
	p := string(regexp.MustCompile(`\[\]|\*`).ReplaceAll([]byte(tmp[0]), []byte("")))

	path := strings.Split(fm.pkg.Path(), "/")
	if p == path[len(path)-1] {
		re := regexp.MustCompile(`[\w]*\.`)
		return string(re.ReplaceAll([]byte(last), []byte("")))
	}
	return last

}

func (fm *FuncMaker) formatPkgType(t types.Type) string {
	return fm.formatPkgString(t.String())
}

func (fm *FuncMaker) deferWrite(f func(*FuncMaker) bool) bool {
	tmpFm := &FuncMaker{
		funcName:   fm.funcName,
		buf:        new(bytes.Buffer),
		pkg:        fm.pkg,
		parentFunc: fm.parentFunc,
		childFunc:  fm.childFunc,

		dstWrittenSelector: fm.dstWrittenSelector,
	}

	written := f(tmpFm)
	if written {
		fm.buf.Write(tmpFm.buf.Bytes())
		// fm.childFunc = tmpFm.childFunc
		fm.dstWrittenSelector = tmpFm.dstWrittenSelector
	}
	return written
}

func nextIndex(index string) string {
	if index == "" {
		return "i"
	}
	return string(index[0] + 1)
}

// 無限ループを防ぐ
func checkHistory(dst, src types.Type, history [][2]types.Type) bool {
	for _, his := range history {
		if types.Identical(his[0], dst) && types.Identical(his[1], src) {
			return true
		}
	}
	return false
}

func (fm *FuncMaker) dstWritten(dstSelector string) bool {
	_, ok := fm.dstWrittenSelector[dstSelector]
	if ok {
		return true
	}

	// 前方一致
	// TODO fix pointer selector
	for sel := range fm.dstWrittenSelector {

		// TODO fix replace * ( ) [ ] . -> \* \( ...
		re := regexp.MustCompile(fmt.Sprintf(`\^%s[\.\(\[]`,
			strings.Replace(sel, "*", "\\*", -1)))
		written := re.Match([]byte(dstSelector))
		if written {
			return true
		}
	}
	return false
}

func (fm *FuncMaker) makeFunc(dst, src Type, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	if fm.dstWritten(dstSelector) {
		return false
	}

	if checkHistory(dst.typ, src.typ, history) {
		return false
	}
	history = append(history, [2]types.Type{dst.typ, src.typ})

	if types.IdenticalIgnoreTags(dst.typ, src.typ) {
		if dst.name != "" && dst.name != src.name {
			fmt.Fprintf(fm.buf, "%s = %s(%s)\n", dstSelector, fm.formatPkgString(dst.name), srcSelector)
		} else {
			fmt.Fprintf(fm.buf, "%s = %s\n", dstSelector, srcSelector)
		}

		fm.dstWrittenSelector[dstSelector] = struct{}{}
		return true
	}

	switch dstT := dst.typ.(type) {
	case *types.Basic:
		switch srcT := src.typ.(type) {
		case *types.Basic:
		case *types.Named:
			return fm.otherAndNamed(dst, TypeNamed{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Slice:
			return fm.otherAndSlice(dst, TypeSlice{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Struct:
			return fm.otherAndStruct(dst, TypeStruct{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Pointer:
			return fm.otherAndPointer(dst, TypePointer{typ: srcT}, dstSelector, srcSelector, index, history)
		default:
		}

	case *types.Named:
		switch srcT := src.typ.(type) {
		case *types.Basic:
			return fm.namedAndOther(TypeNamed{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Named:
			return fm.namedAndNamed(TypeNamed{typ: dstT}, TypeNamed{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Slice:
			return fm.namedAndOther(TypeNamed{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Struct:
			return fm.namedAndOther(TypeNamed{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Pointer:
			return fm.namedAndOther(TypeNamed{typ: dstT}, src, dstSelector, srcSelector, index, history)
		default:
			return fm.namedAndOther(TypeNamed{typ: dstT}, src, dstSelector, srcSelector, index, history)
		}

	case *types.Slice:
		switch srcT := src.typ.(type) {
		case *types.Basic:
			return fm.sliceAndOther(TypeSlice{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Named:
			return fm.otherAndNamed(dst, TypeNamed{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Slice:
			return fm.sliceAndSlice(TypeSlice{typ: dstT}, TypeSlice{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Struct:
			return fm.sliceAndOther(TypeSlice{typ: dstT}, src, dstSelector, srcSelector, index, history) ||
				fm.otherAndStruct(dst, TypeStruct{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Pointer:
			return fm.otherAndPointer(dst, TypePointer{typ: srcT}, dstSelector, srcSelector, index, history)
		default:
			return fm.sliceAndOther(TypeSlice{typ: dstT}, src, dstSelector, srcSelector, index, history)
		}

	case *types.Struct:
		switch srcT := src.typ.(type) {
		case *types.Basic:
			return fm.structAndOther(TypeStruct{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Named:
			return fm.otherAndNamed(dst, TypeNamed{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Slice:
			return fm.otherAndSlice(dst, TypeSlice{typ: srcT}, dstSelector, srcSelector, index, history) ||
				fm.structAndOther(TypeStruct{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Struct:
			return fm.structAndStruct(TypeStruct{typ: dstT}, TypeStruct{typ: srcT}, dstSelector, srcSelector, index, history) ||
				fm.structAndOther(TypeStruct{typ: dstT}, src, dstSelector, srcSelector, index, history) ||
				fm.otherAndStruct(dst, TypeStruct{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Pointer:
			return fm.otherAndPointer(dst, TypePointer{typ: srcT}, dstSelector, srcSelector, index, history)
		default:
			return fm.structAndOther(TypeStruct{typ: dstT}, src, dstSelector, srcSelector, index, history)
		}

	case *types.Pointer:
		switch srcT := src.typ.(type) {
		case *types.Basic:
		case *types.Named:
			return fm.pointerAndOther(TypePointer{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Slice:
			return fm.pointerAndOther(TypePointer{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Struct:
			return fm.pointerAndOther(TypePointer{typ: dstT}, src, dstSelector, srcSelector, index, history)
		case *types.Pointer:
			return fm.otherAndPointer(dst, TypePointer{typ: srcT}, dstSelector, srcSelector, index, history)
		default:
			return fm.pointerAndOther(TypePointer{typ: dstT}, src, dstSelector, srcSelector, index, history)
		}

	default:
		switch srcT := src.typ.(type) {
		case *types.Basic:
		case *types.Named:
			return fm.otherAndNamed(dst, TypeNamed{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Slice:
			return fm.otherAndSlice(dst, TypeSlice{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Struct:
			return fm.otherAndStruct(dst, TypeStruct{typ: srcT}, dstSelector, srcSelector, index, history)
		case *types.Pointer:
			return fm.otherAndPointer(dst, TypePointer{typ: srcT}, dstSelector, srcSelector, index, history)
		default:
		}

	}

	return false
}

func (fm *FuncMaker) structAndOther(dstT TypeStruct, src Type, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	for i := 0; i < dstT.typ.NumFields(); i++ {
		if !fm.pkgVisiable(dstT.typ.Field(i)) {
			continue
		}

		// if struct tag "cvt" exists, use struct tag
		_, _, _, dOption := getTag(dstT.typ.Tag(i))
		if dOption == ignore || dOption == readOnly {
			continue
		}

		written := fm.makeFunc(Type{typ: dstT.typ.Field(i).Type()}, src,
			selectorGen(dstSelector, dstT.typ.Field(i)),
			srcSelector,
			index,
			history,
		)
		if written {
			return true
		}
	}
	return false
}

func (fm *FuncMaker) otherAndStruct(dst Type, srcT TypeStruct, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	for j := 0; j < srcT.typ.NumFields(); j++ {
		if !fm.pkgVisiable(srcT.typ.Field(j)) {
			continue
		}
		// if struct tag "cvt" exists, use struct tag
		_, _, _, sOption := getTag(srcT.typ.Tag(j))
		if sOption == ignore || sOption == writeOnly {
			continue
		}

		written := fm.makeFunc(dst, Type{typ: srcT.typ.Field(j).Type()},
			dstSelector,
			selectorGen(srcSelector, srcT.typ.Field(j)),
			index,
			history,
		)
		if written {
			return true
		}
	}
	return false
}

func (fm *FuncMaker) structAndStruct(dstT, srcT TypeStruct, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	written := false

	// field 同士の比較

	for i := 0; i < dstT.typ.NumFields(); i++ {
		if !fm.pkgVisiable(dstT.typ.Field(i)) {
			continue
		}
		// if struct tag "cvt" exists, use struct tag
		dField, _, dWriteField, dOption := getTag(dstT.typ.Tag(i))
		if dWriteField != "" {
			dField = dWriteField
		}
		if dField == "" {
			dField = dstT.typ.Field(i).Name()
		}
		if dOption == ignore || dOption == readOnly {
			continue
		}

		if dstT.typ.Field(i).Embedded() {
			written = fm.makeFunc(Type{typ: dstT.typ.Field(i).Type()}, Type{typ: srcT.typ, name: srcT.name},
				selectorGen(dstSelector, dstT.typ.Field(i)),
				srcSelector,
				index,
				history,
			) || written
			continue
		}
		for j := 0; j < srcT.typ.NumFields(); j++ {
			if !fm.pkgVisiable(srcT.typ.Field(j)) {
				continue
			}
			// if struct tag "cvt" exists, use struct tag
			sField, sReadField, _, sOption := getTag(srcT.typ.Tag(j))
			if sReadField != "" {
				sField = sReadField
			}
			if sField == "" {
				sField = srcT.typ.Field(j).Name()
			}
			if sOption == ignore || sOption == writeOnly {
				continue
			}

			if srcT.typ.Field(j).Embedded() {
				continue
			}

			if dField == sField {
				written = fm.makeFunc(Type{typ: dstT.typ.Field(i).Type()}, Type{typ: srcT.typ.Field(j).Type()},
					selectorGen(dstSelector, dstT.typ.Field(i)),
					selectorGen(srcSelector, srcT.typ.Field(j)),
					index,
					history,
				) || written
			}
		}
	}

	for j := 0; j < srcT.typ.NumFields(); j++ {
		if srcT.typ.Field(j).Embedded() {
			_, _, _, sOption := getTag(srcT.typ.Tag(j))
			if sOption == ignore || sOption == writeOnly {
				continue
			}

			written = fm.makeFunc(Type{typ: dstT.typ, name: dstT.name}, Type{typ: srcT.typ.Field(j).Type()},
				dstSelector,
				selectorGen(srcSelector, srcT.typ.Field(j)),
				index,
				history,
			) || written
		}
	}

	// 構造体自体とフィールドの比較

	// TODO
	// for j := 0; j < srcT.NumFields(); j++ {
	// sField, sReadField, _, sOption := getTag(srcT.Tag(j))
	// if sReadField != "" {
	// sField = sReadField
	// }
	// if sField == "" {
	// sField = srcT.Field(j).Name()
	// }
	// if sOption == ignore || sOption == writeOnly {
	// continue
	// }

	// dstTName := dstT.String()

	// // TODO 型名とフィールド名の何らかの関連
	// // 情報量は構造体の方が大きいため、型名がフィールド名に含まれるなど
	// if sField == dstTName {
	// written = fm.makeFunc(dstT, srcT.Field(j).Type(),
	// dstSelector,
	// selectorGen(srcSelector, srcT.Field(j)),
	// index,
	// history,
	// ) || written
	// }
	// }

	return written
}

func (fm *FuncMaker) sliceAndOther(dstT TypeSlice, src Type, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	return fm.deferWrite(func(tmpFm *FuncMaker) bool {
		fmt.Fprintf(tmpFm.buf, "%s = make(%s, 1)\n", dstSelector, fm.formatPkgType(dstT.typ))
		return tmpFm.makeFunc(Type{typ: dstT.typ.Elem()}, src, dstSelector+"[0]", srcSelector, index, history)
	})
}

func (fm *FuncMaker) otherAndSlice(dst Type, srcT TypeSlice, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	return fm.deferWrite(func(tmpFm *FuncMaker) bool {
		fmt.Fprintf(tmpFm.buf, "if len(%s)>0 {\n", srcSelector)
		written := tmpFm.makeFunc(dst, Type{typ: srcT.typ.Elem()}, dstSelector, srcSelector+"[0]", index, history)
		fmt.Fprintln(tmpFm.buf, "}")
		return written
	})
}

func (fm *FuncMaker) sliceAndSlice(dstT, srcT TypeSlice, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	index = nextIndex(index)

	return fm.deferWrite(func(tmpFm *FuncMaker) bool {
		fmt.Fprintf(tmpFm.buf, "%s = make(%s, len(%s))\n", dstSelector, fm.formatPkgType(dstT.typ), srcSelector)
		fmt.Fprintf(tmpFm.buf, "for %s := range %s {\n", index, srcSelector)
		written := tmpFm.makeFunc(Type{typ: dstT.typ.Elem()}, Type{typ: srcT.typ.Elem()},
			dstSelector+"["+index+"]",
			srcSelector+"["+index+"]",
			index,
			history,
		)
		fmt.Fprintf(tmpFm.buf, "}\n")
		if written {
			tmpFm.dstWrittenSelector[dstSelector] = struct{}{}
		}
		return written
	})
}

func (fm *FuncMaker) named(namedT TypeNamed, selector string) (Type, string) {
	namedT.typ.Obj().Pkg()
	return Type{typ: namedT.typ.Underlying(), name: namedT.typ.String()}, selector
}

func (fm *FuncMaker) namedAndOther(dstT TypeNamed, src Type, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	dst, dstSelector := fm.named(dstT, dstSelector)
	return fm.makeFunc(dst, src, dstSelector, srcSelector, index, history)
}

func (fm *FuncMaker) otherAndNamed(dst Type, srcT TypeNamed, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	src, srcSelector := fm.named(srcT, srcSelector)
	return fm.makeFunc(dst, src, dstSelector, srcSelector, index, history)
}

func (fm *FuncMaker) namedAndNamed(dstT, srcT TypeNamed, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	funcName := fm.getFuncName(dstT.typ, srcT.typ)
	if !fm.isAlreadyExist(funcName) {
		newFM := &FuncMaker{
			buf:                new(bytes.Buffer),
			pkg:                fm.pkg,
			parentFunc:         fm,
			dstWrittenSelector: map[string]struct{}{},
		}
		tmp := make([]*FuncMaker, 0, 10)
		newFM.childFunc = &tmp

		*fm.childFunc = append(*fm.childFunc, newFM)
		newFM.MakeFunc(Type{typ: dstT.typ, name: dstT.name}, Type{typ: srcT.typ, name: srcT.name})
	}
	if funcName == fm.funcName {
		return fm.makeFunc(Type{typ: dstT.typ.Underlying(), name: dstT.typ.String()}, Type{typ: srcT.typ.Underlying(), name: srcT.typ.String()}, dstSelector, srcSelector, index, history)
	}

	fmt.Fprintf(fm.buf, "%s = %s(%s)\n", dstSelector, funcName, srcSelector)
	fm.dstWrittenSelector[dstSelector] = struct{}{}
	return true
}

// TODO fix pointer

func (fm *FuncMaker) pointer(pointerT TypePointer, selector string) (Type, string) {
	return Type{typ: pointerT.typ.Elem()}, fmt.Sprintf("(*%s)", selector)
}

func (fm *FuncMaker) pointerAndOther(dstT TypePointer, src Type, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	return fm.deferWrite(func(tmpFm *FuncMaker) bool {
		selector := dstSelector
		dst, dstSelector := fm.pointer(dstT, dstSelector)
		fmt.Fprintf(tmpFm.buf, "%s = new(%s)\n", selector, tmpFm.formatPkgType(dst.typ))
		return tmpFm.makeFunc(dst, src, dstSelector, srcSelector, index, history)
	})
}

func (fm *FuncMaker) otherAndPointer(dst Type, srcT TypePointer, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	src, srcSelector := fm.pointer(srcT, srcSelector)
	return fm.makeFunc(dst, src, dstSelector, srcSelector, index, history)
}

func (fm *FuncMaker) pointerAndPointer(dstT, srcT TypePointer, dstSelector, srcSelector, index string, history [][2]types.Type) bool {
	dst, dstSelector := fm.pointer(dstT, dstSelector)
	src, srcSelector := fm.pointer(srcT, srcSelector)
	return fm.makeFunc(dst, src, dstSelector, srcSelector, index, history)
}
