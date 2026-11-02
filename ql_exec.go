package minidb

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// expression evaluation
// ---------------------------------------------------------------------------

type qlEvalCtx struct {
	env Record // input row values
	out Value  // output
	err error
}

func qlErr(ctx *qlEvalCtx, format string, args ...any) {
	if ctx.err == nil {
		ctx.out = Value{}
		ctx.err = fmt.Errorf(format, args...)
	}
}

func boolValue(b bool) Value {
	if b {
		return Value{Type: TypeInt64, I64: 1}
	}
	return Value{Type: TypeInt64, I64: 0}
}

func qlEval(ctx *qlEvalCtx, node QLNode) {
	if ctx.err != nil {
		return
	}
	switch node.Type {
	case qlSym:
		if v := ctx.env.Get(string(node.Str)); v != nil {
			ctx.out = *v
		} else {
			qlErr(ctx, "unknown column: %s", node.Str)
		}
	case qlI64, qlStr:
		ctx.out = node.Value
	case qlTuple:
		qlErr(ctx, "a tuple is not allowed here")
	case qlNeg, qlNot:
		qlEval(ctx, node.Kids[0])
		if ctx.err != nil {
			return
		}
		if ctx.out.Type != TypeInt64 {
			qlErr(ctx, "type error: expect a number")
			return
		}
		if node.Type == qlNeg {
			ctx.out.I64 = -ctx.out.I64
		} else {
			ctx.out = boolValue(ctx.out.I64 == 0)
		}
	case qlAnd, qlOr:
		qlEval(ctx, node.Kids[0])
		if ctx.err != nil {
			return
		}
		if ctx.out.Type != TypeInt64 {
			qlErr(ctx, "type error: expect a boolean")
			return
		}
		l := ctx.out.I64 != 0
		if (node.Type == qlAnd && !l) || (node.Type == qlOr && l) {
			ctx.out = boolValue(l) // short circuit
			return
		}
		qlEval(ctx, node.Kids[1])
		if ctx.err == nil && ctx.out.Type != TypeInt64 {
			qlErr(ctx, "type error: expect a boolean")
			return
		}
		ctx.out = boolValue(ctx.err == nil && ctx.out.I64 != 0)
	default:
		if node.Type >= qlAdd && node.Type <= qlCmpNE {
			qlEvalBinop(ctx, node)
			return
		}
		qlErr(ctx, "bad expression")
	}
}

func qlEvalBinop(ctx *qlEvalCtx, node QLNode) {
	qlEval(ctx, node.Kids[0])
	l := ctx.out
	qlEval(ctx, node.Kids[1])
	r := ctx.out
	if ctx.err != nil {
		return
	}
	if l.Type != r.Type {
		qlErr(ctx, "type error: cannot mix numbers and strings")
		return
	}
	if isCmp(node.Type) {
		var c int
		if l.Type == TypeInt64 {
			switch {
			case l.I64 < r.I64:
				c = -1
			case l.I64 > r.I64:
				c = 1
			}
		} else {
			c = bytes.Compare(l.Str, r.Str)
		}
		var b bool
		switch node.Type {
		case qlCmpGE:
			b = c >= 0
		case qlCmpGT:
			b = c > 0
		case qlCmpLT:
			b = c < 0
		case qlCmpLE:
			b = c <= 0
		case qlCmpEQ:
			b = c == 0
		case qlCmpNE:
			b = c != 0
		}
		ctx.out = boolValue(b)
		return
	}
	if l.Type == TypeBytes {
		if node.Type == qlAdd { // string concatenation
			ctx.out = Value{Type: TypeBytes, Str: append(slices.Clone(l.Str), r.Str...)}
			return
		}
		qlErr(ctx, "type error: operator not defined for strings")
		return
	}
	switch node.Type {
	case qlAdd:
		ctx.out = Value{Type: TypeInt64, I64: l.I64 + r.I64}
	case qlSub:
		ctx.out = Value{Type: TypeInt64, I64: l.I64 - r.I64}
	case qlMul:
		ctx.out = Value{Type: TypeInt64, I64: l.I64 * r.I64}
	case qlDiv, qlMod:
		if r.I64 == 0 {
			qlErr(ctx, "division by zero")
			return
		}
		if node.Type == qlDiv {
			ctx.out = Value{Type: TypeInt64, I64: l.I64 / r.I64}
		} else {
			ctx.out = Value{Type: TypeInt64, I64: l.I64 % r.I64}
		}
	}
}

func qlEvalOne(env Record, node QLNode) (Value, error) {
	ctx := qlEvalCtx{env: env}
	qlEval(&ctx, node)
	return ctx.out, ctx.err
}

// ---------------------------------------------------------------------------
// range queries: INDEX BY -> Scanner
// ---------------------------------------------------------------------------

