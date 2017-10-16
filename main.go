// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main // import "mvdan.cc/gogrep"

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/printer"
	"go/token"
	"io"
	"os"
	"regexp"
	"strings"
)

var usage = func() {
	fmt.Fprint(os.Stderr, `usage: gogrep commands [packages]

Options:

  -r   match all dependencies recursively too

A command is of the form "-A pattern", where -A is one of:

  -x   find all nodes matching a pattern

If -A is ommitted for a single command, -x will be assumed.

A pattern is a piece of Go code which may include wildcards. It can be:

       a statement (many if split by semicolonss)
       an expression (many if split by commas)
       a type expression
       a top-level declaration (var, func, const)
       an entire file

Wildcards consist of '$' and a name. All wildcards with the same name
within an expression must match the same node, excluding "_". Example:

       $x.$_ = $x // assignment of self to a field in self

If '*' is before the name, it will match any number of nodes. Example:

       fmt.Fprintf(os.Stdout, $*_) // all Fprintfs on stdout

Regexes can also be used to match certain identifier names only. The
'.*' pattern can be used to match all identifiers. Example:

       fmt.$(_ /Fprint.*/)(os.Stdout, $*_) // all Fprint* on stdout

The nodes resulting from applying the commands will be printed line by
line to standard output.
`)
}

func main() {
	m := matcher{
		out: os.Stdout,
		ctx: &build.Default,
	}
	err := m.fromArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type matcher struct {
	out io.Writer
	ctx *build.Context

	typed, aggressive bool

	// information about variables (wildcards), by id (which is an
	// integer starting at 0)
	vars []varInfo

	// node values recorded by name, excluding "_" (used only by the
	// actual matching phase)
	values map[string]ast.Node
}

type varInfo struct {
	name   string
	any    bool
	nameRx *regexp.Regexp
}

func (m *matcher) info(id int) varInfo {
	if id < 0 {
		return varInfo{}
	}
	return m.vars[id]
}

type flagPair struct {
	name string
	val  string
}

type orderedFlag struct {
	name  string
	flags *[]flagPair
}

func (o *orderedFlag) String() string {
	return ""
}

func (o *orderedFlag) Set(val string) error {
	*o.flags = append(*o.flags, flagPair{o.name, val})
	return nil
}

func (m *matcher) fromArgs(args []string) error {
	flagSet := flag.NewFlagSet("gogrep", flag.ExitOnError)
	flagSet.Usage = usage
	recurse := flagSet.Bool("r", false, "match all dependencies recursively too")

	var flags []flagPair
	flagSet.Var(&orderedFlag{
		name:  "x",
		flags: &flags,
	}, "x", "range over the matches")
	flagSet.Parse(args)
	paths := flagSet.Args()

	if len(flags) == 0 && len(paths) > 0 {
		flags = append(flags, flagPair{"x", paths[0]})
		paths = paths[1:]
	}
	if len(flags) < 1 {
		return fmt.Errorf("need at least one command")
	}
	if len(flags) > 1 {
		return fmt.Errorf("TODO: command composability")
	}

	exprNode, err := m.compileExpr(flags[0].val)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	var all []ast.Node
	loader := nodeLoader{wd, m.ctx, fset}
	if !m.typed {
		nodes, err := loader.untyped(paths, *recurse)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			all = append(all, m.matches(exprNode, node)...)
		}
	} else {
		prog, err := loader.typed(paths, *recurse)
		if err != nil {
			return err
		}
		// TODO: recursive mode
		for _, pkg := range prog.InitialPackages() {
			for _, file := range pkg.Files {
				all = append(all, m.matches(exprNode, file)...)
			}
		}
	}
	for _, n := range all {
		fpos := loader.fset.Position(n.Pos())
		if strings.HasPrefix(fpos.Filename, wd) {
			fpos.Filename = fpos.Filename[len(wd)+1:]
		}
		fmt.Fprintf(m.out, "%v: %s\n", fpos, singleLinePrint(n))
	}
	return nil
}

type bufferJoinLines struct {
	bytes.Buffer
	last string
}

var rxNeedSemicolon = regexp.MustCompile(`([])}a-zA-Z0-9"'` + "`" + `]|\+\+|--)$`)

func (b *bufferJoinLines) Write(p []byte) (n int, err error) {
	if string(p) == "\n" {
		if rxNeedSemicolon.MatchString(b.last) {
			b.Buffer.WriteByte(';')
		}
		b.Buffer.WriteByte(' ')
		return 1, nil
	}
	p = bytes.Trim(p, "\t")
	n, err = b.Buffer.Write(p)
	b.last = string(p)
	return
}

func singleLinePrint(node ast.Node) string {
	var buf bufferJoinLines
	printNode(&buf, token.NewFileSet(), node)
	return buf.String()
}

func printNode(w io.Writer, fset *token.FileSet, node ast.Node) {
	switch x := node.(type) {
	case exprList:
		if len(x) == 0 {
			return
		}
		printNode(w, fset, x[0])
		for _, n := range x[1:] {
			fmt.Fprintf(w, ", ")
			printNode(w, fset, n)
		}
	case stmtList:
		if len(x) == 0 {
			return
		}
		printNode(w, fset, x[0])
		for _, n := range x[1:] {
			fmt.Fprintf(w, "; ")
			printNode(w, fset, n)
		}
	default:
		err := printer.Fprint(w, fset, node)
		if err != nil && strings.Contains(err.Error(), "go/printer: unsupported node type") {
			// Should never happen, but make it obvious when it does.
			panic(fmt.Errorf("cannot print node: %v\n", node, err))
		}
	}
}

type lineColBuffer struct {
	bytes.Buffer
	line, col, offs int
}

func (l *lineColBuffer) WriteString(s string) (n int, err error) {
	for _, r := range s {
		if r == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.offs++
	}
	return l.Buffer.WriteString(s)
}

func (m *matcher) compileExpr(expr string) (node ast.Node, err error) {
	toks, err := m.tokenize(expr)
	if err != nil {
		return nil, fmt.Errorf("cannot tokenize expr: %v", err)
	}
	var offs []posOffset
	lbuf := lineColBuffer{line: 1, col: 1}
	addOffset := func(length int) {
		lbuf.offs -= length
		offs = append(offs, posOffset{
			atLine: lbuf.line,
			atCol:  lbuf.col,
			offset: length,
		})
	}
	if len(toks) > 0 && toks[0].tok == tokAggressive {
		toks = toks[1:]
		m.aggressive = true
	}
	lastLit := false
	for _, t := range toks {
		if lbuf.offs >= t.pos.Offset && lastLit && t.lit != "" {
			lbuf.WriteString(" ")
		}
		for lbuf.offs < t.pos.Offset {
			lbuf.WriteString(" ")
		}
		if t.lit == "" {
			lbuf.WriteString(t.tok.String())
			lastLit = false
			continue
		}
		if isWildName(t.lit) {
			// to correct the position offsets for the extra
			// info attached to ident name strings
			addOffset(len(wildPrefix) - 1)
		}
		lbuf.WriteString(t.lit)
		lastLit = strings.TrimSpace(t.lit) != ""
	}
	// trailing newlines can cause issues with commas
	exprStr := strings.TrimSpace(lbuf.String())
	if node, err = parse(exprStr); err != nil {
		err = subPosOffsets(err, offs...)
		return nil, fmt.Errorf("cannot parse expr: %v", err)
	}
	return node, nil
}
