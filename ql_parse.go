package minidb

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// syntax tree
// ---------------------------------------------------------------------------

// node types
const (
	qlUninit = 0
	qlStr    = 1
	qlI64    = 2
	qlTuple  = 10
	qlSym    = 11
	qlStar   = 12 // `*` in select
	// unary / binary operators
	qlNeg = 20
	qlNot = 21
	qlAdd = 30
	qlSub = 31
	qlMul = 32
	qlDiv = 33
	qlMod = 34
	// comparisons
	qlCmpGE = 40
	qlCmpGT = 41
	qlCmpLT = 42
	qlCmpLE = 43
	qlCmpEQ = 44
	qlCmpNE = 45
	// logic
	qlAnd = 50
	qlOr  = 51
)

// QLNode is an expression tree node: a tagged union.
type QLNode struct {
	Type uint32
	Value
	Kids []QLNode // operands
}

// common structure for statements: `INDEX BY`, `FILTER`, `LIMIT`
type QLScan struct {
	Table  string
	Key1   QLNode // index by
	Key2   QLNode
	Filter QLNode // filter expression
	Offset int64  // limit
	Limit  int64
}

type QLCreateTable struct{ Def TableDef }
type QLSelect struct {
	QLScan
	Names  []string // expr AS name
	Output []QLNode
}
type QLUpdate struct {
	QLScan
	Names  []string
	Values []QLNode
}
type QLDelete struct{ QLScan }
type QLInsert struct {
	Table  string
	Cols   []string
	Values [][]QLNode
}
type QLShowTables struct{}
type QLDescribe struct{ Table string }
type QLBegin struct{}
type QLCommit struct{}
type QLRollback struct{}

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

type parser struct {
	input string
	idx   int
}

type parseError struct{ msg string }

func (p *parser) fail(format string, args ...any) {
	line, col := 1, 1
	for _, ch := range p.input[:min(p.idx, len(p.input))] {
		if ch == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	panic(parseError{fmt.Sprintf("syntax error at line %d, column %d: %s", line, col, fmt.Sprintf(format, args...))})
}

func (p *parser) skipSpace() {
	for {
		for p.idx < len(p.input) && strings.IndexByte(" \t\r\n", p.input[p.idx]) >= 0 {
			p.idx++
		}
		// -- line comments
		if strings.HasPrefix(p.input[p.idx:], "--") {
			for p.idx < len(p.input) && p.input[p.idx] != '\n' {
				p.idx++
			}
			continue
		}
		return
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentChar(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// match and consume a sequence of keywords or punctuation. All or nothing.
func pKeyword(p *parser, kws ...string) bool {
	save := p.idx
	for _, kw := range kws {
		p.skipSpace()
		end := p.idx + len(kw)
		if end > len(p.input) || !strings.EqualFold(p.input[p.idx:end], kw) {
			p.idx = save
			return false
		}
		if isIdentChar(kw[len(kw)-1]) && end < len(p.input) && isIdentChar(p.input[end]) {
			p.idx = save // not a whole word
			return false
		}
		p.idx = end
	}
	return true
}

func pExpect(p *parser, kw string, what string) {
	if !pKeyword(p, kw) {
		p.skipSpace()
		p.fail("%s", what)
	}
}

var reserved = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`select from index by filter limit create table primary key
		insert into values delete update set and or not as show tables describe begin commit rollback`) {
		reserved[w] = true
	}
}

func pSym(p *parser) (string, bool) {
	p.skipSpace()
	start := p.idx
	if start >= len(p.input) || !isIdentStart(p.input[start]) {
		return "", false
	}
	end := start
	for end < len(p.input) && isIdentChar(p.input[end]) {
		end++
	}
	name := p.input[start:end]
	if reserved[strings.ToLower(name)] {
		return "", false
	}
	p.idx = end
	return name, true
}

func pMustSym(p *parser, what string) string {
	name, ok := pSym(p)
	if !ok {
		p.fail("expect %s", what)
	}
	return name
}

func pNum(p *parser) (int64, bool) {
	p.skipSpace()
	start := p.idx
	end := start
	for end < len(p.input) && p.input[end] >= '0' && p.input[end] <= '9' {
		end++
	}
	if end == start {
		return 0, false
	}
	if end < len(p.input) && isIdentChar(p.input[end]) {
		p.fail("bad number")
	}
	n, err := strconv.ParseInt(p.input[start:end], 10, 64)
	if err != nil {
		p.fail("number out of range")
	}
	p.idx = end
	return n, true
}

func pString(p *parser) ([]byte, bool) {
	p.skipSpace()
	if p.idx >= len(p.input) || (p.input[p.idx] != '\'' && p.input[p.idx] != '"') {
		return nil, false
	}
	quote := p.input[p.idx]
	p.idx++
	var out []byte
	for {
		if p.idx >= len(p.input) {
			p.fail("unterminated string")
		}
		ch := p.input[p.idx]
		p.idx++
		switch {
		case ch == quote && p.idx < len(p.input) && p.input[p.idx] == quote:
			out = append(out, quote) // doubled quote
			p.idx++
		case ch == quote:
			return out, true
		case ch == '\\' && p.idx < len(p.input):
			esc := p.input[p.idx]
			p.idx++
			switch esc {
			case 'n':
				out = append(out, '\n')
			case 't':
				out = append(out, '\t')
			case 'r':
				out = append(out, '\r')
			case '0':
				out = append(out, 0)
			default:
				out = append(out, esc)
			}
		default:
			out = append(out, ch)
		}
	}
}

// ---------------------------------------------------------------------------
// statements
// ---------------------------------------------------------------------------

// QLParse parses a single statement (an optional `;` may follow).
func QLParse(src string) (stmt any, err error) {
	stmts, err := QLParseAll(src)
	if err != nil {
		return nil, err
	}
	if len(stmts) != 1 {
		return nil, fmt.Errorf("expect 1 statement, got %d", len(stmts))
	}
	return stmts[0], nil
}

// QLParseAll parses `;`-separated statements.
func QLParseAll(src string) (stmts []any, err error) {
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(parseError)
			if !ok {
				panic(r)
			}
			stmts, err = nil, errors.New(pe.msg)
		}
	}()
	p := &parser{input: src}
	for {
		for pKeyword(p, ";") {
		}
		p.skipSpace()
		if p.idx >= len(p.input) {
			return stmts, nil
		}
		stmts = append(stmts, pStmt(p))
		p.skipSpace()
		if p.idx < len(p.input) && !pKeyword(p, ";") {
			p.fail("unexpected input")
		}
	}
}

