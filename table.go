package minidb

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ---------------------------------------------------------------------------
// data model
// ---------------------------------------------------------------------------

// column types
const (
	TypeBytes = 1 // string (of arbitrary bytes)
	TypeInt64 = 2 // integer; 64-bit signed
)

// Value is a table cell: a tagged union.
type Value struct {
	Type uint32
	I64  int64
	Str  []byte
}

func (v Value) String() string {
	switch v.Type {
	case TypeInt64:
		return fmt.Sprint(v.I64)
	case TypeBytes:
		return string(v.Str)
	}
	return "NULL"
}

// Record is a list of column names and values (a table row).
type Record struct {
	Cols []string
	Vals []Value
}

func (rec *Record) AddStr(col string, val []byte) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Value{Type: TypeBytes, Str: val})
	return rec
}

func (rec *Record) AddInt64(col string, val int64) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Value{Type: TypeInt64, I64: val})
	return rec
}

func (rec *Record) Get(col string) *Value {
	for i, c := range rec.Cols {
		if c == col {
			return &rec.Vals[i]
		}
	}
	return nil
}

// TableDef is a table schema.
type TableDef struct {
	// user defined
	Name    string
	Types   []uint32   // column types
	Cols    []string   // column names
	Indexes [][]string // the first index is the primary key
	// auto-assigned B-tree key prefixes for different tables and indexes
	Prefixes []uint32
}

// internal tables
var tdefMeta = &TableDef{
	Name:     "@meta",
	Types:    []uint32{TypeBytes, TypeBytes},
	Cols:     []string{"key", "val"},
	Indexes:  [][]string{{"key"}},
	Prefixes: []uint32{1},
}

var tdefTable = &TableDef{
	Name:     "@table",
	Types:    []uint32{TypeBytes, TypeBytes},
	Cols:     []string{"name", "def"},
	Indexes:  [][]string{{"name"}},
	Prefixes: []uint32{2},
}

const tablePrefixMin = 100

func (t *TableDef) colIndex(name string) int { return slices.Index(t.Cols, name) }
func (t *TableDef) pkCols() []string         { return t.Indexes[0] }
func (t *TableDef) isPK(col int) bool        { return slices.Contains(t.Indexes[0], t.Cols[col]) }

// validate a user supplied schema and complete the secondary indexes with the
// primary key columns so that every index key is unique.
func tableDefCheck(tdef *TableDef) error {
	if tdef.Name == "" || tdef.Name[0] == '@' {
		return fmt.Errorf("bad table name: %q", tdef.Name)
	}
	if len(tdef.Cols) == 0 || len(tdef.Cols) != len(tdef.Types) {
		return errors.New("bad columns")
	}
	for i, c := range tdef.Cols {
		if c == "" || slices.Index(tdef.Cols, c) != i {
			return fmt.Errorf("bad or duplicate column name: %q", c)
		}
		if tdef.Types[i] != TypeBytes && tdef.Types[i] != TypeInt64 {
			return fmt.Errorf("bad type of column %q", c)
		}
	}
	if len(tdef.Indexes) == 0 || len(tdef.Indexes[0]) == 0 {
		return errors.New("a primary key is required")
	}
	pk := tdef.Indexes[0]
	for i, index := range tdef.Indexes {
		seen := map[string]bool{}
		for _, c := range index {
			if tdef.colIndex(c) < 0 {
				return fmt.Errorf("unknown column %q in index", c)
			}
			if seen[c] {
				return fmt.Errorf("duplicate column %q in index", c)
			}
			seen[c] = true
		}
		if i > 0 { // append the primary key columns
			for _, c := range pk {
				if !seen[c] {
					tdef.Indexes[i] = append(tdef.Indexes[i], c)
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// order-preserving encoding
// ---------------------------------------------------------------------------

// 00 -> 01 01, 01 -> 01 02: so that strings can be 0-terminated
func escapeString(in []byte) []byte {
	zeros := bytes.Count(in, []byte{0})
	ones := bytes.Count(in, []byte{1})
	if zeros+ones == 0 {
		return in
	}
	out := make([]byte, 0, len(in)+zeros+ones)
	for _, ch := range in {
		if ch <= 1 {
			out = append(out, 0x01, ch+1)
		} else {
			out = append(out, ch)
		}
	}
	return out
}

func unescapeString(in []byte) []byte {
	if bytes.IndexByte(in, 1) < 0 {
		return in
	}
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] == 1 {
			i++
			out = append(out, in[i]-1)
		} else {
			out = append(out, in[i])
		}
	}
	return out
}

// order-preserving encoding of a tuple. Each column starts with its type tag,
// which is never 0xff (used as +infinity).
func encodeValues(out []byte, vals []Value) []byte {
	for _, v := range vals {
		out = append(out, byte(v.Type))
		switch v.Type {
		case TypeInt64:
			var buf [8]byte
			u := uint64(v.I64) + (1 << 63)        // flip the sign bit
			binary.BigEndian.PutUint64(buf[:], u) // big endian
			out = append(out, buf[:]...)
		case TypeBytes:
			out = append(out, escapeString(v.Str)...)
			out = append(out, 0) // null-terminated
		default:
			panic("bad value type")
		}
	}
	return out
}

// the types of `out` must be set; the values are filled in.
func decodeValues(in []byte, out []Value) error {
	for i := range out {
		if len(in) == 0 || in[0] != byte(out[i].Type) {
			return errors.New("corrupted data: bad type tag")
		}
		in = in[1:]
		switch out[i].Type {
		case TypeInt64:
			if len(in) < 8 {
				return errors.New("corrupted data")
			}
			out[i].I64 = int64(binary.BigEndian.Uint64(in[:8]) - (1 << 63))
			in = in[8:]
		case TypeBytes:
			idx := bytes.IndexByte(in, 0)
			if idx < 0 {
				return errors.New("corrupted data: unterminated string")
			}
			out[i].Str = unescapeString(in[:idx])
			in = in[idx+1:]
		}
	}
	return nil
}

// for primary keys and indexes: 4-byte prefix + encoded columns
func encodeKey(out []byte, prefix uint32, vals []Value) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], prefix)
	out = append(out, buf[:]...)
	return encodeValues(out, vals)
}

