// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package api_test

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aozmekik/fibonacci-service/internal/api"
	"github.com/aozmekik/fibonacci-service/internal/fibonacci"
)

// limit is the length of the table the tests run with.
const limit = 100

func TestFibonacci(t *testing.T) {
	t.Parallel()

	handler := newHandler(t)

	for _, test := range []struct {
		query    string
		expected string
	}{
		{query: "n=0", expected: "[]"},
		{query: "n=1", expected: "[0]"},
		{query: "n=5", expected: "[0,1,1,2,3]"},
		{query: "n=10", expected: "[0,1,1,2,3,5,8,13,21,34]"},
	} {
		t.Run(test.query, func(t *testing.T) {
			t.Parallel()

			response := do(t, handler, http.MethodGet, "/v1/fibonacci?"+test.query)

			assert.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, test.expected, response.Body.String())
			assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
			assert.Equal(t, strconv.Itoa(len(test.expected)), response.Header().Get("Content-Length"))
			assert.Equal(t, "public, max-age=86400, immutable", response.Header().Get("Cache-Control"))
			assert.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
		})
	}

	t.Run("limit", func(t *testing.T) {
		t.Parallel()

		response := do(t, handler, http.MethodGet, "/v1/fibonacci?n="+strconv.Itoa(limit))

		require.Equal(t, http.StatusOK, response.Code)

		var values []json.Number

		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &values))
		assert.Len(t, values, limit)
	})
}

// TestHead checks that HEAD gets the headers of GET, and that no body is written for it.
//
// A server drops such a body anyway, but whatever counts the bytes written does not know that.
func TestHead(t *testing.T) {
	t.Parallel()

	handler := newHandler(t)

	for _, target := range []string{"/v1/fibonacci?n=5", "/v1/fibonacci?n=-1", "/v1/fibonacci?n=101", "/"} {
		get := do(t, handler, http.MethodGet, target)
		head := do(t, handler, http.MethodHead, target)

		assert.Equal(t, get.Code, head.Code, target)
		assert.Equal(t, get.Header(), head.Header(), target)
		assert.Equal(t, strconv.Itoa(get.Body.Len()), head.Header().Get("Content-Length"), target)
		assert.Zero(t, head.Body.Len(), target)
	}
}

func TestFibonacciErrors(t *testing.T) {
	t.Parallel()

	handler := newHandler(t)

	// the messages there are, each of which has to be true of the query it is given for
	const (
		missing   = "n is required"
		notAlone  = "n must be the only parameter, and given once"
		negative  = "n must not be negative"
		malformed = "n must be a decimal integer without a sign or leading zeros"
		tooLarge  = "n must not be greater than 20000"
		overLimit = "n must not be greater than 100"
	)

	for _, test := range []struct {
		name     string
		query    string
		expected string
		message  string
	}{
		{name: "missing", query: "", expected: "invalid_argument", message: missing},
		{name: "empty", query: "n=", expected: "invalid_argument", message: malformed},
		{name: "name only", query: "n", expected: "invalid_argument", message: malformed},
		{name: "not a number", query: "n=abc", expected: "invalid_argument", message: malformed},
		{name: "fraction", query: "n=1.5", expected: "invalid_argument", message: malformed},
		{name: "exponent", query: "n=1e3", expected: "invalid_argument", message: malformed},
		{name: "hexadecimal", query: "n=0x10", expected: "invalid_argument", message: malformed},
		{name: "space", query: "n=%205", expected: "invalid_argument", message: malformed},
		{name: "plus sign", query: "n=+5", expected: "invalid_argument", message: malformed},
		{name: "leading zero", query: "n=05", expected: "invalid_argument", message: malformed},
		{name: "minus sign only", query: "n=-", expected: "invalid_argument", message: malformed},
		{name: "negative zero", query: "n=-0", expected: "invalid_argument", message: malformed},
		{name: "negative with a leading zero", query: "n=-05", expected: "invalid_argument", message: malformed},
		{name: "given twice", query: "n=1&n=2", expected: "invalid_argument", message: notAlone},
		{name: "unknown parameter", query: "n=1&m=2", expected: "invalid_argument", message: notAlone},
		{name: "unknown parameter only", query: "m=2", expected: "invalid_argument", message: notAlone},
		{name: "malformed query", query: "n=%zz", expected: "invalid_argument", message: malformed},
		{name: "negative", query: "n=-1", expected: "invalid_argument", message: negative},
		{name: "negative beyond 64 bits", query: "n=-99999999999999999999", expected: "invalid_argument", message: negative},
		{name: "above the limit", query: "n=101", expected: "out_of_range", message: overLimit},
		{name: "highest limit", query: "n=" + strconv.Itoa(fibonacci.MaxLen), expected: "out_of_range", message: overLimit},
		// no instance takes these, whatever its limit
		{name: "above the highest limit", query: "n=" + strconv.Itoa(fibonacci.MaxLen+1), expected: "invalid_argument", message: tooLarge},
		{name: "beyond 64 bits", query: "n=99999999999999999999", expected: "invalid_argument", message: tooLarge},
		// the way a value is written is checked before its size, so these are not told that they are too large
		{name: "plus sign beyond 64 bits", query: "n=+99999999999999999999", expected: "invalid_argument", message: malformed},
		{name: "leading zero beyond 64 bits", query: "n=099999999999999999999", expected: "invalid_argument", message: malformed},
		{name: "letters after digits beyond 64 bits", query: "n=99999999999999999999abc", expected: "invalid_argument", message: malformed},
		// each of these would be n=5 after unescaping the query or splitting it at the separators
		{name: "escaped value", query: "n=%35", expected: "invalid_argument", message: malformed},
		{name: "escaped name", query: "%6E=5", expected: "invalid_argument", message: notAlone},
		{name: "separator after", query: "n=5&", expected: "invalid_argument", message: notAlone},
		{name: "separator before", query: "&n=5", expected: "invalid_argument", message: notAlone},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response := do(t, handler, http.MethodGet, "/v1/fibonacci?"+test.query)

			assert.Equal(t, http.StatusBadRequest, response.Code)
			assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))

			code, message := decodeError(t, response)

			assert.Equal(t, test.expected, code)
			assert.Equal(t, test.message, message)
		})
	}

	// the case the task asks for, down to the wire format
	response := do(t, handler, http.MethodGet, "/v1/fibonacci?n=-5")

	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.JSONEq(t, `{"code": "invalid_argument", "message": "n must not be negative"}`, response.Body.String())
}