func pStmt(p *parser) any {
	switch {
	case pKeyword(p, "create", "table"):
		return pCreateTable(p)
	case pKeyword(p, "select"):
		return pSelect(p)
	case pKeyword(p, "insert", "into"):
		return pInsert(p)
	case pKeyword(p, "delete", "from"):
		stmt := &QLDelete{}
		stmt.Table = pMustSym(p, "table name")
		pScan(p, &stmt.QLScan)
		return stmt
	case pKeyword(p, "update"):
		return pUpdate(p)
	case pKeyword(p, "show", "tables"):
		return &QLShowTables{}
	case pKeyword(p, "describe"):
		return &QLDescribe{Table: pMustSym(p, "table name")}
	case pKeyword(p, "begin"):
		return &QLBegin{}
	case pKeyword(p, "commit"):
		return &QLCommit{}
	case pKeyword(p, "rollback"):
		return &QLRollback{}
	}
	p.skipSpace()
	p.fail("expect a statement (create, select, insert, update, delete, show, describe, begin, commit, rollback)")
	return nil
}

func pNameList(p *parser) []string {
	pExpect(p, "(", "expect `(`")
	var names []string
	for {
		names = append(names, pMustSym(p, "column name"))
		if !pKeyword(p, ",") {
			break
		}
	}
	pExpect(p, ")", "expect `)`")
	return names
}

func pCreateTable(p *parser) *QLCreateTable {
	stmt := &QLCreateTable{}
	stmt.Def.Name = pMustSym(p, "table name")
	pExpect(p, "(", "expect `(`")
	var pk []string
	for {
		switch {
		case pKeyword(p, "primary", "key"):
			if pk != nil {
				p.fail("duplicate primary key")
			}
			pk = pNameList(p)
		case pKeyword(p, "index"):
			stmt.Def.Indexes = append(stmt.Def.Indexes, pNameList(p))
		default:
			name := pMustSym(p, "column name, `index` or `primary key`")
			var typ uint32
			switch {
			case pKeyword(p, "int64"), pKeyword(p, "int"), pKeyword(p, "integer"):
				typ = TypeInt64
			case pKeyword(p, "string"), pKeyword(p, "bytes"), pKeyword(p, "text"):
				typ = TypeBytes
			default:
				p.skipSpace()
				p.fail("expect a column type: int64 or string")
			}
			stmt.Def.Cols = append(stmt.Def.Cols, name)
			stmt.Def.Types = append(stmt.Def.Types, typ)
		}
		if !pKeyword(p, ",") {
			break
		}
		if pKeyword(p, ")") { // trailing comma
			p.idx--
			break
		}
	}
	pExpect(p, ")", "expect `)`")
	if pk == nil {
		p.fail("a primary key is required")
	}
	stmt.Def.Indexes = append([][]string{pk}, stmt.Def.Indexes...)
	return stmt
}