// for the input range, which can be a prefix of the index key.
func encodeKeyPartial(out []byte, prefix uint32, vals []Value, cmp int) []byte {
	out = encodeKey(out, prefix, vals)
	if cmp == CmpGT || cmp == CmpLE { // encode missing columns as infinity
		out = append(out, 0xff) // unreachable +infinity
	} // else: -infinity is the empty string
	return out
}

// ---------------------------------------------------------------------------
// DB, transactions
// ---------------------------------------------------------------------------

// DB is a relational database on top of KV.
type DB struct {
	Path string
	kv   KV
}

// DBTX is a database transaction.
type DBTX struct {
	kv KVTX
	db *DB
}

func (db *DB) Open() error {
	db.kv.Path = db.Path
	return db.kv.Open()
}

func (db *DB) Close() error { return db.kv.Close() }

func (db *DB) Begin(tx *DBTX) {
	tx.db = db
	db.kv.Begin(&tx.kv)
}

func (db *DB) Commit(tx *DBTX) error { return db.kv.Commit(&tx.kv) }
func (db *DB) Abort(tx *DBTX)        { db.kv.Abort(&tx.kv) }

// View runs fn on a consistent snapshot.
func (db *DB) View(fn func(tx *DBTX) error) error {
	var tx DBTX
	db.Begin(&tx)
	defer db.Abort(&tx)
	return fn(&tx)
}

// Update runs fn in a transaction and commits it. On conflict fn is run
// again, so it must be safe to repeat.
func (db *DB) Update(fn func(tx *DBTX) error) error {
	for i := 0; ; i++ {
		var tx DBTX
		db.Begin(&tx)
		if err := fn(&tx); err != nil {
			db.Abort(&tx)
			return err
		}
		err := db.Commit(&tx)
		if errors.Is(err, ErrConflict) && i < maxRetries {
			continue
		}
		return err
	}
}

// ---------------------------------------------------------------------------
// schema lookup
// ---------------------------------------------------------------------------

func (tx *DBTX) getTableDef(name string) (*TableDef, error) {
	switch name {
	case tdefMeta.Name:
		return tdefMeta, nil
	case tdefTable.Name:
		return tdefTable, nil
	}
	rec := (&Record{}).AddStr("name", []byte(name))
	ok, err := dbGet(tx, tdefTable, rec)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("table not found: %s", name)
	}
	tdef := &TableDef{}
	if err := json.Unmarshal(rec.Get("def").Str, tdef); err != nil {
		return nil, fmt.Errorf("corrupted schema: %w", err)
	}
	return tdef, nil
}

// ListTables returns the names of all user tables, sorted.
func (tx *DBTX) ListTables() (names []string, err error) {
	sc := Scanner{Cmp1: CmpGE, Cmp2: CmpLE}
	if err = dbScan(tx, tdefTable, &sc); err != nil {
		return nil, err
	}
	for ; sc.Valid(); sc.Next() {
		var rec Record
		if err = sc.Deref(&rec); err != nil {
			return nil, err
		}
		names = append(names, string(rec.Get("name").Str))
	}
	return names, nil
}

