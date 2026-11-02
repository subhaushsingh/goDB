package minidb

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func openDB(t testing.TB, path string) *DB {
	t.Helper()
	db := &DB{Path: path}
	if err := db.Open(); err != nil {
		t.Fatal(err)
	}
	return db
}

// run statements; return the rows of the last result as strings
func run(t testing.TB, s *Session, sql string) [][]string {
	t.Helper()
	res, err := s.Exec(sql)
	if err != nil {
		t.Fatalf("%s\n -> %v", sql, err)
	}
	return rows(res[len(res)-1])
}

func rows(r *QLResult) [][]string {
	var out [][]string
	for _, row := range r.Rows {
		var cells []string
		for _, v := range row {
			cells = append(cells, v.String())
		}
		out = append(out, cells)
	}
	return out
}

func flat(r [][]string) string {
	var parts []string
	for _, row := range r {
		parts = append(parts, strings.Join(row, ","))
	}
	return strings.Join(parts, " | ")
}

func expect(t testing.TB, s *Session, sql, want string) {
	t.Helper()
	if got := flat(run(t, s, sql)); got != want {
		t.Fatalf("%s\n got:  %s\n want: %s", sql, got, want)
	}
}

func expectErr(t testing.TB, s *Session, sql, substr string) {
	t.Helper()
	_, err := s.Exec(sql)
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("%s\n got error %v, want %q", sql, err, substr)
	}
}

func TestSQLBasics(t *testing.T) {
	db := openDB(t, tmpPath(t))
	defer db.Close()
	s := db.NewSession()

	run(t, s, `create table users (
		id int64, name string, age int64, city string,
		primary key (id), index (city, age), index (name));`)
	run(t, s, `insert into users (id, name, age, city) values
		(1, 'alice', 30, 'paris'), (2, 'bob', 25, 'london'),
		(3, 'carol', 35, 'paris'), (4, 'dave', 25, 'paris'), (5, 'eve', 41, 'berlin')`)

	expect(t, s, "show tables", "users")
	expect(t, s, "select * from users", "1,alice,30,paris | 2,bob,25,london | 3,carol,35,paris | 4,dave,25,paris | 5,eve,41,berlin")
	expect(t, s, "select name, age + 1 as next from users index by id >= 2 and id < 4", "bob,26 | carol,36")
	expect(t, s, "select id from users index by id < 4 and id > 1", "3 | 2") // descending
	expect(t, s, "select id from users index by id = 3", "3")
	expect(t, s, "select id from users index by id > 3", "4 | 5")
	expect(t, s, "select id from users index by id <= 2", "2 | 1")
	// secondary index (city, age)
	expect(t, s, "select id from users index by city = 'paris'", "4 | 1 | 3") // sorted by age
	expect(t, s, "select id from users index by (city, age) > ('paris', 25) and (city, age) < ('paris', 40)", "1 | 3")
	expect(t, s, "select id from users index by city > 'london'", "4 | 1 | 3")
}

func TestSQLSecondaryIndexMaintenance(t *testing.T) {
	db := openDB(t, tmpPath(t))
	defer db.Close()
	s := db.NewSession()
	run(t, s, `create table t (id int64, name string, city string, primary key (id), index (city), index (name))`)
	run(t, s, `insert into t (id,name,city) values (1,'a','x'),(2,'b','x'),(3,'c','y')`)
	expect(t, s, "select id from t index by city = 'x'", "1 | 2")
	run(t, s, `update t set city = 'y' filter id = 1`)
	expect(t, s, "select id from t index by city = 'x'", "2")
	expect(t, s, "select id from t index by city = 'y'", "1 | 3")
	run(t, s, `delete from t index by id = 2`)
	expect(t, s, "select id from t index by city = 'x'", "")
	expect(t, s, "select id from t index by name = 'b'", "")
	run(t, s, `update t set name = 'zz' filter id = 3`)
	expect(t, s, "select id from t index by name >= 'c'", "3")
	expect(t, s, "select name from t index by name >= 'a'", "a | zz")
	// the index is not stale after delete + reinsert
	run(t, s, `delete from t`)
	expect(t, s, "select id from t index by city = 'y'", "")
	run(t, s, `insert into t (id,name,city) values (9,'q','y')`)
	expect(t, s, "select id from t index by city = 'y'", "9")
}