func TestRouting(t *testing.T) {
	t.Parallel()

	handler := newHandler(t)

	for _, target := range []string{
		"/", "/v1", "/v1/fibonacci/", "/v1/fibonacci/5", "/fibonacci?n=5", "/metrics",
		// other spellings of the path are other paths, and they are not redirected to it either
		"//v1/fibonacci?n=5", "/v1/./fibonacci?n=5", "/v1/%66ibonacci?n=5",
	} {
		response := do(t, handler, http.MethodGet, target)

		assert.Equal(t, http.StatusNotFound, response.Code, target)

		code, _ := decodeError(t, response)

		assert.Equal(t, "not_found", code, target)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		response := do(t, handler, method, "/v1/fibonacci?n=5")

		assert.Equal(t, http.StatusMethodNotAllowed, response.Code, method)
		assert.Equal(t, "GET, HEAD", response.Header().Get("Allow"), method)

		code, _ := decodeError(t, response)

		assert.Equal(t, "method_not_allowed", code, method)
	}

	// a path which is not there is not found, whatever the method is
	response := do(t, handler, http.MethodPost, "/")

	assert.Equal(t, http.StatusNotFound, response.Code)
}

// TestWriteFailure checks that a response which could not be written is reported,
// and that the handler serves without a function to report it to.
func TestWriteFailure(t *testing.T) {
	t.Parallel()

	table, err := fibonacci.NewTable(limit)
	require.NoError(t, err)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/fibonacci?n=5", nil)

	var reported error

	api.NewHandler(table, func(err error) { reported = err }).ServeHTTP(brokenWriter{httptest.NewRecorder()}, request)

	assert.ErrorIs(t, reported, errBroken)

	assert.NotPanics(t, func() {
		api.NewHandler(table, nil).ServeHTTP(brokenWriter{httptest.NewRecorder()}, request)
	})
}