func pSelect(p *parser) *QLSelect {
	stmt := &QLSelect{}
	for {
		pSelectExpr(p, stmt)
		if !pKeyword(p, ",") {
			break
		}
	}
	pExpect(p, "from", "expect `FROM` table")
	stmt.Table = pMustSym(p, "table name")
	pScan(p, &stmt.QLScan)
	return stmt
}

func pSelectExpr(p *parser, node *QLSelect) {
	if pKeyword(p, "*") {
		node.Names = append(node.Names, "*")
		node.Output = append(node.Output, QLNode{Type: qlStar})
		return
	}
	var expr QLNode
	pExprOr(p, &expr)
	name := ""
	if pKeyword(p, "as") {
		name = pMustSym(p, "column alias")
	} else if expr.Type == qlSym {
		name = string(expr.Str)
	} else {
		name = fmt.Sprintf("expr%d", len(node.Output)+1)
	}
	node.Names = append(node.Names, name)
	node.Output = append(node.Output, expr)
}

func pInsert(p *parser) *QLInsert {
	stmt := &QLInsert{}
	stmt.Table = pMustSym(p, "table name")
	stmt.Cols = pNameList(p)
	pExpect(p, "values", "expect `VALUES`")
	for {
		pExpect(p, "(", "expect `(`")
		var row []QLNode
		for {
			var e QLNode
			pExprOr(p, &e)
			row = append(row, e)
			if !pKeyword(p, ",") {
				break
			}
		}
		pExpect(p, ")", "expect `)`")
		if len(row) != len(stmt.Cols) {
			p.fail("expect %d values, got %d", len(stmt.Cols), len(row))
		}
		stmt.Values = append(stmt.Values, row)
		if !pKeyword(p, ",") {
			break
		}
	}
	return stmt
}

func pUpdate(p *parser) *QLUpdate {
	stmt := &QLUpdate{}
	stmt.Table = pMustSym(p, "table name")
	pExpect(p, "set", "expect `SET`")
	for {
		stmt.Names = append(stmt.Names, pMustSym(p, "column name"))
		pExpect(p, "=", "expect `=`")
		var e QLNode
		pExprOr(p, &e)
		stmt.Values = append(stmt.Values, e)
		if !pKeyword(p, ",") {
			break
		}
	}
	pScan(p, &stmt.QLScan)
	return stmt
}

// INDEX BY ... FILTER ... LIMIT ...
func pScan(p *parser, node *QLScan) {
	if pKeyword(p, "index", "by") {
		pIndexBy(p, node)
	}
	if pKeyword(p, "filter") {
		pExprOr(p, &node.Filter)
	}
	node.Offset, node.Limit = 0, math.MaxInt64
	if pKeyword(p, "limit") {
		n1, ok := pNum(p)
		if !ok {
			p.fail("expect a number")
		}
		if pKeyword(p, ",") { // LIMIT offset, count
			n2, ok := pNum(p)
			if !ok {
				p.fail("expect a number")
			}
			node.Offset, node.Limit = n1, n2
		} else {
			node.Limit = n1
		}
	}
}

func isCmp(t uint32) bool { return t >= qlCmpGE && t <= qlCmpNE }

func pIndexBy(p *parser, node *QLScan) {
	var index QLNode
	pExprAnd(p, &index)
	switch {
	case isCmp(index.Type):
		node.Key1 = index
	case index.Type == qlAnd:
		node.Key1, node.Key2 = index.Kids[0], index.Kids[1]
		if !isCmp(node.Key1.Type) || !isCmp(node.Key2.Type) {
			p.fail("INDEX BY expects `a > x`, `a = x` or `a > x AND a < y`")
		}
	default:
		p.fail("INDEX BY expects a comparison")
	}
}

