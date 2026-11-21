// Package awstest is a recording fake AWS endpoint for adapter tests. It is an httptest server that
// records every request an SDK client sends (method, path, X-Amz-Target, form fields, JSON body) and
// answers from handlers the test registers, so adapters are tested for the requests they make and the
// responses they parse without a network or an account. Nothing in it speaks to AWS.
package awstest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// Request is one recorded request.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	// Action is the IAM/Query-protocol action ("CreateRole") or the X-Amz-Target operation.
	Action string
	// Form holds the decoded form fields of a Query-protocol request.
	Form url.Values
	// Body is the raw body.
	Body []byte
}

// JSON decodes the body into v (JSON protocols).
func (r Request) JSON(v any) error { return json.Unmarshal(r.Body, v) }

// Response is what a handler answers.
type Response struct {
	Status int
	Body   string
	// ContentType defaults by protocol: text/xml for Query, application/json otherwise.
	ContentType string
}

// Handler answers a request, or returns nil to pass to the next handler.
type Handler func(Request) *Response

// Server is the recorder.
type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []Request
	handlers []Handler
}

// New starts a recorder; it is closed when t ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the endpoint to give the SDK.
func (s *Server) URL() string { return s.srv.URL }

// Config returns an SDK configuration that talks to the recorder with dummy credentials.
func (s *Server) Config() awssdk.Config {
	return awssdk.Config{
		Region:       "us-east-1",
		BaseEndpoint: awssdk.String(s.srv.URL),
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret-for-tests-only", ""),
		HTTPClient:   s.srv.Client(),
		// Tests must not retry into the recorder's request count.
		RetryMaxAttempts: 1,
	}
}

// On registers a handler; handlers run in registration order and the first non-nil answer wins.
func (s *Server) On(h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers = append(s.handlers, h)
}

// OnAction answers every request of an action with a fixed response.
func (s *Server) OnAction(action string, resp Response) {
	s.On(func(r Request) *Response {
		if r.Action == action {
			return &resp
		}
		return nil
	})
}

// Requests returns a copy of every request recorded so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Actions returns the recorded actions in order.
func (s *Server) Actions() []string {
	var out []string
	for _, r := range s.Requests() {
		out = append(out, r.Action)
	}
	return out
}

// Count returns how many requests were made for an action.
func (s *Server) Count(action string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Action == action {
			n++
		}
	}
	return n
}

// Find returns the first recorded request for an action.
func (s *Server) Find(action string) (Request, bool) {
	for _, r := range s.Requests() {
		if r.Action == action {
			return r, true
		}
	}
	return Request{}, false
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body}
	if t := r.Header.Get("X-Amz-Target"); t != "" {
		_, req.Action, _ = strings.Cut(t, ".")
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		req.Form, _ = url.ParseQuery(string(body))
		req.Action = req.Form.Get("Action")
	}
	if req.Action == "" {
		req.Action = r.Method + " " + r.URL.Path
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	handlers := append([]Handler(nil), s.handlers...)
	s.mu.Unlock()
	for _, h := range handlers {
		if resp := h(req); resp != nil {
			ct := resp.ContentType
			if ct == "" {
				ct = "application/json"
				if req.Form != nil {
					ct = "text/xml"
				}
			}
			w.Header().Set("Content-Type", ct)
			if resp.Status == 0 {
				resp.Status = http.StatusOK
			}
			w.WriteHeader(resp.Status)
			_, _ = io.WriteString(w, resp.Body)
			return
		}
	}
	http.Error(w, fmt.Sprintf("awstest: no handler for %s", req.Action), http.StatusNotImplemented)
}

// QueryOK is a successful Query-protocol (IAM) response: <ActionResponse><ActionResult>inner</...>.
func QueryOK(action, inner string) Response {
	return Response{Body: fmt.Sprintf(`<%[1]sResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><%[1]sResult>%[2]s</%[1]sResult><ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata></%[1]sResponse>`, action, inner)}
}

// QueryEmptyOK is a successful Query-protocol response with no result (PutRolePolicy and others).
func QueryEmptyOK(action string) Response {
	return Response{Body: fmt.Sprintf(`<%[1]sResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata></%[1]sResponse>`, action)}
}

// QueryError is a Query-protocol error response with an HTTP status and an AWS error code.
func QueryError(status int, code, message string) Response {
	return Response{Status: status, Body: fmt.Sprintf(`<ErrorResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>req-1</RequestId></ErrorResponse>`, code, message)}
}

// JSONOK is a successful JSON-protocol or REST-JSON response.
func JSONOK(v any) Response {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return Response{Body: string(b)}
}

// JSONError is a REST-JSON error: the error type goes in the X-Amzn-Errortype header in real AWS; the
// SDK also reads "__type" from the body, which is what is used here.
func JSONError(status int, code, message string) Response {
	return Response{Status: status, Body: fmt.Sprintf(`{"__type":%q,"message":%q}`, code, message)}
}