// FuzzFibonacci checks that every request target gets the response it should.
//
// The expected response is worked out from the target as a string, with a regular expression
// and big integers, so the check shares no code with the parsing it checks.
func FuzzFibonacci(f *testing.F) {
	for _, target := range []string{
		"/v1/fibonacci?n=5", "/v1/fibonacci?n=0", "/v1/fibonacci?n=100", "/v1/fibonacci?n=101", "/v1/fibonacci?n=-1",
		"/v1/fibonacci", "/v1/fibonacci?", "/v1/fibonacci?n", "/v1/fibonacci?n=", "/v1/fibonacci?n=05", "/v1/fibonacci?n=+5", "/v1/fibonacci?n=%35", "/v1/fibonacci?%6E=5",
		"/v1/fibonacci?n=5&", "/v1/fibonacci?&n=5", "/v1/fibonacci?n=1&n=2", "/v1/fibonacci?n=1&m=2", "/v1/fibonacci?N=5",
		"/v1/fibonacci?n=5;", "/v1/fibonacci?n=%zz", "/v1/fibonacci?n=٥", "/v1/fibonacci?n=5#",
		"/v1/fibonacci?n=20000", "/v1/fibonacci?n=20001", "/v1/fibonacci?n=99999999999999999999",
		"/v1/fibonacci?n=+99999999999999999999", "/v1/fibonacci?n=099999999999999999999", "/v1/fibonacci?n=99999999999999999999abc",
		"/v1/fibonacci?n=-", "/v1/fibonacci?n=-0", "/v1/fibonacci?n=-05", "/v1/fibonacci?n=-99999999999999999999",
		"//v1/fibonacci?n=5", "/v1/%66ibonacci?n=5", "/v1/fibonacci/?n=5", "/",
	} {
		f.Add(target)
	}

	table, err := fibonacci.NewTable(limit)
	if err != nil {
		f.Fatal(err)
	}

	handler := api.NewHandler(table, nil)
	canonical := regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

	f.Fuzz(func(t *testing.T, target string) {
		// net/http parses the target of a request with the same function, and rejects what it rejects
		requestURL, err := url.ParseRequestURI(target)
		if err != nil || !strings.HasPrefix(target, "/") {
			t.Skip()
		}

		request := (&http.Request{Method: http.MethodGet, URL: requestURL}).WithContext(t.Context())
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		path, query, _ := strings.Cut(target, "?")
		value, isN := strings.CutPrefix(query, "n=")
		isNumber := isN && canonical.MatchString(value)
		n, _ := new(big.Int).SetString(value, 10)

		var (
			status int
			code   string
		)

		switch {
		case path != "/v1/fibonacci":
			status, code = http.StatusNotFound, "not_found"
		case !isNumber || n.Cmp(big.NewInt(fibonacci.MaxLen)) > 0:
			status, code = http.StatusBadRequest, "invalid_argument"
		case n.Cmp(big.NewInt(limit)) > 0:
			status, code = http.StatusBadRequest, "out_of_range"
		default:
			require.Equal(t, http.StatusOK, response.Code)

			var values []json.Number

			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &values))
			require.Len(t, values, int(n.Int64()))

			return
		}

		require.Equal(t, status, response.Code)

		actual, _ := decodeError(t, response)

		require.Equal(t, code, actual)
	})
}

// BenchmarkFibonacci measures the handler without the server around it.
//
// What it allocates are the header values, the body is written straight from the table.
func BenchmarkFibonacci(b *testing.B) {
	table, err := fibonacci.NewTable(1000)
	if err != nil {
		b.Fatal(err)
	}

	handler := api.NewHandler(table, nil)
	request := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/v1/fibonacci?n=1000", nil)
	w := discardWriter{}

	b.ReportAllocs()

	for b.Loop() {
		clear(w)

		handler.ServeHTTP(w, request)
	}
}

func newHandler(t *testing.T) http.Handler {
	t.Helper()

	table, err := fibonacci.NewTable(limit)
	require.NoError(t, err)

	return api.NewHandler(table, func(err error) { t.Errorf("failed to write a response: %v", err) })
}

func do(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()

	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), method, target, nil))

	return response
}

func decodeError(t *testing.T, response *httptest.ResponseRecorder) (code, message string) {
	t.Helper()

	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	decoder := json.NewDecoder(response.Body)
	decoder.DisallowUnknownFields()

	require.NoError(t, decoder.Decode(&body))

	return body.Code, body.Message
}

var errBroken = errors.New("broken")

// brokenWriter is a response writer which fails to write the body.
type brokenWriter struct {
	http.ResponseWriter
}

func (brokenWriter) Write([]byte) (int, error) {
	return 0, errBroken
}

// discardWriter is a response writer which keeps the headers and nothing else.
type discardWriter http.Header

func (w discardWriter) Header() http.Header {
	return http.Header(w)
}

func (discardWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (discardWriter) WriteHeader(int) {}
