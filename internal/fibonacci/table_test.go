// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package fibonacci_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"math/bits"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aozmekik/fibonacci-service/internal/fibonacci"
)

func TestNewTable(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 1000} {
		table, err := fibonacci.NewTable(n)
		require.NoError(t, err)

		assert.Equal(t, n, table.Len())
	}

	for _, n := range []int{-1, fibonacci.MaxLen + 1} {
		_, err := fibonacci.NewTable(n)
		require.Error(t, err)
	}
}

func TestTableJSON(t *testing.T) {
	t.Parallel()

	table, err := fibonacci.NewTable(10)
	require.NoError(t, err)

	for _, test := range []struct {
		expected string
		n        int
	}{
		{n: 0, expected: "[]"},
		{n: 1, expected: "[0]"},
		{n: 2, expected: "[0,1]"},
		{n: 5, expected: "[0,1,1,2,3]"},
		{n: 10, expected: "[0,1,1,2,3,5,8,13,21,34]"},
	} {
		assert.Equal(t, test.expected, encode(t, table, test.n))
		assert.Equal(t, len(test.expected), table.JSONLen(test.n))
	}

	// out of range is a bug of the caller, the same way as with a slice
	assert.Panics(t, func() { table.JSONLen(-1) })
	assert.Panics(t, func() { table.JSONLen(11) })
	assert.Panics(t, func() { table.WriteJSON(io.Discard, 11) }) //nolint:errcheck
}

// TestTableValues checks the numbers against the values published as https://oeis.org/A000045
// and against a second algorithm.
//
// The table is long enough to go past the 64 bit integers, which end at F(93).
func TestTableValues(t *testing.T) {
	t.Parallel()

	table, err := fibonacci.NewTable(2000)
	require.NoError(t, err)

	var values []*big.Int

	require.NoError(t, json.Unmarshal([]byte(encode(t, table, table.Len())), &values))
	require.Len(t, values, table.Len())

	for i, expected := range map[int]string{
		0:   "0",
		1:   "1",
		2:   "1",
		3:   "2",
		10:  "55",
		20:  "6765",
		92:  "7540113804746346429",
		93:  "12200160415121876738",
		94:  "19740274219868223167",
		100: "354224848179261915075",
		300: "222232244629420445529739893461909967206666939096499764990979600",
	} {
		assert.Equal(t, expected, values[i].String(), "F(%d)", i)
	}

	for i, value := range values {
		require.Zero(t, value.Cmp(fib(i)), "F(%d)", i)
	}
}

// TestTablePrefixes checks that every n gets the first n numbers of the longest array.
func TestTablePrefixes(t *testing.T) {
	t.Parallel()

	table, err := fibonacci.NewTable(300)
	require.NoError(t, err)

	full := encode(t, table, table.Len())

	for n := range table.Len() + 1 {
		encoded := encode(t, table, n)

		require.Equal(t, table.JSONLen(n), len(encoded), "n = %d", n)
		require.Equal(t, full[:len(encoded)-1]+"]", encoded, "n = %d", n)

		var values []json.Number

		require.NoError(t, json.Unmarshal([]byte(encoded), &values), "n = %d", n)
		require.Len(t, values, n)
	}
}

// TestTableWriteError checks that a failure of either of the two writes is returned.
func TestTableWriteError(t *testing.T) {
	t.Parallel()

	table, err := fibonacci.NewTable(10)
	require.NoError(t, err)

	for failing := range 2 {
		assert.ErrorIs(t, table.WriteJSON(&failingWriter{failing: failing}, 10), errWrite, "write %d", failing)
	}
}

// TestTableSize pins the sizes the documentation and the limits are based on.
func TestTableSize(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		n        int
		expected int
	}{
		{n: 1000, expected: 105_543},
		{n: 10_000, expected: 10_459_846},
		{n: fibonacci.MaxLen, expected: 41_818_451},
	} {
		table, err := fibonacci.NewTable(test.n)
		require.NoError(t, err)

		assert.Equal(t, test.expected, table.JSONLen(test.n), "n = %d", test.n)
	}
}

func BenchmarkNewTable(b *testing.B) {
	for _, n := range []int{1000, 10_000, fibonacci.MaxLen} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				if _, err := fibonacci.NewTable(n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func encode(t *testing.T, table *fibonacci.Table, n int) string {
	t.Helper()

	var buf bytes.Buffer

	require.NoError(t, table.WriteJSON(&buf, n))

	return buf.String()
}

var errWrite = errors.New("write failed")

// failingWriter is a writer which fails the write with the given index, counted from 0, and no other.
//
// A writer which went on failing would hide an error of the first write that is dropped,
// as the second write would fail in its place.
type failingWriter struct {
	failing int
	writes  int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	index := w.writes

	w.writes++

	if index == w.failing {
		return 0, errWrite
	}

	return len(p), nil
}

// fib returns the n-th Fibonacci number.
//
// It uses fast doubling, which has nothing in common with the additions the table is built with:
//
//	F(2k)   = F(k) * (2*F(k+1) - F(k))
//	F(2k+1) = F(k)^2 + F(k+1)^2
func fib(n int) *big.Int {
	// a and b are F(k) and F(k+1), where k is made of the bits of n seen so far
	a, b := big.NewInt(0), big.NewInt(1)

	for i := bits.Len(uint(n)) - 1; i >= 0; i-- {
		c := new(big.Int).Lsh(b, 1)
		c.Sub(c, a).Mul(c, a)

		d := new(big.Int).Mul(a, a)
		d.Add(d, new(big.Int).Mul(b, b))

		a, b = c, d

		if n>>i&1 == 1 {
			a, b = d, c.Add(c, d)
		}
	}

	return a
}