func TestSQLFilterLimitExpr(t *testing.T) {
	db := openDB(t, tmpPath(t))
	defer db.Close()
	s := db.NewSession()
	run(t, s, `create table n (i int64, s string, primary key (i))`)
	var vals []string
	for i := 1; i <= 50; i++ {
		vals = append(vals, fmt.Sprintf("(%d, 'row%d')", i, i))
	}
	run(t, s, "insert into n (i, s) values "+strings.Join(vals, ","))
	expect(t, s, "select i from n filter i % 10 = 0", "10 | 20 | 30 | 40 | 50")
	expect(t, s, "select i from n filter i % 10 = 0 limit 2", "10 | 20")
	expect(t, s, "select i from n filter i % 10 = 0 limit 1, 2", "20 | 30")
	expect(t, s, "select i from n limit 0", "")
	expect(t, s, "select i, s from n index by i > 47 filter not (i = 49)", "48,row48 | 50,row50")
	expect(t, s, "select i*i+1, -i, s + '!' from n index by i = 7", "50,-7,row7!")
	expect(t, s, "select i from n filter i < 3 or i > 49 or s = 'row25'", "1 | 2 | 25 | 50")
	expect(t, s, "select 1 + 2 * 3 - 4 / 2 from n limit 1", "5")
	expect(t, s, "select i from n filter i > 5 and i < 9 and s <> 'row7'", "6 | 8")
	expect(t, s, "update n set s = s + '?' filter i <= 2", "")
	expect(t, s, "select s from n limit 3", "row1? | row2? | row3")
	expect(t, s, "delete from n filter i > 3", "")
	expect(t, s, "select i from n", "1 | 2 | 3")
}

func TestSQLErrors(t *testing.T) {
	db := openDB(t, tmpPath(t))
	defer db.Close()
	s := db.NewSession()
	run(t, s, `create table t (a int64, b string, primary key (a))`)
	run(t, s, `insert into t (a,b) values (1,'x')`)
	expectErr(t, s, `insert into t (a,b) values (1,'y')`, "duplicate")
	expectErr(t, s, `insert into t (a) values (2)`, "missing column")
	expectErr(t, s, `insert into t (a,b) values ('s','y')`, "type")
	expectErr(t, s, `select * from nope`, "table not found")
	expectErr(t, s, `select zz from t`, "unknown column")
	expectErr(t, s, `select * from t index by b = 'x'`, "no index")
	expectErr(t, s, `update t set a = 5`, "primary key")
	expectErr(t, s, `select * form t`, "syntax error")
	expectErr(t, s, `select a + b from t`, "type error")
	expectErr(t, s, `select a / 0 from t`, "division by zero")
	expectErr(t, s, `create table t (a int64, primary key (a))`, "exists")
	expectErr(t, s, `create table u (a int64)`, "primary key")
	expectErr(t, s, `select 'unterminated from t`, "unterminated")
	// nothing leaked by the failed statements
	expect(t, s, "select * from t", "1,x")
}

func TestSQLTransactions(t *testing.T) {
	db := openDB(t, tmpPath(t))
	defer db.Close()
	s1, s2 := db.NewSession(), db.NewSession()
	run(t, s1, `create table acct (id int64, bal int64, primary key (id))`)
	run(t, s1, `insert into acct (id, bal) values (1, 100), (2, 100)`)

	run(t, s1, "begin")
	run(t, s1, "update acct set bal = bal - 30 filter id = 1")
	run(t, s1, "update acct set bal = bal + 30 filter id = 2")
	expect(t, s1, "select bal from acct", "70 | 130")  // sees own writes
	expect(t, s2, "select bal from acct", "100 | 100") // others don't
	run(t, s1, "commit")
	expect(t, s2, "select bal from acct", "70 | 130")

	run(t, s1, "begin")
	run(t, s1, "delete from acct")
	run(t, s1, "rollback")
	expect(t, s2, "select id from acct", "1 | 2")

	// an error rolls the explicit transaction back
	run(t, s1, "begin")
	run(t, s1, "update acct set bal = 0 filter id = 1")
	expectErr(t, s1, "insert into acct (id, bal) values (2, 5)", "rolled back")
	if s1.InTx() {
		t.Fatal("transaction should be gone")
	}
	expect(t, s2, "select bal from acct", "70 | 130")

	// write skew / lost update is detected
	run(t, s1, "begin")
	run(t, s2, "begin")
	run(t, s1, "select bal from acct index by id = 1")
	run(t, s2, "select bal from acct index by id = 1")
	run(t, s1, "update acct set bal = bal + 1 filter id = 1")
	run(t, s2, "update acct set bal = bal + 1 filter id = 1")
	run(t, s1, "commit")
	expectErr(t, s2, "commit", "conflict")
	expect(t, s1, "select bal from acct index by id = 1", "71")
}

