package minidb

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openKV(t testing.TB, path string) *KV {
	t.Helper()
	kv := &KV{Path: path}
	if err := kv.Open(); err != nil {
		t.Fatal(err)
	}
	return kv
}

func tmpPath(t testing.TB) string {
	return filepath.Join(t.TempDir(), "test.db")
}

func TestKVPersistence(t *testing.T) {
	path := tmpPath(t)
	kv := openKV(t, path)
	ref := map[string]string{}
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 3000; i++ {
		k := fmt.Sprintf("key-%04d", rng.Intn(800))
		if rng.Intn(4) == 0 {
			if _, err := kv.Del([]byte(k)); err != nil {
				t.Fatal(err)
			}
			delete(ref, k)
		} else {
			v := fmt.Sprintf("value-%d", i)
			if err := kv.Set([]byte(k), []byte(v)); err != nil {
				t.Fatal(err)
			}
			ref[k] = v
		}
	}
	check := func(kv *KV) {
		t.Helper()
		for k, v := range ref {
			got, ok, err := kv.Get([]byte(k))
			if err != nil || !ok || string(got) != v {
				t.Fatalf("get %s: %q %v %v, want %q", k, got, ok, err, v)
			}
		}
		n := 0
		kv.View(func(tx *KVTX) error {
			for it, _ := tx.Seek([]byte{0}, CmpGE); it.Valid(); it.Next() {
				n++
			}
			return nil
		})
		if n != len(ref) {
			t.Fatalf("scan found %d keys, want %d", n, len(ref))
		}
	}
	check(kv)
	kv.Close()
	kv = openKV(t, path) // reopen
	defer kv.Close()
	check(kv)
}

// pages are reused: the file must not grow without bound
func TestKVFreeListReuse(t *testing.T) {
	path := tmpPath(t)
	kv := openKV(t, path)
	defer kv.Close()
	for i := 0; i < 200; i++ {
		kv.Set([]byte(fmt.Sprintf("k%03d", i)), bytes.Repeat([]byte("x"), 500))
	}
	size := func() int64 { st, _ := os.Stat(path); return st.Size() }
	for round := 0; round < 3; round++ {
		for i := 0; i < 200; i++ {
			if err := kv.Set([]byte(fmt.Sprintf("k%03d", i)), bytes.Repeat([]byte{byte('a' + round)}, 500)); err != nil {
				t.Fatal(err)
			}
		}
	}
	s1 := size()
	for round := 0; round < 20; round++ {
		for i := 0; i < 200; i++ {
			kv.Set([]byte(fmt.Sprintf("k%03d", i)), bytes.Repeat([]byte{byte('A' + round)}, 500))
		}
	}
	s2 := size()
	if s2 > s1+s1/4 {
		t.Fatalf("file keeps growing: %d -> %d", s1, s2)
	}
	t.Logf("file size stable: %d -> %d bytes", s1, s2)
}

