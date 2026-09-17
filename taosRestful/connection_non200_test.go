package taosRestful

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	taosErrors "github.com/taosdata/driver-go/v3/errors"
)

func newNon200TestConn(t *testing.T, handler http.HandlerFunc, header map[string][]string) (*taosConn, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	u, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	tc := &taosConn{
		cfg:    &Config{},
		client: server.Client(),
		url:    u,
		header: header,
	}
	return tc, server.Close
}

// taosAdapter with httpCodeServerError=true answers C API errors with a
// non-200 status while still sending the TDengine error JSON in the body.
// The driver must surface the same typed error as with a 200 response.
func TestTaosQueryNon200ParsesTDengineErrorBody(t *testing.T) {
	tc, cleanup := newNon200TestConn(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":855,"desc":"Authentication failure"}`))
	}, map[string][]string{})
	defer cleanup()

	_, err := tc.taosQuery(context.Background(), "show databases", 512)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var taosErr *taosErrors.TaosError
	if !errors.As(err, &taosErr) {
		t.Fatalf("expected typed *TaosError, got %T: %v", err, err)
	}
	if taosErr.Code != 0x357 {
		t.Fatalf("expected error code 0x357, got %#x", taosErr.Code)
	}
	if taosErr.ErrStr != "Authentication failure" {
		t.Fatalf("expected error message %q, got %q", "Authentication failure", taosErr.ErrStr)
	}
}

func TestTaosQueryNon200GzipTDengineErrorBody(t *testing.T) {
	tc, cleanup := newNon200TestConn(t, func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write([]byte(`{"code":855,"desc":"Authentication failure"}`))
		_ = gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(buf.Bytes())
	}, map[string][]string{"Accept-Encoding": {"gzip"}})
	defer cleanup()

	_, err := tc.taosQuery(context.Background(), "show databases", 512)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var taosErr *taosErrors.TaosError
	if !errors.As(err, &taosErr) {
		t.Fatalf("expected typed *TaosError, got %T: %v", err, err)
	}
	if taosErr.Code != 0x357 {
		t.Fatalf("expected error code 0x357, got %#x", taosErr.Code)
	}
}

// A non-200 response from a gateway/proxy without a TDengine error JSON body
// must keep the historical "server response: status - body" message.
func TestTaosQueryNon200NonJSONBodyKeepsStatusMessage(t *testing.T) {
	tc, cleanup := newNon200TestConn(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}, map[string][]string{})
	defer cleanup()

	_, err := tc.taosQuery(context.Background(), "show databases", 512)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var taosErr *taosErrors.TaosError
	if errors.As(err, &taosErr) {
		t.Fatalf("expected plain error, got typed *TaosError: %v", taosErr)
	}
	if !strings.Contains(err.Error(), "server response: 502 Bad Gateway") ||
		!strings.Contains(err.Error(), "<html>bad gateway</html>") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// A non-200 response whose body carries code 0 is not a TDengine error and
// must not be treated as a success.
func TestTaosQueryNon200ZeroCodeBodyKeepsStatusMessage(t *testing.T) {
	tc, cleanup := newNon200TestConn(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":0,"desc":""}`))
	}, map[string][]string{})
	defer cleanup()

	_, err := tc.taosQuery(context.Background(), "show databases", 512)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "server response: 500 Internal Server Error") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// A proxy may wrongly set Content-Encoding: gzip on an uncompressed body.
// The driver must fall back to the raw body instead of failing with a gzip
// header error, so the typed TDengine error is still surfaced.
func TestTaosQueryNon200GzipHeaderWithPlainBody(t *testing.T) {
	tc, cleanup := newNon200TestConn(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":855,"desc":"Authentication failure"}`))
	}, map[string][]string{"Accept-Encoding": {"gzip"}})
	defer cleanup()

	_, err := tc.taosQuery(context.Background(), "show databases", 512)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var taosErr *taosErrors.TaosError
	if !errors.As(err, &taosErr) {
		t.Fatalf("expected typed *TaosError, got %T: %v", err, err)
	}
	if taosErr.Code != 0x357 {
		t.Fatalf("expected error code 0x357, got %#x", taosErr.Code)
	}
}

// End-to-end through database/sql, mirroring how taoskeeper connects.
func TestNon200AuthFailureViaSQLOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":855,"desc":"Authentication failure"}`))
	}))
	defer server.Close()

	dsn := fmt.Sprintf("root:taosdata@http(%s)/", strings.TrimPrefix(server.URL, "http://"))
	db, err := sql.Open("taosRestful", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(context.Background(), "show databases")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var taosErr *taosErrors.TaosError
	if !errors.As(err, &taosErr) {
		t.Fatalf("expected typed *TaosError, got %T: %v", err, err)
	}
	if taosErr.Code != 0x357 {
		t.Fatalf("expected error code 0x357, got %#x", taosErr.Code)
	}
}
