package minidb

import (
	"errors"
	"fmt"
)

// Errors returned by the public API.
var (
	ErrConflict = errors.New("minidb: transaction conflict")
	ErrTxDone   = errors.New("minidb: transaction already finished")
	ErrKeySize  = errors.New("minidb: key is empty or too large")
	ErrValSize  = errors.New("minidb: value is too large")
)

func assert(cond bool) {
	if !cond {
		panic("minidb: assertion failure")
	}
}

// ioPanic is used to unwind from deep inside B+tree code (which has no error
// returns) when a disk read fails. Public entry points recover it.
type ioPanic struct{ err error }

func catchIO(errp *error) {
	if r := recover(); r != nil {
		if e, ok := r.(ioPanic); ok {
			*errp = fmt.Errorf("minidb: io error: %w", e.err)
			return
		}
		panic(r)
	}
}
