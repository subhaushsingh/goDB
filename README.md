# minidb

A transactional relational database in ~2,500 lines of Go, built from the
"Build Your Own Database From Scratch in Go" design: copy-on-write B+tree →
durable KV with free list → tables + secondary indexes → snapshot-isolation
transactions → SQL-like parser and executor. No dependencies, no mmap/syscall
(builds on Windows, Linux, macOS; Go 1.22+).

## Run

    go test -race ./...
    go run ./cmd/minidb -db my.db            # shell
    go run ./cmd/minidb -db my.db -f x.sql   # script

## Layout

    util.go        errors, assert
    btree.go       node format, split/merge, insert/delete, iterator (copy-on-write)
    freelist.go    on-disk unrolled linked list of reusable pages (versioned)
    kv.go          pager, meta page, 2-phase fsync commit, KVTX (snapshot isolation,
                   optimistic conflict detection), merged iterator
    table.go       schemas, order-preserving key encoding, CRUD, secondary indexes, Scanner
    ql_parse.go    recursive-descent parser (statements + expressions)
    ql_exec.go     expression eval, iterator chain, statements, Session
    cmd/minidb     REPL

## Language

    create table t (a int64, b string, index (b), primary key (a));
    insert into t (a, b) values (1, 'x'), (2, 'y');
    select a, b+'!' as c from t index by a >= 1 and a < 5 filter a <> 3 limit 1, 10;
    select * from t index by a < 5 and a > 1;     -- descending
    select * from t index by (b, a) > ('x', 0);
    update t set b = 'z' filter a = 1;   delete from t index by a = 2;
    begin; ...; commit; | rollback;      show tables;   describe t;

`INDEX BY` picks the index and range explicitly (primary key by default);
`FILTER` is an arbitrary predicate; `LIMIT [offset,] n` applies after the filter.

## Go API

    db := &minidb.DB{Path: "x.db"}; db.Open(); defer db.Close()
    db.Update(func(tx *minidb.DBTX) error { _, err := tx.Insert("t", rec); return err })
    db.NewSession().Exec("select * from t;")

## Limits

Single process (no file lock). Keys ≤ 1000 B, values ≤ 3000 B. Writers are
serialized at commit; conflicting transactions get `ErrConflict` (`Update`
retries automatically). Primary-key columns can't be updated; no DROP/ALTER.
