//  Copyright (c) 2018 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httputil

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/cenkalti/backoff"
)

var retryableCodes = map[int]struct{}{
	http.StatusTooManyRequests:    {},
	http.StatusBadGateway:         {},
	http.StatusServiceUnavailable: {},
	http.StatusGatewayTimeout:     {},
}

// StatusError occurs if an HTTP response has an unexpected status code.
type StatusError struct {
	Method       string
	URL          string
	Status       int
	Header       http.Header
	ResponseDump string
}

// NewStatusError returns a new StatusError.
func NewStatusError(resp *http.Response) StatusError {
	defer resp.Body.Close()
	respBytes, err := ioutil.ReadAll(resp.Body)
	respDump := string(respBytes)
	if err != nil {
		respDump = fmt.Sprintf("failed to dump response: %s", err)
	}
	return StatusError{
		Method:       resp.Request.Method,
		URL:          resp.Request.URL.String(),
		Status:       resp.StatusCode,
		Header:       resp.Header,
		ResponseDump: respDump,
	}
}

func (e StatusError) Error() string {
	if e.ResponseDump == "" {
		return fmt.Sprintf("%s %s %d", e.Method, e.URL, e.Status)
	}
	return fmt.Sprintf("%s %s %d: %s", e.Method, e.URL, e.Status, e.ResponseDump)
}

// IsStatus returns true if err is a StatusError of the given status.
func IsStatus(err error, status int) bool {
	var e StatusError
	if errors.As(err, &e) {
		return e.Status == status
	}
	return false
}

// IsCreated returns true if err is a "created", 201
func IsCreated(err error) bool {
	return IsStatus(err, http.StatusCreated)
}

// IsNotFound returns true if err is a "not found" StatusError.
func IsNotFound(err error) bool {
	return IsStatus(err, http.StatusNotFound)
}

// IsConflict returns true if err is a "status conflict" StatusError.
func IsConflict(err error) bool {
	return IsStatus(err, http.StatusConflict)
}

// IsAccepted returns true if err is a "status accepted" StatusError.
func IsAccepted(err error) bool {
	return IsStatus(err, http.StatusAccepted)
}

// IsForbidden returns true if statis code is 403 "forbidden"
func IsForbidden(err error) bool {
	return IsStatus(err, http.StatusForbidden)
}

func isRetryable(code int) bool {
	_, ok := retryableCodes[code]
	return ok
}

// IsRetryable returns true if the statis code indicates that the request is
// retryable.
func IsRetryable(err error) bool {
	var e StatusError
	if errors.As(err, &e) {
		return isRetryable(e.Status)
	}
	return false
}

// NetworkError occurs on any Send error which occurred while trying to send
// the HTTP request, e.g. the given host is unresponsive.
type NetworkError struct {
	err error
}

func (e NetworkError) Error() string {
	return fmt.Sprintf("network error: %s", e.err)
}

// IsNetworkError returns true if err is a NetworkError.
func IsNetworkError(err error) bool {
	var e NetworkError
	return errors.As(err, &e)
}

type sendOptions struct {
	body          io.Reader
	timeout       time.Duration
	acceptedCodes map[int]bool
	headers       map[string]string
	redirect      func(req *http.Request, via []*http.Request) error
	retry         retryOptions
	transport     http.RoundTripper
	ctx           context.Context

	// This is not a valid http option. It provides a way to override
	// http.Client. This should always used by tests.
	client *http.Client

	// This is not a valid http option. It provides a way to override
	// parts of the url. For example, url.Scheme can be changed from
	// http to https.
	url *url.URL

	// This is not a valid http option. HTTP fallback is added to allow
	// easier migration from http to https.
	// In go1.11 and go1.12, the responses returned when http request is
	// sent to https server are different in the fallback mode:
	// go1.11 returns a network error whereas go1.12 returns BadRequest.
	// This causes TestTLSClientBadAuth to fail because the test checks
	// retry error.
	// This flag is added to allow disabling http fallback in unit tests.
	// NOTE: it does not impact how it runs in production.
	httpFallbackDisabled bool
}

// SendOption allows overriding defaults for the Send function.
type SendOption func(*sendOptions)

// SendNoop returns a no-op option.
func SendNoop() SendOption {
	return func(o *sendOptions) {}
}

// SendBody specifies a body for http request
func SendBody(body io.Reader) SendOption {
	return func(o *sendOptions) { o.body = body }
}

// SendTimeout specifies timeout for http request
func SendTimeout(timeout time.Duration) SendOption {
	return func(o *sendOptions) { o.timeout = timeout }
}

// SendHeaders specifies headers for http request
func SendHeaders(headers map[string]string) SendOption {
	return func(o *sendOptions) { o.headers = headers }
}

// SendAcceptedCodes specifies accepted codes for http request
func SendAcceptedCodes(codes ...int) SendOption {
	m := make(map[int]bool)
	for _, c := range codes {
		m[c] = true
	}
	return func(o *sendOptions) { o.acceptedCodes = m }
}

// SendRedirect specifies a redirect policy for http request
func SendRedirect(redirect func(req *http.Request, via []*http.Request) error) SendOption {
	return func(o *sendOptions) { o.redirect = redirect }
}

// SendClient specifies a http client.
func SendClient(client *http.Client) SendOption {
	return func(o *sendOptions) { o.client = client }
}

type retryOptions struct {
	backoff    backoff.BackOff
	extraCodes map[int]bool
}

// RetryOption allows overriding defaults for the SendRetry option.
type RetryOption func(*retryOptions)