func TestSQLPersistence(t *testing.T) {
	path := tmpPath(t)
	db := openDB(t, path)
	s := db.NewSession()
	run(t, s, `create table a (k string, v int64, primary key (k), index (v))`)
	run(t, s, `create table b (x int64, primary key (x))`)
	for i := 0; i < 300; i++ {
		run(t, s, fmt.Sprintf("insert into a (k, v) values ('key%03d', %d)", i, i%10))
	}
	run(t, s, "insert into b (x) values (1), (2)")
	db.Close()

	db = openDB(t, path)
	defer db.Close()
	s = db.NewSession()
	expect(t, s, "show tables", "a | b")
	expect(t, s, "select k from a index by v = 3 limit 3", "key003 | key013 | key023")
	expect(t, s, "select x from b", "1 | 2")
	run(t, s, "create table c (z int64, primary key (z))") // prefix counter survived
	run(t, s, "insert into c (z) values (5)")
	expect(t, s, "select * from c", "5")
	expect(t, s, "select x from b", "1 | 2")
}

// random operations on a table vs a Go map
func TestSQLRandomAgainstReference(t *testing.T) {
	db := openDB(t, tmpPath(t))
	defer db.Close()
	s := db.NewSession()
	run(t, s, `create table r (id int64, grp int64, txt string, primary key (id), index (grp, txt))`)
	type row struct {
		grp int64
		txt string
	}
	ref := map[int64]row{}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 1500; i++ {
		id := int64(rng.Intn(200) - 100) // negative ids too
		switch rng.Intn(3) {
		case 0, 1:
			r := row{int64(rng.Intn(5)), fmt.Sprintf("t\x00%d\x01", rng.Intn(20))} // awkward bytes
			tx := &DBTX{}
			db.Begin(tx)
			rec := Record{}
			rec.AddInt64("id", id).AddInt64("grp", r.grp).AddStr("txt", []byte(r.txt))
			if _, err := tx.Upsert("r", rec); err != nil {
				t.Fatal(err)
			}
			if err := db.Commit(tx); err != nil {
				t.Fatal(err)
			}
			ref[id] = r
		default:
			run(t, s, fmt.Sprintf("delete from r index by id = %d", id))
			delete(ref, id)
		}
	}
	// compare full scan, by primary key order
	res, _ := s.Exec("select id, grp, txt from r")
	got := res[0].Rows
	if len(got) != len(ref) {
		t.Fatalf("%d rows, want %d", len(got), len(ref))
	}
	var prev int64 = -1 << 62
	for _, r := range got {
		id := r[0].I64
		if id <= prev {
			t.Fatalf("not sorted: %d after %d", id, prev)
		}
		prev = id
		w, ok := ref[id]
		if !ok || w.grp != r[1].I64 || w.txt != string(r[2].Str) {
			t.Fatalf("row %d mismatch: %v vs %v", id, r, w)
		}
	}
	// secondary index returns the same rows per group
	for g := int64(0); g < 5; g++ {
		res, err := s.Exec(fmt.Sprintf("select id from r index by grp = %d", g))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, w := range ref {
			if w.grp == g {
				n++
			}
		}
		if len(res[0].Rows) != n {
			t.Fatalf("group %d: %d rows, want %d", g, len(res[0].Rows), n)
		}
	}
}

func TestParseMultiStatement(t *testing.T) {
	stmts, err := QLParseAll("begin; select 1 from t; ; commit;")
	if err != nil || len(stmts) != 3 {
		t.Fatalf("%v %v", stmts, err)
	}
}