func TestKVBigTransaction(t *testing.T) {
	path := tmpPath(t)
	kv := openKV(t, path)
	err := kv.Update(func(tx *KVTX) error {
		for i := 0; i < 20000; i++ {
			if err := tx.Set([]byte(fmt.Sprintf("k%06d", i)), []byte(fmt.Sprint(i))); err != nil {
				return err
			}
		}
		// read back own writes + delete
		v, ok, _ := tx.Get([]byte("k000042"))
		if !ok || string(v) != "42" {
			return fmt.Errorf("read your own write failed")
		}
		for i := 0; i < 20000; i += 2 {
			if _, err := tx.Delete([]byte(fmt.Sprintf("k%06d", i))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	t.Logf("20000 inserts + 10000 deletes in 1 tx: file %d KB", st.Size()/1024)
	if st.Size() > 64<<20 {
		t.Fatalf("file too big: %d", st.Size())
	}
	kv.Close()
	kv = openKV(t, path)
	defer kv.Close()
	n := 0
	kv.View(func(tx *KVTX) error {
		for it, _ := tx.Seek([]byte{0}, CmpGE); it.Valid(); it.Next() {
			k, _ := it.Deref()
			var i int
			fmt.Sscanf(string(k), "k%d", &i)
			if i%2 == 0 {
				t.Fatalf("deleted key %s is back", k)
			}
			n++
		}
		return nil
	})
	if n != 10000 {
		t.Fatalf("got %d keys", n)
	}
}

func TestKVSnapshotIsolation(t *testing.T) {
	kv := openKV(t, tmpPath(t))
	defer kv.Close()
	kv.Set([]byte("a"), []byte("1"))

	var reader KVTX
	kv.Begin(&reader) // snapshot taken here
	defer kv.Abort(&reader)

	kv.Set([]byte("a"), []byte("2"))
	kv.Set([]byte("b"), []byte("x"))
	for i := 0; i < 500; i++ { // churn pages so reuse would be tempting
		kv.Set([]byte(fmt.Sprintf("churn%d", i%7)), []byte(fmt.Sprint(i)))
	}

	v, ok, _ := reader.Get([]byte("a"))
	if !ok || string(v) != "1" {
		t.Fatalf("snapshot saw %q", v)
	}
	if _, ok, _ := reader.Get([]byte("b")); ok {
		t.Fatal("snapshot saw a later insert")
	}
	n := 0
	for it, _ := reader.Seek([]byte{0}, CmpGE); it.Valid(); it.Next() {
		n++
	}
	if n != 1 {
		t.Fatalf("snapshot scan found %d keys", n)
	}
	v, _, _ = kv.Get([]byte("a"))
	if string(v) != "2" {
		t.Fatalf("latest saw %q", v)
	}
}

func TestKVConflicts(t *testing.T) {
	kv := openKV(t, tmpPath(t))
	defer kv.Close()
	kv.Set([]byte("a"), []byte("1"))
	kv.Set([]byte("b"), []byte("1"))

	var t1, t2 KVTX
	kv.Begin(&t1)
	kv.Begin(&t2)
	v, _, _ := t1.Get([]byte("a")) // t1 depends on a
	t1.Set([]byte("b"), append(v, '!'))
	t2.Set([]byte("a"), []byte("changed")) // t2 changes a
	if err := kv.Commit(&t2); err != nil {
		t.Fatal(err)
	}
	if err := kv.Commit(&t1); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if v, _, _ := kv.Get([]byte("b")); string(v) != "1" {
		t.Fatalf("conflicting tx leaked a write: %q", v)
	}

	// disjoint transactions both succeed
	kv.Begin(&t1)
	kv.Begin(&t2)
	t1.Set([]byte("x"), []byte("1"))
	t2.Set([]byte("y"), []byte("2"))
	if err := kv.Commit(&t1); err != nil {
		t.Fatal(err)
	}
	if err := kv.Commit(&t2); err != nil {
		t.Fatal(err)
	}

	// phantom: a range read conflicts with an insert into the range
	kv.Begin(&t1)
	kv.Begin(&t2)
	for it, _ := t1.Seek([]byte("a"), CmpGE); it.Valid(); it.Next() {
	}
	t1.Set([]byte("z"), []byte("out"))
	t1.AddRead([]byte("a"), []byte("zzzz")) // the range this scan covered
	t2.Set([]byte("m"), []byte("phantom"))
	if err := kv.Commit(&t2); err != nil {
		t.Fatal(err)
	}
	if err := kv.Commit(&t1); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

// a bank: concurrent transfers must keep the total constant
func TestKVConcurrentTransfers(t *testing.T) {
	kv := openKV(t, tmpPath(t))
	defer kv.Close()
	const accounts, initial = 10, 1000
	kv.Update(func(tx *KVTX) error {
		for i := 0; i < accounts; i++ {
			tx.Set([]byte(fmt.Sprintf("acct%d", i)), []byte(fmt.Sprint(initial)))
		}
		return nil
	})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < 60; i++ {
				from, to := rng.Intn(accounts), rng.Intn(accounts)
				if from == to {
					continue
				}
				err := kv.Update(func(tx *KVTX) error {
					fk, tk := []byte(fmt.Sprintf("acct%d", from)), []byte(fmt.Sprintf("acct%d", to))
					fv, _, _ := tx.Get(fk)
					tv, _, _ := tx.Get(tk)
					var f, tt int
					fmt.Sscan(string(fv), &f)
					fmt.Sscan(string(tv), &tt)
					tx.Set(fk, []byte(fmt.Sprint(f-1)))
					tx.Set(tk, []byte(fmt.Sprint(tt+1)))
					return nil
				})
				if err != nil && !errors.Is(err, ErrConflict) {
					t.Error(err)
				}
			}
		}(int64(w))
	}
	// concurrent readers always see a consistent total
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				kv.View(func(tx *KVTX) error {
					sum := 0
					for it, _ := tx.Seek([]byte("acct"), CmpGE); it.Valid(); it.Next() {
						_, v := it.Deref()
						var n int
						fmt.Sscan(string(v), &n)
						sum += n
					}
					if sum != accounts*initial {
						t.Errorf("reader saw total %d", sum)
					}
					return nil
				})
			}
		}()
	}
	wg.Wait()
	sum := 0
	kv.View(func(tx *KVTX) error {
		for it, _ := tx.Seek([]byte("acct"), CmpGE); it.Valid(); it.Next() {
			_, v := it.Deref()
			var n int
			fmt.Sscan(string(v), &n)
			sum += n
		}
		return nil
	})
	if sum != accounts*initial {
		t.Fatalf("total is %d", sum)
	}
}

// a torn commit (pages written, meta page not) must leave the old version
func TestKVCrashBeforeMeta(t *testing.T) {
	path := tmpPath(t)
	kv := openKV(t, path)
	for i := 0; i < 100; i++ {
		kv.Set([]byte(fmt.Sprintf("k%03d", i)), []byte("old"))
	}
	meta := saveMeta(kv)
	kv.Close()

	// simulate: the next commit wrote garbage pages at the end and in free
	// pages, but crashed before the meta page flip.
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	st, _ := f.Stat()
	f.WriteAt(bytes.Repeat([]byte{0xAB}, 5*pageSize), st.Size())
	f.Close()

	kv = openKV(t, path)
	defer kv.Close()
	if !bytes.Equal(saveMeta(kv), meta) {
		t.Fatal("meta changed")
	}
	for i := 0; i < 100; i++ {
		v, ok, _ := kv.Get([]byte(fmt.Sprintf("k%03d", i)))
		if !ok || string(v) != "old" {
			t.Fatalf("k%03d = %q", i, v)
		}
	}
	if err := kv.Set([]byte("after"), []byte("crash")); err != nil {
		t.Fatal(err)
	}
}

func TestKVBadFile(t *testing.T) {
	path := tmpPath(t)
	os.WriteFile(path, bytes.Repeat([]byte("not a database"), 400), 0o644)
	kv := &KV{Path: path}
	if err := kv.Open(); err == nil {
		t.Fatal("opened a garbage file")
	}
}