// `a > 1` or `(a, b) >= (1, 2)` to a record and a comparison operator
func qlEvalScanKey(node QLNode) (Record, int, error) {
	if node.Type == qlUninit {
		return Record{}, 0, nil
	}
	cmp := 0
	switch node.Type {
	case qlCmpGE, qlCmpEQ:
		cmp = CmpGE
	case qlCmpGT:
		cmp = CmpGT
	case qlCmpLT:
		cmp = CmpLT
	case qlCmpLE:
		cmp = CmpLE
	default:
		return Record{}, 0, errors.New("INDEX BY does not support this comparison")
	}
	names, vals := node.Kids[0], node.Kids[1]
	var cols []string
	var kids []QLNode
	if names.Type == qlTuple {
		if vals.Type != qlTuple || len(vals.Kids) != len(names.Kids) {
			return Record{}, 0, errors.New("INDEX BY: tuple size mismatch")
		}
		for _, k := range names.Kids {
			if k.Type != qlSym {
				return Record{}, 0, errors.New("INDEX BY expects column names on the left")
			}
			cols = append(cols, string(k.Str))
		}
		kids = vals.Kids
	} else {
		if names.Type != qlSym {
			return Record{}, 0, errors.New("INDEX BY expects a column name on the left")
		}
		cols, kids = []string{string(names.Str)}, []QLNode{vals}
	}
	rec := Record{Cols: cols}
	for _, k := range kids {
		v, err := qlEvalOne(Record{}, k)
		if err != nil {
			return Record{}, 0, fmt.Errorf("INDEX BY: %w", err)
		}
		rec.Vals = append(rec.Vals, v)
	}
	return rec, cmp, nil
}

func qlScanInit(req *QLScan, sc *Scanner) (err error) {
	if sc.Key1, sc.Cmp1, err = qlEvalScanKey(req.Key1); err != nil {
		return err
	}
	if sc.Key2, sc.Cmp2, err = qlEvalScanKey(req.Key2); err != nil {
		return err
	}
	switch { // special handling when `Key1` and `Key2` are not both present
	case req.Key1.Type == 0 && req.Key2.Type == 0: // no `INDEX BY`
		sc.Cmp1, sc.Cmp2 = CmpGE, CmpLE // full table scan
	case req.Key1.Type == qlCmpEQ && req.Key2.Type == 0:
		// equal by a prefix: INDEX BY key = val
		sc.Key2 = sc.Key1
		sc.Cmp1, sc.Cmp2 = CmpGE, CmpLE
	case req.Key1.Type != 0 && req.Key2.Type == 0:
		// open-ended range: INDEX BY key > val
		if sc.Cmp1 > 0 {
			sc.Cmp2 = CmpLE // compare with a zero-length tuple
		} else {
			sc.Cmp2 = CmpGE
		}
	case req.Key1.Type == qlCmpEQ || req.Key2.Type == qlCmpEQ:
		return errors.New("INDEX BY: `=` cannot be combined with another condition")
	}
	return nil
}

// ---------------------------------------------------------------------------
// iterators all the way down
// ---------------------------------------------------------------------------

type recordIter interface {
	Valid() bool
	Next()
	Deref(*Record) error
}

// qlScanIter: INDEX BY via the Scanner, then FILTER, then OFFSET/LIMIT.
type qlScanIter struct {
	req     *QLScan
	sc      Scanner
	matched int64 // rows that passed the filter so far
	end     bool
	rec     Record
	err     error
}

func qlScanIterInit(tx *DBTX, req *QLScan) (*qlScanIter, error) {
	it := &qlScanIter{req: req}
	if err := qlScanInit(req, &it.sc); err != nil {
		return nil, err
	}
	if err := tx.Scan(req.Table, &it.sc); err != nil {
		return nil, err
	}
	it.seek()
	return it, it.err
}

// move to the next row that passes the filter and the limit
func (it *qlScanIter) seek() {
	for ; it.sc.Valid() && it.err == nil; it.sc.Next() {
		if it.req.Limit <= 0 || max(0, it.matched-it.req.Offset) >= it.req.Limit {
			break
		}
		var rec Record
		if it.err = it.sc.Deref(&rec); it.err != nil {
			return
		}
		ok := true
		if it.req.Filter.Type != qlUninit {
			v, err := qlEvalOne(rec, it.req.Filter)
			if err != nil {
				it.err = fmt.Errorf("FILTER: %w", err)
				return
			}
			if v.Type != TypeInt64 {
				it.err = errors.New("FILTER: expect a boolean expression")
				return
			}
			ok = v.I64 != 0
		}
		if !ok {
			continue
		}
		it.matched++
		if it.matched <= it.req.Offset {
			continue
		}
		it.rec = rec
		return
	}
	it.end = true
}