// RetryBackoff adds exponential backoff between retries.
func RetryBackoff(b backoff.BackOff) RetryOption {
	return func(o *retryOptions) { o.backoff = b }
}

// RetryCodes adds more status codes to be retried (in addition to the default
// retryableCodes).
func RetryCodes(codes ...int) RetryOption {
	return func(o *retryOptions) {
		for _, c := range codes {
			o.extraCodes[c] = true
		}
	}
}

// SendRetry will we retry the request on network / 5XX errors.
func SendRetry(options ...RetryOption) SendOption {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 250 * time.Millisecond
	b.Multiplier = 1 // No backoff.
	b.MaxInterval = 30 * time.Second
	retry := retryOptions{
		backoff:    backoff.WithMaxRetries(b, 3),
		extraCodes: make(map[int]bool),
	}
	for _, o := range options {
		o(&retry)
	}
	return func(o *sendOptions) { o.retry = retry }
}

// DisableHTTPFallback disables http fallback when https request fails.
func DisableHTTPFallback() SendOption {
	return func(o *sendOptions) {
		o.httpFallbackDisabled = true
	}
}

// SendTLS sets the transport with TLS config for the HTTP client.
func SendTLS(config *tls.Config) SendOption {
	return func(o *sendOptions) {
		if config == nil {
			return
		}
		o.transport = &http.Transport{TLSClientConfig: config}
		o.url.Scheme = "https"
	}
}

// SendTLSTransport sets the transport with TLS config for the HTTP client.
func SendTLSTransport(transport http.RoundTripper) SendOption {
	return func(o *sendOptions) {
		o.transport = transport
		o.url.Scheme = "https"
	}
}

// SendTransport sets the transport for the HTTP client.
func SendTransport(transport http.RoundTripper) SendOption {
	return func(o *sendOptions) { o.transport = transport }
}

// SendContext sets the context for the HTTP client.
func SendContext(ctx context.Context) SendOption {
	return func(o *sendOptions) { o.ctx = ctx }
}

// Send sends an HTTP request. May return NetworkError or StatusError (see above).
func Send(method, rawurl string, options ...SendOption) (*http.Response, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("parse url: %s", err)
	}
	opts := &sendOptions{
		body:                 nil,
		timeout:              60 * time.Second,
		acceptedCodes:        map[int]bool{http.StatusOK: true},
		headers:              map[string]string{},
		retry:                retryOptions{backoff: &backoff.StopBackOff{}},
		transport:            nil, // Use HTTP default.
		ctx:                  context.Background(),
		url:                  u,
		httpFallbackDisabled: false,
	}
	for _, o := range options {
		o(opts)
	}

	req, err := newRequest(method, opts)
	if err != nil {
		return nil, err
	}

	client := opts.client
	if client == nil {
		client = &http.Client{
			Timeout:       opts.timeout,
			CheckRedirect: opts.redirect,
			Transport:     opts.transport,
		}
	}

	var resp *http.Response
	for {
		resp, err = client.Do(req)
		if err != nil || shouldRetry(resp, opts) {
			d := opts.retry.backoff.NextBackOff()
			if d == backoff.Stop {
				break // Backoff timed out.
			}
			time.Sleep(d)
			continue
		}
		break
	}
	if err != nil {
		return nil, NetworkError{err}
	}
	if !opts.acceptedCodes[resp.StatusCode] {
		return nil, NewStatusError(resp)
	}
	return resp, nil
}

// Get sends a GET http request.
func Get(url string, options ...SendOption) (*http.Response, error) {
	return Send("GET", url, options...)
}

// Head sends a HEAD http request.
func Head(url string, options ...SendOption) (*http.Response, error) {
	return Send("HEAD", url, options...)
}

// Post sends a POST http request.
func Post(url string, options ...SendOption) (*http.Response, error) {
	return Send("POST", url, options...)
}

// Put sends a PUT http request.
func Put(url string, options ...SendOption) (*http.Response, error) {
	return Send("PUT", url, options...)
}

// Patch sends a PATCH http request.
func Patch(url string, options ...SendOption) (*http.Response, error) {
	return Send("PATCH", url, options...)
}

// Delete sends a DELETE http request.
func Delete(url string, options ...SendOption) (*http.Response, error) {
	return Send("DELETE", url, options...)
}

func newRequest(method string, opts *sendOptions) (*http.Request, error) {
	req, err := http.NewRequest(method, opts.url.String(), opts.body)
	if err != nil {
		return nil, fmt.Errorf("new request: %s", err)
	}
	req = req.WithContext(opts.ctx)
	if opts.body == nil {
		req.ContentLength = 0
	}
	for key, val := range opts.headers {
		req.Header.Set(key, val)
	}
	return req, nil
}

func fallbackToHTTP(
	client *http.Client, method string, opts *sendOptions) (*http.Response, error) {

	req, err := newRequest(method, opts)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = "http"

	return client.Do(req)
}

func shouldFallbackToHTTP(req *http.Request, resp *http.Response, opts *sendOptions) bool {
	if req.URL.Scheme == "http" { // Already in HTTP.
		return false
	}
	// Try fallback on non-retryable errors.
	return !shouldRetry(resp, opts)
}

func shouldRetry(resp *http.Response, opts *sendOptions) bool {
	if resp != nil {
		return (isRetryable(resp.StatusCode) && !opts.acceptedCodes[resp.StatusCode]) ||
			(opts.retry.extraCodes[resp.StatusCode])
	}
	return false
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
