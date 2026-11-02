package minidb

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// in-memory pages, so the B+tree can be tested without the rest of the DB
type testTree struct {
	tree  btree
	ref   map[string]string
	pages map[uint64]bnode
	next  uint64
}

func newTestTree() *testTree {
	c := &testTree{ref: map[string]string{}, pages: map[uint64]bnode{}, next: 1}
	c.tree = btree{
		get: func(ptr uint64) []byte {
			node, ok := c.pages[ptr]
			assert(ok)
			return node
		},
		new: func(node []byte) uint64 {
			assert(bnode(node).nbytes() <= pageSize)
			ptr := c.next
			c.next++
			c.pages[ptr] = node
			return ptr
		},
		del: func(ptr uint64) {
			_, ok := c.pages[ptr]
			assert(ok)
			delete(c.pages, ptr)
		},
	}
	return c
}

func (c *testTree) add(key, val string) {
	c.tree.Insert([]byte(key), []byte(val))
	c.ref[key] = val
}

func (c *testTree) del(key string) bool {
	_, want := c.ref[key]
	got := c.tree.Delete(&DeleteReq{Key: []byte(key)})
	if got != want {
		panic(fmt.Sprintf("delete %q: got %v want %v", key, got, want))
	}
	delete(c.ref, key)
	return got
}

// the structure is valid and the data matches the reference
func (c *testTree) verify(t *testing.T) {
	t.Helper()
	// walk the tree: sorted keys, size limits, uniform height, reachability
	reachable := 0
	var leafKeys []string
	height := -1
	var walk func(ptr uint64, depth int, lo []byte)
	walk = func(ptr uint64, depth int, lo []byte) {
		node := bnode(c.tree.get(ptr))
		reachable++
		if node.nbytes() > pageSize {
			t.Fatalf("node too big: %d", node.nbytes())
		}
		if node.nkeys() == 0 {
			t.Fatalf("empty node")
		}
		if !bytes.Equal(node.getKey(0), lo) && lo != nil {
			t.Fatalf("first key %q != separator %q", node.getKey(0), lo)
		}
		for i := uint16(1); i < node.nkeys(); i++ {
			if bytes.Compare(node.getKey(i-1), node.getKey(i)) >= 0 {
				t.Fatalf("keys not sorted")
			}
		}
		if node.btype() == nodeLeaf {
			if height == -1 {
				height = depth
			} else if height != depth {
				t.Fatalf("leaves at different heights")
			}
			for i := uint16(0); i < node.nkeys(); i++ {
				if len(node.getKey(i)) > 0 {
					leafKeys = append(leafKeys, string(node.getKey(i)))
					if c.ref[string(node.getKey(i))] != string(node.getVal(i)) {
						t.Fatalf("value mismatch for %q", node.getKey(i))
					}
				}
			}
			return
		}
		for i := uint16(0); i < node.nkeys(); i++ {
			walk(node.getPtr(i), depth+1, node.getKey(i))
		}
	}
	if c.tree.root != 0 {
		walk(c.tree.root, 0, nil)
	}
	if reachable != len(c.pages) {
		t.Fatalf("page leak: %d reachable vs %d allocated", reachable, len(c.pages))
	}
	if !sort.StringsAreSorted(leafKeys) || len(leafKeys) != len(c.ref) {
		t.Fatalf("leaf keys: %d vs ref %d", len(leafKeys), len(c.ref))
	}
}

func TestBTreeRandom(t *testing.T) {
	c := newTestTree()
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		key := fmt.Sprintf("key%05d", rng.Intn(3000))
		switch rng.Intn(3) {
		case 0, 1:
			c.add(key, fmt.Sprintf("val-%d-%s", i, bytes.Repeat([]byte("x"), rng.Intn(200))))
		default:
			c.del(key)
		}
		if i%2000 == 0 {
			c.verify(t)
		}
	}
	c.verify(t)
	// delete everything
	for k := range c.ref {
		c.del(k)
	}
	c.verify(t)
}

func TestBTreeBigValues(t *testing.T) {
	c := newTestTree()
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 500; i++ {
		k := string(bytes.Repeat([]byte{byte('a' + rng.Intn(26))}, 1+rng.Intn(maxKeySize))) + fmt.Sprint(i)
		if len(k) > maxKeySize {
			k = k[:maxKeySize]
		}
		c.add(k, string(bytes.Repeat([]byte("v"), rng.Intn(maxValSize+1))))
	}
	c.verify(t)
	keys := make([]string, 0)
	for k := range c.ref {
		keys = append(keys, k)
	}
	for _, k := range keys {
		c.del(k)
	}
	c.verify(t)
}

func TestBTreeIterator(t *testing.T) {
	c := newTestTree()
	var keys []string
	for i := 0; i < 5000; i += 2 { // even numbers only
		k := fmt.Sprintf("k%05d", i)
		c.add(k, "v")
		keys = append(keys, k)
	}
	// full forward scan from the first key
	i := 0
	for it := c.tree.first(); it.Valid(); it.Next() {
		k, _ := it.Deref()
		if string(k) != keys[i] {
			t.Fatalf("scan: got %s want %s", k, keys[i])
		}
		i++
	}
	if i != len(keys) {
		t.Fatalf("scan: %d of %d", i, len(keys))
	}
	// seeks in all 4 flavors, present and absent keys, in both directions
	for n := -1; n < 5003; n += 1 {
		probe := fmt.Sprintf("k%05d", n)
		if n < 0 {
			probe = "a"
		}
		lo := sort.SearchStrings(keys, probe)
		cases := []struct {
			cmp  int
			want int // index into keys, or -1 / len(keys) for none
		}{
			{CmpGE, lo},
			{CmpGT, lo},
			{CmpLE, lo - 1},
			{CmpLT, lo - 1},
		}
		exact := lo < len(keys) && keys[lo] == probe
		cases[1].want = lo
		if exact {
			cases[1].want = lo + 1
			cases[2].want = lo
		}
		for _, cs := range cases {
			it := c.tree.Seek([]byte(probe), cs.cmp)
			if cs.want < 0 || cs.want >= len(keys) {
				if it.Valid() {
					k, _ := it.Deref()
					t.Fatalf("seek %s cmp %d: want invalid, got %s", probe, cs.cmp, k)
				}
				continue
			}
			if !it.Valid() {
				t.Fatalf("seek %s cmp %d: want %s, got invalid", probe, cs.cmp, keys[cs.want])
			}
			k, _ := it.Deref()
			if string(k) != keys[cs.want] {
				t.Fatalf("seek %s cmp %d: got %s want %s", probe, cs.cmp, k, keys[cs.want])
			}
		}
	}
	// backward scan
	i = len(keys) - 1
	for it := c.tree.Seek([]byte("z"), CmpLE); it.Valid(); it.Prev() {
		k, _ := it.Deref()
		if string(k) != keys[i] {
			t.Fatalf("backward: got %s want %s", k, keys[i])
		}
		i--
	}
	if i != -1 {
		t.Fatalf("backward scan stopped at %d", i)
	}
}