func (it *qlScanIter) Valid() bool { return !it.end && it.err == nil }
func (it *qlScanIter) Next() {
	it.sc.Next()
	it.seek()
}
func (it *qlScanIter) Deref(rec *Record) error {
	*rec = Record{Cols: it.rec.Cols, Vals: slices.Clone(it.rec.Vals)}
	return it.err
}

// qlSelectIter: evaluate the SELECT expressions
type qlSelectIter struct {
	iter  recordIter
	names []string
	exprs []QLNode
}

func (it *qlSelectIter) Valid() bool { return it.iter.Valid() }
func (it *qlSelectIter) Next()       { it.iter.Next() }
func (it *qlSelectIter) Deref(rec *Record) error {
	if err := it.iter.Deref(rec); err != nil {
		return err
	}
	vals := make([]Value, len(it.exprs))
	for i, e := range it.exprs {
		v, err := qlEvalOne(*rec, e)
		if err != nil {
			return err
		}
		vals[i] = v
	}
	*rec = Record{Cols: it.names, Vals: vals}
	return nil
}

// ---------------------------------------------------------------------------
// statements
// ---------------------------------------------------------------------------

// QLResult is the outcome of a statement.
type QLResult struct {
	Cols     []string  // for queries
	Rows     [][]Value // for queries
	Affected int64     // for insert / update / delete
	Message  string
}

func (tx *DBTX) qlExec(stmt any) (res *QLResult, err error) {
	defer catchIO(&err)
	switch s := stmt.(type) {
	case *QLCreateTable:
		def := s.Def
		if err := tx.TableNew(&def); err != nil {
			return nil, err
		}
		return &QLResult{Message: "table created: " + def.Name}, nil
	case *QLSelect:
		return qlSelect(tx, s)
	case *QLInsert:
		return qlInsert(tx, s)
	case *QLUpdate:
		return qlUpdate(tx, s)
	case *QLDelete:
		return qlDelete(tx, s)
	case *QLShowTables:
		names, err := tx.ListTables()
		if err != nil {
			return nil, err
		}
		res := &QLResult{Cols: []string{"table"}}
		for _, n := range names {
			res.Rows = append(res.Rows, []Value{{Type: TypeBytes, Str: []byte(n)}})
		}
		return res, nil
	case *QLDescribe:
		return qlDescribe(tx, s)
	}
	return nil, fmt.Errorf("unsupported statement %T", stmt)
}

func typeName(t uint32) string {
	if t == TypeInt64 {
		return "int64"
	}
	return "string"
}

func qlDescribe(tx *DBTX, s *QLDescribe) (*QLResult, error) {
	tdef, err := tx.getTableDef(s.Table)
	if err != nil {
		return nil, err
	}
	res := &QLResult{Cols: []string{"column", "type", "key"}}
	str := func(s string) Value { return Value{Type: TypeBytes, Str: []byte(s)} }
	for i, c := range tdef.Cols {
		key := ""
		if tdef.isPK(i) {
			key = "primary"
		}
		res.Rows = append(res.Rows, []Value{str(c), str(typeName(tdef.Types[i])), str(key)})
	}
	for i, idx := range tdef.Indexes {
		if i > 0 {
			res.Rows = append(res.Rows, []Value{str("(" + strings.Join(idx, ", ") + ")"), str("index"), str("")})
		}
	}
	return res, nil
}

func qlSelect(tx *DBTX, req *QLSelect) (*QLResult, error) {
	tdef, err := tx.getTableDef(req.Table)
	if err != nil {
		return nil, err
	}
	// expand `*`
	var names []string
	var exprs []QLNode
	for i, e := range req.Output {
		if e.Type == qlStar {
			for _, c := range tdef.Cols {
				names = append(names, c)
				exprs = append(exprs, QLNode{Type: qlSym, Value: Value{Type: TypeBytes, Str: []byte(c)}})
			}
		} else {
			names = append(names, req.Names[i])
			exprs = append(exprs, e)
		}
	}
	scan, err := qlScanIterInit(tx, &req.QLScan)
	if err != nil {
		return nil, err
	}
	iter := &qlSelectIter{iter: scan, names: names, exprs: exprs}
	res := &QLResult{Cols: names}
	for ; iter.Valid(); iter.Next() {
		var rec Record
		if err := iter.Deref(&rec); err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, rec.Vals)
	}
	if scan.err != nil {
		return nil, scan.err
	}
	return res, nil
}

func qlInsert(tx *DBTX, req *QLInsert) (*QLResult, error) {
	res := &QLResult{}
	for _, row := range req.Values {
		rec := Record{Cols: req.Cols}
		for _, e := range row {
			v, err := qlEvalOne(Record{}, e)
			if err != nil {
				return nil, err
			}
			rec.Vals = append(rec.Vals, v)
		}
		ok, err := tx.Insert(req.Table, rec)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("duplicate primary key")
		}
		res.Affected++
	}
	return res, nil
}