// TableDef returns a copy of the schema of a table.
func (tx *DBTX) TableDef(name string) (*TableDef, error) { return tx.getTableDef(name) }

// TableNew creates a table.
func (tx *DBTX) TableNew(tdef *TableDef) (err error) {
	defer catchIO(&err)
	if err := tableDefCheck(tdef); err != nil {
		return err
	}
	// check for duplicate names
	table := (&Record{}).AddStr("name", []byte(tdef.Name))
	ok, err := dbGet(tx, tdefTable, table)
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("table exists: %s", tdef.Name)
	}
	// allocate B+tree key prefixes
	prefix := uint32(tablePrefixMin)
	meta := (&Record{}).AddStr("key", []byte("next_prefix"))
	ok, err = dbGet(tx, tdefMeta, meta)
	if err != nil {
		return err
	}
	if ok {
		prefix = binary.LittleEndian.Uint32(meta.Get("val").Str)
		if prefix < tablePrefixMin {
			return errors.New("corrupted meta table")
		}
	}
	tdef.Prefixes = tdef.Prefixes[:0]
	for i := range tdef.Indexes {
		tdef.Prefixes = append(tdef.Prefixes, prefix+uint32(i))
	}
	next := prefix + uint32(len(tdef.Indexes))
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], next)
	meta = (&Record{}).AddStr("key", []byte("next_prefix")).AddStr("val", buf[:])
	if _, err = dbUpdate(tx, tdefMeta, *meta, ModeUpsert); err != nil {
		return err
	}
	// store the definition
	val, err := json.Marshal(tdef)
	if err != nil {
		return err
	}
	table.AddStr("def", val)
	_, err = dbUpdate(tx, tdefTable, *table, ModeInsertOnly)
	return err
}

// ---------------------------------------------------------------------------
// point query, insert, update, delete
// ---------------------------------------------------------------------------

// reorder a record into schema order and check for missing columns.
// full=false: the record must contain the primary key columns.
// full=true: the record must contain exactly all columns.
func checkRecord(tdef *TableDef, rec Record, full bool) ([]Value, error) {
	if len(rec.Cols) != len(rec.Vals) {
		return nil, errors.New("bad record")
	}
	values := make([]Value, len(tdef.Cols))
	set := make([]bool, len(tdef.Cols))
	for i, c := range rec.Cols {
		idx := tdef.colIndex(c)
		if idx < 0 {
			return nil, fmt.Errorf("unknown column: %s", c)
		}
		if set[idx] {
			return nil, fmt.Errorf("duplicate column: %s", c)
		}
		if rec.Vals[i].Type != tdef.Types[idx] {
			return nil, fmt.Errorf("bad value type for column %s", c)
		}
		values[idx], set[idx] = rec.Vals[i], true
	}
	for i, c := range tdef.Cols {
		if (full || tdef.isPK(i)) && !set[i] {
			return nil, fmt.Errorf("missing column: %s", c)
		}
	}
	return values, nil
}

func pick(tdef *TableDef, values []Value, cols []string) []Value {
	out := make([]Value, len(cols))
	for i, c := range cols {
		out[i] = values[tdef.colIndex(c)]
	}
	return out
}

func encodeRow(tdef *TableDef, values []Value) (key, val []byte) {
	key = encodeKey(nil, tdef.Prefixes[0], pick(tdef, values, tdef.pkCols()))
	var rest []Value
	for i := range tdef.Cols {
		if !tdef.isPK(i) {
			rest = append(rest, values[i])
		}
	}
	return key, encodeValues(nil, rest)
}

// fill the non-primary-key columns from the KV value
func decodeRow(tdef *TableDef, values []Value, val []byte) error {
	var idx []int
	var rest []Value
	for i := range tdef.Cols {
		if !tdef.isPK(i) {
			idx = append(idx, i)
			rest = append(rest, Value{Type: tdef.Types[i]})
		}
	}
	if err := decodeValues(val, rest); err != nil {
		return err
	}
	for j, i := range idx {
		values[i] = rest[j]
	}
	return nil
}

// get a single row by the primary key
func dbGet(tx *DBTX, tdef *TableDef, rec *Record) (bool, error) {
	values, err := checkRecord(tdef, *rec, false)
	if err != nil {
		return false, err
	}
	key := encodeKey(nil, tdef.Prefixes[0], pick(tdef, values, tdef.pkCols()))
	val, ok, err := tx.kv.Get(key)
	if err != nil || !ok {
		return false, err
	}
	if err := decodeRow(tdef, values, val); err != nil {
		return false, err
	}
	rec.Cols, rec.Vals = tdef.Cols, values
	return true, nil
}

