// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package fibonacci provides the Fibonacci sequence encoded as JSON.
package fibonacci

import (
	"fmt"
	"io"
	"math/big"
)

// MaxLen is the length of the longest table NewTable builds.
//
// The size of a table grows with the square of its length: at MaxLen it takes about 42 MB,
// and so does a single response which returns all of it.
const MaxLen = 20_000

var closingBracket = []byte{']'}

// Table holds the first Len Fibonacci numbers, starting from 0, encoded as a JSON array.
//
// Without the closing bracket, the array of the first n numbers is a prefix of the array of
// the first n+1 numbers. So the table keeps a single array without the closing bracket, and
// the offset at which each of the shorter arrays ends in it.
//
// The table is not modified after NewTable returns, so it is safe for concurrent use.
type Table struct {
	// data is "[0,1,1,2,3,5" and so on.
	data []byte
	// ends[n] is the length of the array of the first n numbers in data.
	ends []int
}

// NewTable builds the table of the first n Fibonacci numbers.
func NewTable(n int) (*Table, error) {
	if n < 0 || n > MaxLen {
		return nil, fmt.Errorf("table length must be between 0 and %d, got %d", MaxLen, n)
	}

	t := &Table{
		// The k-th number has about 0.209*k digits (log10 of the golden ratio per number),
		// so n numbers take about 0.1045*n*n bytes, which n*n/9 covers.
		// This is only a hint to allocate once, append grows the slice if it is ever off.
		data: make([]byte, 0, n*n/9+2*n+16),
		ends: make([]int, 0, n+1),
	}

	t.data = append(t.data, '[')
	t.ends = append(t.ends, len(t.data))

	a, b := new(big.Int), big.NewInt(1)

	for i := range n {
		if i > 0 {
			t.data = append(t.data, ',')
		}

		t.data = a.Append(t.data, 10)
		t.ends = append(t.ends, len(t.data))

		// (a, b) becomes (b, a+b), the sum reuses the memory of a
		a.Add(a, b)
		a, b = b, a
	}

	return t, nil
}

// Len returns the number of Fibonacci numbers in the table.
func (t *Table) Len() int {
	return len(t.ends) - 1
}

// JSONLen returns the size in bytes of the JSON array of the first n numbers.
//
// It panics if n is not between 0 and Len.
func (t *Table) JSONLen(n int) int {
	return t.ends[n] + len(closingBracket)
}

// WriteJSON writes the first n Fibonacci numbers to w as a JSON array.
//
// It panics if n is not between 0 and Len.
func (t *Table) WriteJSON(w io.Writer, n int) error {
	if _, err := w.Write(t.data[:t.ends[n]]); err != nil {
		return err
	}

	_, err := w.Write(closingBracket)

	return err
}