// ---------------------------------------------------------------------------
// expressions: recursive descent; one function per priority level
//
//	a OR b         -- pExprOr
//	a AND b        -- pExprAnd
//	NOT a          -- pExprNot
//	a = b, a < b   -- pExprCmp
//	a + b, a - b   -- pExprAdd
//	a * b, a / b   -- pExprMul
//	-a             -- pExprUnop
// ---------------------------------------------------------------------------

func pExprOr(p *parser, node *QLNode) {
	pExprAnd(p, node)
	for pKeyword(p, "or") {
		var right QLNode
		pExprAnd(p, &right)
		*node = QLNode{Type: qlOr, Kids: []QLNode{*node, right}}
	}
}

func pExprAnd(p *parser, node *QLNode) {
	pExprNot(p, node)
	for pKeyword(p, "and") {
		var right QLNode
		pExprNot(p, &right)
		*node = QLNode{Type: qlAnd, Kids: []QLNode{*node, right}}
	}
}

func pExprNot(p *parser, node *QLNode) {
	if pKeyword(p, "not") {
		var kid QLNode
		pExprNot(p, &kid)
		*node = QLNode{Type: qlNot, Kids: []QLNode{kid}}
		return
	}
	pExprCmp(p, node)
}

func pExprCmp(p *parser, node *QLNode) {
	pExprAdd(p, node)
	// longer operators first
	ops := []struct {
		tok string
		typ uint32
	}{{"<=", qlCmpLE}, {">=", qlCmpGE}, {"<>", qlCmpNE}, {"!=", qlCmpNE},
		{"<", qlCmpLT}, {">", qlCmpGT}, {"==", qlCmpEQ}, {"=", qlCmpEQ}}
	for _, op := range ops {
		if pKeyword(p, op.tok) {
			var right QLNode
			pExprAdd(p, &right)
			*node = QLNode{Type: op.typ, Kids: []QLNode{*node, right}}
			return
		}
	}
}

func pExprAdd(p *parser, node *QLNode) {
	pExprMul(p, node)
	for {
		var typ uint32
		switch {
		case pKeyword(p, "+"):
			typ = qlAdd
		case pKeyword(p, "-"):
			typ = qlSub
		default:
			return
		}
		var right QLNode
		pExprMul(p, &right)
		*node = QLNode{Type: typ, Kids: []QLNode{*node, right}}
	}
}

func pExprMul(p *parser, node *QLNode) {
	pExprUnop(p, node)
	for {
		var typ uint32
		switch {
		case pKeyword(p, "*"):
			typ = qlMul
		case pKeyword(p, "/"):
			typ = qlDiv
		case pKeyword(p, "%"):
			typ = qlMod
		default:
			return
		}
		var right QLNode
		pExprUnop(p, &right)
		*node = QLNode{Type: typ, Kids: []QLNode{*node, right}}
	}
}

func pExprUnop(p *parser, node *QLNode) {
	if pKeyword(p, "-") {
		var kid QLNode
		pExprUnop(p, &kid)
		*node = QLNode{Type: qlNeg, Kids: []QLNode{kid}}
		return
	}
	pExprAtom(p, node)
}

// a name, a literal value, or a parenthesized expression / tuple
func pExprAtom(p *parser, node *QLNode) {
	switch {
	case pKeyword(p, "("):
		pExprOr(p, node)
		if pKeyword(p, ",") { // tuple
			kids := []QLNode{*node}
			for {
				var kid QLNode
				pExprOr(p, &kid)
				kids = append(kids, kid)
				if !pKeyword(p, ",") {
					break
				}
			}
			*node = QLNode{Type: qlTuple, Kids: kids}
		}
		pExpect(p, ")", "expect `)`")
	default:
		if name, ok := pSym(p); ok {
			*node = QLNode{Type: qlSym, Value: Value{Type: TypeBytes, Str: []byte(name)}}
		} else if n, ok := pNum(p); ok {
			*node = QLNode{Type: qlI64, Value: Value{Type: TypeInt64, I64: n}}
		} else if s, ok := pString(p); ok {
			*node = QLNode{Type: qlStr, Value: Value{Type: TypeBytes, Str: s}}
		} else {
			p.skipSpace()
			p.fail("expect a column name, a number, a string or `(`")
		}
	}
}
