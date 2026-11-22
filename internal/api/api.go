// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package api implements the HTTP API of the Fibonacci service.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/aozmekik/fibonacci-service/internal/fibonacci"
)

// path is the only path of the API.
const path = "/v1/fibonacci"

// The codes of the errors, which are the part of an error response clients can rely on.
//
// The first two follow the gRPC codes of the same names: an argument is invalid when no
// instance of the service would take it, and out of range when this one does not.
const (
	codeInvalidArgument  = "invalid_argument"
	codeOutOfRange       = "out_of_range"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
)

// The errors of parseN. Their messages are sent to the client the way they are.
var (
	errMissing   = errors.New("n is required")
	errNotAlone  = errors.New("n must be the only parameter, and given once")
	errNegative  = errors.New("n must not be negative")
	errMalformed = errors.New("n must be a decimal integer without a sign or leading zeros")
	errTooLarge  = fmt.Errorf("n must not be greater than %d", fibonacci.MaxLen)
)

// errorBody is the body of every response of the API which is not a success.
//
// The message is there for humans, and it never repeats the input.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type handler struct {
	table       *fibonacci.Table
	writeFailed func(error)
}

// NewHandler returns the handler of the API, which serves the numbers from the given table.
//
// writeFailed is called when a response could not be written in full. The status is sent
// by then, so the caller has no other way to learn about it. It may be nil.
func NewHandler(table *fibonacci.Table, writeFailed func(error)) http.Handler {
	if writeFailed == nil {
		writeFailed = func(error) {}
	}

	return &handler{table: table, writeFailed: writeFailed}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// The path is compared the way it was sent, as each response is to have one URL.
	// http.ServeMux matches an escaped letter too, and redirects a doubled slash.
	if r.URL.EscapedPath() != path {
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such path")

		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")

		writeError(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "only GET and HEAD are allowed")

		return
	}

	n, err := parseN(r.URL.RawQuery)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, err.Error())

		return
	}

	// The query is one that the service takes. Whether this instance does is up to its limit.
	if n > h.table.Len() {
		writeError(w, r, http.StatusBadRequest, codeOutOfRange, fmt.Sprintf("n must not be greater than %d", h.table.Len()))

		return
	}

	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(h.table.JSONLen(n)))
	// The numbers never change, and they do not depend on the configuration of the service.
	// The lifetime is kept at a day to limit for how long a broken release would be cached.
	header.Set("Cache-Control", "public, max-age=86400, immutable")

	if r.Method == http.MethodHead {
		return
	}

	// A write fails when the client goes away, or reads too slowly for the write timeout of the
	// server, before the rest of the response is in the buffers of the connection. A client which
	// leaves later goes unnoticed. The length is in the headers, so the client can tell.
	if err := h.table.WriteJSON(w, n); err != nil {
		h.writeFailed(err)
	}
}

// parseN returns the n of the query.
//
// The query has one form: "n=" and a decimal integer without a sign or leading zeros,
// which is at most fibonacci.MaxLen. It is compared the way it was sent, so "n=%35" and
// "n=5&" are not accepted for "n=5". It is easier to accept more later than to take
// something back.
//
// An error means that no instance of the service takes the query, whatever its limit is.
func parseN(rawQuery string) (int, error) {
	// a name without "=" is a parameter with an empty value, so "n" goes on the way "n=" does
	name, value, _ := strings.Cut(rawQuery, "=")

	switch {
	case rawQuery == "":
		return 0, errMissing
	case name != "n" || strings.Contains(value, "&"):
		return 0, errNotAlone
	}

	// a negative number gets a message of its own, if the sign is all that is wrong with it
	if digits, negative := strings.CutPrefix(value, "-"); negative && digits != "0" && isCanonical(digits) {
		return 0, errNegative
	}

	if !isCanonical(value) {
		return 0, errMalformed
	}

	// The value is all digits by now, so the only way left to fail is that it does not fit into an int.
	n, err := strconv.Atoi(value)
	if err != nil || n > fibonacci.MaxLen {
		return 0, errTooLarge
	}

	return n, nil
}

// isCanonical reports whether value is a decimal integer written without a sign and without leading zeros.
func isCanonical(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}

	for _, c := range []byte(value) {
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	// a struct of strings always encodes
	data, _ := json.Marshal(errorBody{Code: code, Message: message}) //nolint:errcheck,errchkjson

	header := w.Header()
	header.Set("Content-Type", "application/json")
	// net/http works the length out from the body, which a response to HEAD does not get
	header.Set("Content-Length", strconv.Itoa(len(data)))
	// An error can depend on the configuration, e.g. on the limit, so it must not outlive it in a cache.
	header.Set("Cache-Control", "no-store")

	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}

	// a failed write means that the client is gone, and there is nothing left to tell it
	w.Write(data) //nolint:errcheck
}