// Get reads a row by its primary key. rec is both the input (the primary key
// columns) and the output (all columns).
func (tx *DBTX) Get(table string, rec *Record) (found bool, err error) {
	defer catchIO(&err)
	tdef, err := tx.getTableDef(table)
	if err != nil {
		return false, err
	}
	return dbGet(tx, tdef, rec)
}

func (tx *DBTX) indexKeys(tdef *TableDef, values []Value) [][]byte {
	keys := make([][]byte, len(tdef.Indexes))
	for i := 1; i < len(tdef.Indexes); i++ {
		keys[i] = encodeKey(nil, tdef.Prefixes[i], pick(tdef, values, tdef.Indexes[i]))
	}
	return keys
}

// add or replace a full row, maintaining secondary indexes
func dbUpdate(tx *DBTX, tdef *TableDef, rec Record, mode int) (bool, error) {
	values, err := checkRecord(tdef, rec, true)
	if err != nil {
		return false, err
	}
	key, val := encodeRow(tdef, values)
	req := UpdateReq{Key: key, Val: val, Mode: mode}
	if _, err = tx.kv.Update(&req); err != nil {
		return false, err
	}
	if !req.Updated {
		return false, nil
	}
	if len(tdef.Indexes) > 1 {
		newKeys := tx.indexKeys(tdef, values)
		if !req.Added { // remove the stale index keys
			oldValues := slices.Clone(values)
			if err := decodeRow(tdef, oldValues, req.Old); err != nil {
				return false, err
			}
			oldKeys := tx.indexKeys(tdef, oldValues)
			for i := 1; i < len(tdef.Indexes); i++ {
				if bytes.Equal(oldKeys[i], newKeys[i]) {
					newKeys[i] = nil // unchanged
					continue
				}
				if _, err := tx.kv.Del(&DeleteReq{Key: oldKeys[i]}); err != nil {
					return false, err
				}
			}
		}
		for i := 1; i < len(tdef.Indexes); i++ {
			if newKeys[i] == nil {
				continue
			}
			ireq := UpdateReq{Key: newKeys[i], Mode: ModeUpsert}
			if _, err := tx.kv.Update(&ireq); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func dbDelete(tx *DBTX, tdef *TableDef, rec Record) (bool, error) {
	values, err := checkRecord(tdef, rec, false)
	if err != nil {
		return false, err
	}
	key := encodeKey(nil, tdef.Prefixes[0], pick(tdef, values, tdef.pkCols()))
	req := DeleteReq{Key: key}
	deleted, err := tx.kv.Del(&req)
	if err != nil || !deleted {
		return false, err
	}
	if len(tdef.Indexes) > 1 {
		if err := decodeRow(tdef, values, req.Old); err != nil {
			return false, err
		}
		for i, k := range tx.indexKeys(tdef, values) {
			if i == 0 {
				continue
			}
			if _, err := tx.kv.Del(&DeleteReq{Key: k}); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// Set inserts, updates or upserts a full row depending on mode (ModeUpsert,
// ModeUpdateOnly, ModeInsertOnly). Returns whether the row changed.
func (tx *DBTX) Set(table string, rec Record, mode int) (changed bool, err error) {
	defer catchIO(&err)
	tdef, err := tx.getTableDef(table)
	if err != nil {
		return false, err
	}
	return dbUpdate(tx, tdef, rec, mode)
}

func (tx *DBTX) Insert(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, ModeInsertOnly)
}
func (tx *DBTX) Update(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, ModeUpdateOnly)
}
func (tx *DBTX) Upsert(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, ModeUpsert)
}

// Delete removes a row by its primary key.
func (tx *DBTX) Delete(table string, rec Record) (deleted bool, err error) {
	defer catchIO(&err)
	tdef, err := tx.getTableDef(table)
	if err != nil {
		return false, err
	}
	return dbDelete(tx, tdef, rec)
}

// ---------------------------------------------------------------------------
// range queries
// ---------------------------------------------------------------------------

// Scanner iterates over rows within a range of an index. Key1 and Key2 are
// prefixes of the index columns (in index order); Cmp1 and Cmp2 are CmpXX
// operators: ascending scans use CmpGE/CmpGT with CmpLE/CmpLT, descending
// scans the opposite. The index is selected by matching the columns of Key1
// and Key2.
type Scanner struct {
	// the range, from Key1 to Key2
	Cmp1 int
	Cmp2 int
	Key1 Record
	Key2 Record
	// internal
	tx     *DBTX
	tdef   *TableDef
	index  int // which index?
	iter   *KVIter
	keyEnd []byte // the encoded Key2
}

func (tx *DBTX) Scan(table string, req *Scanner) (err error) {
	defer catchIO(&err)
	tdef, err := tx.getTableDef(table)
	if err != nil {
		return err
	}
	return dbScan(tx, tdef, req)
}

func checkKey(tdef *TableDef, key Record) error {
	if len(key.Cols) != len(key.Vals) {
		return errors.New("bad key")
	}
	for i, c := range key.Cols {
		idx := tdef.colIndex(c)
		if idx < 0 {
			return fmt.Errorf("unknown column: %s", c)
		}
		if key.Vals[i].Type != tdef.Types[idx] {
			return fmt.Errorf("bad value type for column %s", c)
		}
	}
	return nil
}

func dbScan(tx *DBTX, tdef *TableDef, req *Scanner) error {
	switch {
	case req.Cmp1 > 0 && req.Cmp2 < 0:
	case req.Cmp2 > 0 && req.Cmp1 < 0:
	default:
		return errors.New("bad range")
	}
	if err := checkKey(tdef, req.Key1); err != nil {
		return err
	}
	if err := checkKey(tdef, req.Key2); err != nil {
		return err
	}
	// select an index: both keys must be prefixes of its columns
	covered := func(key []string, index []string) bool {
		return len(index) >= len(key) && slices.Equal(index[:len(key)], key)
	}
	req.index = slices.IndexFunc(tdef.Indexes, func(index []string) bool {
		return covered(req.Key1.Cols, index) && covered(req.Key2.Cols, index)
	})
	if req.index < 0 {
		return fmt.Errorf("no index found for columns %v / %v", req.Key1.Cols, req.Key2.Cols)
	}
	prefix := tdef.Prefixes[req.index]
	start := encodeKeyPartial(nil, prefix, req.Key1.Vals, req.Cmp1)
	req.keyEnd = encodeKeyPartial(nil, prefix, req.Key2.Vals, req.Cmp2)
	req.tx, req.tdef = tx, tdef

	// the whole range is a dependency of the transaction (phantoms)
	lo, hi := start, req.keyEnd
	if bytes.Compare(lo, hi) > 0 {
		lo, hi = hi, lo
	}
	tx.kv.AddRead(lo, append(slices.Clone(hi), 0xff))

	var err error
	req.iter, err = tx.kv.Seek(start, req.Cmp1)
	return err
}

// within the range or not?
func (sc *Scanner) Valid() bool {
	if !sc.iter.Valid() {
		return false
	}
	key, _ := sc.iter.Deref()
	return cmpOK(key, sc.Cmp2, sc.keyEnd)
}

// move the underlying iterator
func (sc *Scanner) Next() {
	assert(sc.Valid())
	sc.iter.Next()
}

// fetch the current row
func (sc *Scanner) Deref(rec *Record) (err error) {
	defer catchIO(&err)
	assert(sc.Valid())
	tdef := sc.tdef
	key, val := sc.iter.Deref()
	values := make([]Value, len(tdef.Cols))
	if sc.index == 0 {
		pk := make([]Value, len(tdef.pkCols()))
		for i, c := range tdef.pkCols() {
			pk[i].Type = tdef.Types[tdef.colIndex(c)]
		}
		if err := decodeValues(key[4:], pk); err != nil {
			return err
		}
		for i, c := range tdef.pkCols() {
			values[tdef.colIndex(c)] = pk[i]
		}
		if err := decodeRow(tdef, values, val); err != nil {
			return err
		}
	} else {
		// a secondary index: the key has the primary key; fetch the row
		cols := tdef.Indexes[sc.index]
		ivals := make([]Value, len(cols))
		for i, c := range cols {
			ivals[i].Type = tdef.Types[tdef.colIndex(c)]
		}
		if err := decodeValues(key[4:], ivals); err != nil {
			return err
		}
		for i, c := range cols {
			values[tdef.colIndex(c)] = ivals[i]
		}
		pkey := encodeKey(nil, tdef.Prefixes[0], pick(tdef, values, tdef.pkCols()))
		row, ok, err := sc.tx.kv.Get(pkey)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("corrupted index: row not found")
		}
		if err := decodeRow(tdef, values, row); err != nil {
			return err
		}
	}
	rec.Cols, rec.Vals = tdef.Cols, values
	return nil
}
