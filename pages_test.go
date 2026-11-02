package minidb

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

// every page is either in the tree, a free list node, or a free list item
func accountPages(kv *KV) (tree, nodes, items, total int, dup bool) {
	seen := map[uint64]string{}
	mark := func(p uint64, what string) {
		if _, ok := seen[p]; ok {
			dup = true
			fmt.Println("DUP", p, what, seen[p])
		}
		seen[p] = what
	}
	var walk func(ptr uint64)
	walk = func(ptr uint64) {
		mark(ptr, "tree")
		tree++
		n := bnode(kv.readPageFile(ptr))
		if n.btype() == nodeInternal {
			for i := uint16(0); i < n.nkeys(); i++ {
				walk(n.getPtr(i))
			}
		}
	}
	if kv.tree.root != 0 {
		walk(kv.tree.root)
	}
	fl := kv.free
	// walk list nodes
	page := fl.headPage
	seq := fl.headSeq
	for {
		node := lnode(kv.readPageFile(page))
		mark(page, "listnode")
		nodes++
		for {
			if seq == fl.tailSeq {
				break
			}
			p, _ := node.getItem(seq2idx(seq))
			mark(p, "item")
			items++
			seq++
			if seq2idx(seq) == 0 {
				break
			}
		}
		if page == fl.tailPage {
			break
		}
		page = node.getNext()
	}
	total = int(kv.page.flushed) - 1
	return
}

func TestKVNoPageLeaks(t *testing.T) {
	path := tmpPath(t)
	kv := openKV(t, path)
	defer kv.Close()
	for i := 0; i < 200; i++ {
		kv.Set([]byte(fmt.Sprintf("k%03d", i)), bytes.Repeat([]byte("x"), 500))
	}
	for round := 0; round < 10; round++ {
		for i := 0; i < 200; i++ {
			kv.Set([]byte(fmt.Sprintf("k%03d", i)), bytes.Repeat([]byte{byte('A' + round)}, 500))
			if i%3 == 0 {
				kv.Del([]byte(fmt.Sprintf("k%03d", (i*7)%200)))
			}
		}
		tr, no, it, total, dup := accountPages(kv)
		if dup || tr+no+it != total {
			t.Fatalf("round %d: tree=%d listnodes=%d items=%d total=%d dup=%v", round, tr, no, it, total, dup)
		}
	}
	_ = os.Remove
}