// collect all matching rows first: the tree must not change under an iterator
func qlCollect(tx *DBTX, scan *QLScan) ([]Record, error) {
	it, err := qlScanIterInit(tx, scan)
	if err != nil {
		return nil, err
	}
	var rows []Record
	for ; it.Valid(); it.Next() {
		var rec Record
		if err := it.Deref(&rec); err != nil {
			return nil, err
		}
		rows = append(rows, rec)
	}
	return rows, it.err
}

func qlUpdate(tx *DBTX, req *QLUpdate) (*QLResult, error) {
	tdef, err := tx.getTableDef(req.Table)
	if err != nil {
		return nil, err
	}
	for _, n := range req.Names {
		i := tdef.colIndex(n)
		if i < 0 {
			return nil, fmt.Errorf("unknown column: %s", n)
		}
		if tdef.isPK(i) {
			return nil, fmt.Errorf("cannot update primary key column: %s", n)
		}
	}
	rows, err := qlCollect(tx, &req.QLScan)
	if err != nil {
		return nil, err
	}
	res := &QLResult{}
	for _, row := range rows {
		upd := Record{Cols: slices.Clone(row.Cols), Vals: slices.Clone(row.Vals)}
		for i, n := range req.Names {
			v, err := qlEvalOne(row, req.Values[i]) // always on the old row
			if err != nil {
				return nil, err
			}
			upd.Vals[tdef.colIndex(n)] = v
		}
		changed, err := tx.Update(req.Table, upd)
		if err != nil {
			return nil, err
		}
		if changed {
			res.Affected++
		}
	}
	return res, nil
}

func qlDelete(tx *DBTX, req *QLDelete) (*QLResult, error) {
	rows, err := qlCollect(tx, &req.QLScan)
	if err != nil {
		return nil, err
	}
	res := &QLResult{}
	for _, row := range rows {
		ok, err := tx.Delete(req.Table, row)
		if err != nil {
			return nil, err
		}
		if ok {
			res.Affected++
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// sessions: auto-commit statements and explicit BEGIN / COMMIT / ROLLBACK
// ---------------------------------------------------------------------------

// Session executes SQL text against a DB. Not safe for concurrent use; create
// one per connection.
type Session struct {
	db *DB
	tx *DBTX // explicit transaction, if any
}

func (db *DB) NewSession() *Session { return &Session{db: db} }

// InTx reports whether an explicit transaction is open.
func (s *Session) InTx() bool { return s.tx != nil }

// Close rolls back an open transaction.
func (s *Session) Close() {
	if s.tx != nil {
		s.db.Abort(s.tx)
		s.tx = nil
	}
}

// Exec parses and runs `;`-separated statements, stopping at the first error.
// Statements outside BEGIN..COMMIT run in their own transaction. An error
// inside an explicit transaction rolls it back.
func (s *Session) Exec(src string) ([]*QLResult, error) {
	stmts, err := QLParseAll(src)
	if err != nil {
		return nil, err
	}
	var results []*QLResult
	for _, stmt := range stmts {
		res, err := s.execOne(stmt)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

func (s *Session) execOne(stmt any) (*QLResult, error) {
	switch stmt.(type) {
	case *QLBegin:
		if s.tx != nil {
			return nil, errors.New("a transaction is already open")
		}
		s.tx = &DBTX{}
		s.db.Begin(s.tx)
		return &QLResult{Message: "transaction started"}, nil
	case *QLCommit:
		if s.tx == nil {
			return nil, errors.New("no transaction is open")
		}
		tx := s.tx
		s.tx = nil
		if err := s.db.Commit(tx); err != nil {
			if errors.Is(err, ErrConflict) {
				return nil, fmt.Errorf("%w: rolled back, please retry", err)
			}
			return nil, err
		}
		return &QLResult{Message: "committed"}, nil
	case *QLRollback:
		if s.tx == nil {
			return nil, errors.New("no transaction is open")
		}
		s.db.Abort(s.tx)
		s.tx = nil
		return &QLResult{Message: "rolled back"}, nil
	}
	if s.tx != nil {
		res, err := s.tx.qlExec(stmt)
		if err != nil {
			s.db.Abort(s.tx)
			s.tx = nil
			return nil, fmt.Errorf("%w (transaction rolled back)", err)
		}
		return res, nil
	}
	for attempt := 0; ; attempt++ {
		var tx DBTX
		s.db.Begin(&tx)
		res, err := tx.qlExec(stmt)
		if err != nil {
			s.db.Abort(&tx)
			return nil, err
		}
		err = s.db.Commit(&tx)
		if errors.Is(err, ErrConflict) && attempt < maxRetries {
			continue
		}
		return res, err
	}
}
