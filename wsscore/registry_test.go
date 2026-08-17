package wsscore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode"
)

// Reasons() and WinsockErrnos() are hand-maintained projections of const
// blocks, because Go cannot enumerate constants at runtime. Every downstream
// sync check walks the projection, so a constant added to a block but not to
// its projection would slip past all of them. These tests read the package
// source instead and fail on exactly that gap.

// exportedConstExprs parses every non-test file of the package and returns the
// value expression of each exported constant whose name starts with prefix
// followed by an upper-case rune.
func exportedConstExprs(t *testing.T, prefix string) map[string]ast.Expr {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	found := map[string]ast.Expr{}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.CONST {
				continue
			}
			for _, spec := range genDecl.Specs {
				valueSpec := spec.(*ast.ValueSpec)
				for index, ident := range valueSpec.Names {
					rest := strings.TrimPrefix(ident.Name, prefix)
					if rest == ident.Name || rest == "" || !unicode.IsUpper(rune(rest[0])) {
						continue
					}
					if len(valueSpec.Values) != len(valueSpec.Names) {
						t.Errorf("%s: constant %s carries no explicit value", name, ident.Name)
						continue
					}
					found[ident.Name] = valueSpec.Values[index]
				}
			}
		}
	}
	return found
}

// TestReasonsCoversTheConstBlock checks that the Reason* const block and the
// Reasons() registry name exactly the same token set, so a token cannot be
// minted without entering the registry every consumer's tests walk.
func TestReasonsCoversTheConstBlock(t *testing.T) {
	declared := map[string]string{}
	for name, expr := range exportedConstExprs(t, "Reason") {
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s is not a plain string literal", name)
			continue
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("%s: unquote %s: %v", name, lit.Value, err)
			continue
		}
		declared[name] = value
	}
	if len(declared) == 0 {
		t.Fatal("found no Reason* constants in the package source")
	}

	registry := Reasons()
	for index, token := range registry {
		if slices.Contains(registry[:index], token) {
			t.Errorf("Reasons() lists %q twice", token)
		}
	}
	declaredValues := make(map[string]bool, len(declared))
	for name, value := range declared {
		declaredValues[value] = true
		if !slices.Contains(registry, value) {
			t.Errorf("constant %s (%q) is missing from Reasons()", name, value)
		}
	}
	for _, token := range registry {
		if !declaredValues[token] {
			t.Errorf("Reasons() lists %q, which no Reason* constant declares", token)
		}
	}
}

// TestWinsockErrnosCoversTheConstBlock checks that the WSAE* const block and
// the WinsockErrnos() table agree symbol by symbol and value by value. It also
// requires each constant to be spelled as a raw syscall.Errno literal — a
// constant rewritten to a portable syscall.E* name would hold an invented
// value on Windows and never match what the net stack surfaces, which is the
// reversion the old build-tagged table could only catch on a Windows test run.
func TestWinsockErrnosCoversTheConstBlock(t *testing.T) {
	declared := map[string]syscall.Errno{}
	for name, expr := range exportedConstExprs(t, "WSAE") {
		call, ok := expr.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			t.Errorf("%s is not spelled as a raw syscall.Errno(<number>) literal", name)
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Errno" {
			t.Errorf("%s is not spelled as a raw syscall.Errno(<number>) literal", name)
			continue
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			t.Errorf("%s is not spelled as a raw syscall.Errno(<number>) literal", name)
			continue
		}
		value, err := strconv.Atoi(lit.Value)
		if err != nil {
			t.Errorf("%s: parse %s: %v", name, lit.Value, err)
			continue
		}
		declared[name] = syscall.Errno(value)
	}
	if len(declared) == 0 {
		t.Fatal("found no WSAE* constants in the package source")
	}

	table := WinsockErrnos()
	for name, value := range declared {
		got, exists := table[name]
		switch {
		case !exists:
			t.Errorf("constant %s is missing from WinsockErrnos()", name)
		case got != value:
			t.Errorf("WinsockErrnos()[%q] = %d, but the constant declares %d", name, got, value)
		}
	}
	for symbol := range table {
		if _, exists := declared[symbol]; !exists {
			t.Errorf("WinsockErrnos() lists %q, which no WSAE* constant declares", symbol)
		}
	}
}
