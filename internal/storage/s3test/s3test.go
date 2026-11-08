// Package s3test is a minimal in-process S3 server (path-style PUT/HEAD/GET of
// single objects) for testing the real S3 adapter without any cloud account.
package s3test

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type object struct {
	data []byte
	meta map[string]string
}

// Server is a fake S3 endpoint.
type Server struct {
	*httptest.Server
	bucket string

	mu   sync.Mutex
	objs map[string]object

	failPuts atomic.Int32
	// PutGate, if set, is waited on (or the request context) before a PUT is stored.
	PutGate chan struct{}
	Puts    atomic.Int32
	Heads   atomic.Int32
	// PartialPuts counts PUT requests whose context ended before the object was stored.
	PartialPuts atomic.Int32
}

// New starts a plain-HTTP server (adapter tests).
func New(t testing.TB, bucket string) *Server { return start(t, bucket, false) }

// NewTLS starts an HTTPS server (needed when the binary under test requires an https endpoint).
func NewTLS(t testing.TB, bucket string) *Server { return start(t, bucket, true) }

func start(t testing.TB, bucket string, tls bool) *Server {
	s := &Server{bucket: bucket, objs: map[string]object{}}
	h := http.HandlerFunc(s.handle)
	if tls {
		s.Server = httptest.NewTLSServer(h)
	} else {
		s.Server = httptest.NewServer(h)
	}
	t.Cleanup(s.Server.Close)
	return s
}

// FailNextPuts makes the next n PUT requests return HTTP 500.
func (s *Server) FailNextPuts(n int) { s.failPuts.Store(int32(n)) }

// Object returns a stored object.
func (s *Server) Object(key string) ([]byte, map[string]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[key]
	return o.data, o.meta, ok
}

// Delete removes an object.
func (s *Server) Delete(key string) { s.mu.Lock(); delete(s.objs, key); s.mu.Unlock() }

// Keys lists object keys.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k []string
	for key := range s.objs {
		k = append(k, key)
	}
	return k
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != s.bucket || key == "" {
		s.xmlError(w, r, 404, "NoSuchBucket")
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.put(w, r, key)
	case http.MethodHead:
		s.Heads.Add(1)
		s.mu.Lock()
		o, ok := s.objs[key]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		writeMeta(w, o)
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		w.WriteHeader(200)
	case http.MethodGet:
		s.mu.Lock()
		o, ok := s.objs[key]
		s.mu.Unlock()
		if !ok {
			s.xmlError(w, r, 404, "NoSuchKey")
			return
		}
		writeMeta(w, o)
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		w.Write(o.data)
	default:
		s.xmlError(w, r, 405, "MethodNotAllowed")
	}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, key string) {
	s.Puts.Add(1)
	if s.failPuts.Add(-1) >= 0 {
		io.Copy(io.Discard, r.Body)
		s.xmlError(w, r, 500, "InternalError")
		return
	}
	s.failPuts.Store(0)
	data, err := readBody(r)
	if err != nil {
		s.PartialPuts.Add(1)
		http.Error(w, err.Error(), 400)
		return
	}
	if gate := s.PutGate; gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			s.PartialPuts.Add(1)
			return
		}
	}
	meta := map[string]string{}
	for k, v := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-meta-") {
			meta[strings.TrimPrefix(lk, "x-amz-meta-")] = v[0]
		}
	}
	s.mu.Lock()
	s.objs[key] = object{data: data, meta: meta}
	s.mu.Unlock()
	sum := md5.Sum(data)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(200)
}

func writeMeta(w http.ResponseWriter, o object) {
	for k, v := range o.meta {
		w.Header().Set("x-amz-meta-"+k, v)
	}
}

func (s *Server) xmlError(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
	}
}

// readBody returns the request payload, decoding aws-chunked framing if the SDK used it.
func readBody(r *http.Request) ([]byte, error) {
	if !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") &&
		!strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeHex, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			io.Copy(io.Discard, br) // trailers
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, n); err != nil {
			return nil, err
		}
		br.Discard(2)
	}
}

// Set stores an object directly (e.g. to simulate bit-rot or tampering).
func (s *Server) Set(key string, data []byte, meta map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = object{data: data, meta: meta}
}
