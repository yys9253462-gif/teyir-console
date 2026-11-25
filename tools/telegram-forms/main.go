package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Location string `json:"location"`
}
type Form struct {
	Fields  []Field `json:"fields"`
	Browser bool    `json:"browser"`
}

func main() {
	check := flag.Bool("check", false, "check generated catalog")
	flag.Parse()
	dir := "server/internal/httpapi"
	fs := token.NewFileSet()
	files, err := parser.ParseDir(fs, dir, func(i os.FileInfo) bool { return !strings.HasSuffix(i.Name(), "_test.go") }, 0)
	if err != nil {
		panic(err)
	}
	pkg := files["httpapi"]
	types := map[string]ast.Expr{}
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			switch v := d.(type) {
			case *ast.GenDecl:
				for _, sp := range v.Specs {
					if t, ok := sp.(*ast.TypeSpec); ok {
						types[t.Name.Name] = t.Type
					}
				}
			case *ast.FuncDecl:
				funcs[v.Name.Name] = v
			}
		}
	}
	var fields func(ast.Expr, string) []Field
	fields = func(e ast.Expr, prefix string) []Field {
		if p, ok := e.(*ast.StarExpr); ok {
			return fields(p.X, prefix)
		}
		if id, ok := e.(*ast.Ident); ok {
			if t, ok := types[id.Name]; ok {
				return fields(t, prefix)
			}
		}
		st, ok := e.(*ast.StructType)
		if !ok {
			return nil
		}
		out := []Field{}
		for _, f := range st.Fields.List {
			if len(f.Names) == 0 || f.Tag == nil {
				continue
			}
			tag, _ := strconv.Unquote(f.Tag.Value)
			name := strings.Split(reflect.StructTag(tag).Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			name = prefix + name
			typ := f.Type
			if p, ok := typ.(*ast.StarExpr); ok {
				typ = p.X
			}
			kind := "string"
			switch t := typ.(type) {
			case *ast.Ident:
				if strings.Contains(t.Name, "int") || strings.Contains(t.Name, "float") {
					kind = "number"
				} else if t.Name == "bool" {
					kind = "bool"
				} else if nested := fields(t, name+"."); len(nested) > 0 {
					out = append(out, nested...)
					continue
				}
			case *ast.ArrayType:
				kind = "array"
			case *ast.MapType:
				kind = "map"
			case *ast.StructType:
				out = append(out, fields(t, name+".")...)
				continue
			}
			out = append(out, Field{name, kind, "body"})
		}
		return out
	}
	forms := map[string]Form{}
	server := pkg.Files[filepath.Join(dir, "server.go")]
	if server == nil {
		for path, f := range pkg.Files {
			if strings.HasSuffix(path, "server.go") {
				server = f
			}
		}
	}
	ast.Inspect(server, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "panelRoute" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		pattern, _ := strconv.Unquote(lit.Value)
		expr := call.Args[1]
		for {
			if c, ok := expr.(*ast.CallExpr); ok && len(c.Args) > 0 {
				expr = c.Args[0]
			} else {
				break
			}
		}
		ref, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		fn := funcs[ref.Sel.Name]
		if fn == nil {
			return true
		}
		form := Form{Fields: []Field{}}
		vars := map[string]ast.Expr{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if v, ok := n.(*ast.ValueSpec); ok {
				for _, name := range v.Names {
					vars[name.Name] = v.Type
				}
			}
			return true
		})
		seen := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			m, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if m.Sel.Name == "ParseMultipartForm" {
				form.Browser = true
			}
			if m.Sel.Name == "Decode" && len(c.Args) == 1 {
				if u, ok := c.Args[0].(*ast.UnaryExpr); ok {
					if id, ok := u.X.(*ast.Ident); ok {
						for _, f := range fields(vars[id.Name], "") {
							if !seen[f.Location+f.Name] {
								seen[f.Location+f.Name] = true
								form.Fields = append(form.Fields, f)
							}
						}
					}
				}
			}
			if m.Sel.Name == "Get" && len(c.Args) == 1 {
				if receiver, ok := m.X.(*ast.CallExpr); ok {
					if q, ok := receiver.Fun.(*ast.SelectorExpr); ok && q.Sel.Name == "Query" {
						if l, ok := c.Args[0].(*ast.BasicLit); ok {
							key, _ := strconv.Unquote(l.Value)
							if !seen["query"+key] {
								seen["query"+key] = true
								form.Fields = append(form.Fields, Field{key, "string", "query"})
							}
						}
					}
				}
			}
			return true
		})
		forms[pattern] = form
		return true
	})
	raw, err := json.MarshalIndent(forms, "", "  ")
	if err != nil {
		panic(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join(dir, "telegram_forms.json")
	if *check {
		old, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(old, raw) {
			fmt.Fprintln(os.Stderr, "Telegram表单目录过期，请运行go run ./tools/telegram-forms")
			os.Exit(1)
		}
	} else {
		if err = os.WriteFile(path, raw, 0644); err != nil {
			panic(err)
		}
	}
	fmt.Printf("Telegram forms: %d routes\n", len(forms))
}
