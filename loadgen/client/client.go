// Package client is a client for loadgen's control API (DESIGN.md sec
// 10), for gate Stage 5 and the orchestrator. It reaches loadgen through
// the base URL and *http.Client its caller passes, so the transport is
// the caller's. It imports the standard library and loadgen/wire only.
//
// It is strict: a response it cannot fully account for is an error,
// never a guess. A method returns its wire type only for a response
// with one of the statuses loadgen answers that endpoint's success with,
// Content-Type application/json, one boot_id in the wire.BootIDHeader
// header and the same boot_id in the body, and a body of one JSON value
// with no field the wire type lacks and nothing after it. Any other
// status is an *APIError. Once Expect is called, a response carrying
// another boot_id is ErrBootChanged, whatever its status.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/codeinthecreek/eK8s/loadgen/wire"
)

// maxResponseBody caps every response body the client reads, in bytes.
// The largest body loadgen sends is a records page, so the cap derives
// from loadgen's own caps: a page holds at most maxRecordsPage records
// (loadgen/control_http.go), each at most maxRecordJSON bytes. A
// record's two evidence byte fields, base64 on the wire, each hold at
// most maxEvidenceBytes: the response body loadgen read, which it caps
// at classifier.MaxBodyBytes+1 (4 KiB + 1), and the request body it
// sent, which it keeps inside cmd/service's 4 KiB body limit
// (maxTooLongLen, loadgen/negative.go). recordRest covers the rest of a
// record: names, numbers, times, ids, the evidence path, a /hash pair (a password of at
// most 1024 bytes and a 97-byte hash at the fixed parameters) and a
// fault's text. pageSlack covers the page's own fields. loadgen's
// TestClientRecordsCap checks that a maximal page fits.
const (
	maxRecordsPage   = 1000
	maxEvidenceBytes = 4<<10 + 1
	recordRest       = 8 << 10
	maxRecordJSON    = 2*((maxEvidenceBytes+2)/3*4) + recordRest
	pageSlack        = 4 << 10
	maxResponseBody  = maxRecordsPage*maxRecordJSON + pageSlack
)

// ErrBootChanged is the error, wrapped, for a response whose boot_id
// is not the one Expect was given: loadgen has restarted, and its
// targets, runs and records are gone.
var ErrBootChanged = errors.New("loadgen's boot_id changed")

// APIError is a response with a status other than those loadgen answers
// the endpoint's success with.
type APIError struct {
	// Status is the response's status.
	Status int
	// BootID is the wire.BootIDHeader header's value, "" when the
	// response carried none, an empty one or several: then it may not
	// be from loadgen.
	BootID string
	// Message is the body's wire.Error text, "" unless the body decodes
	// as a wire.Error, as the client decodes every body, whose boot_id
	// is BootID.
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("status %d", e.Status)
	}
	return fmt.Sprintf("status %d: %s", e.Status, e.Message)
}

// Client is a client for one loadgen's control API. It is safe for
// concurrent use.
type Client struct {
	base string
	hc   *http.Client

	mu        sync.Mutex
	want      string // the boot_id Expect was given
	expecting bool   // whether Expect has been called
}

// New returns a Client for the control API at baseURL, with any
// trailing slash dropped, which sends its requests with hc; hc must not
// be nil.
func New(baseURL string, hc *http.Client) *Client {
	return &Client{base: strings.TrimSuffix(baseURL, "/"), hc: hc}
}

// Expect makes any later response whose boot_id is not bootID an error
// matching ErrBootChanged. Before Expect is called, any boot_id is
// accepted. Every response carries a non-empty boot_id, so after
// Expect("") every response is ErrBootChanged.
func (c *Client) Expect(bootID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.want, c.expecting = bootID, true
}

// ExpectAny undoes Expect: any boot_id is accepted again until the next
// Expect. The orchestrator calls it after a loadgen restart, to read the
// new boot_id from GET /config before it expects that one.
func (c *Client) ExpectAny() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.want, c.expecting = "", false
}

// expected returns an error matching ErrBootChanged if Expect was
// called with another boot_id than got.
func (c *Client) expected(got string) error {
	c.mu.Lock()
	want, expecting := c.want, c.expecting
	c.mu.Unlock()
	if expecting && got != want {
		return fmt.Errorf("%w: got %s, want %s", ErrBootChanged, got, want)
	}
	return nil
}

// The statuses loadgen answers each endpoint's success with.
var (
	statusOK      = []int{http.StatusOK}
	statusCreated = []int{http.StatusCreated}
	statusPut     = []int{http.StatusOK, http.StatusCreated}
)

// Config returns loadgen's configuration: GET /config.
func (c *Client) Config(ctx context.Context) (wire.Config, error) {
	var v wire.Config
	if _, err := c.do(ctx, http.MethodGet, "/config", nil, nil, statusOK, &v, &v.BootID); err != nil {
		return wire.Config{}, err
	}
	return v, nil
}

// Targets returns loadgen's targets: GET /targets.
func (c *Client) Targets(ctx context.Context) (wire.Targets, error) {
	var v wire.Targets
	if _, err := c.do(ctx, http.MethodGet, "/targets", nil, nil, statusOK, &v, &v.BootID); err != nil {
		return wire.Targets{}, err
	}
	return v, nil
}

// PutTarget makes id a target, PUT /targets/{id}, and reports whether
// it was created (201) rather than already a target (200). A response
// for another id is an error.
func (c *Client) PutTarget(ctx context.Context, id string) (v wire.PutTarget, created bool, err error) {
	path := "/targets/" + url.PathEscape(id)
	status, err := c.do(ctx, http.MethodPut, path, nil, nil, statusPut, &v, &v.BootID)
	if err == nil && v.ID != id {
		err = fmt.Errorf("loadgen/client: PUT %s: answered for id %q", path, v.ID)
	}
	if err != nil {
		return wire.PutTarget{}, false, err
	}
	return v, status == http.StatusCreated, nil
}

// DeleteTarget drops target id once its records through Seq through
// have been read: DELETE /targets/{id}?through=S.
func (c *Client) DeleteTarget(ctx context.Context, id string, through uint64) (wire.BootID, error) {
	var v wire.BootID
	q := url.Values{"through": {strconv.FormatUint(through, 10)}}
	if _, err := c.do(ctx, http.MethodDelete, "/targets/"+url.PathEscape(id), q, nil, statusOK, &v, &v.BootID); err != nil {
		return wire.BootID{}, err
	}
	return v, nil
}

// Records returns one page of target id's records, at most limit from
// Seq since: GET /targets/{id}/records?since=S&limit=L. The next page
// starts at the page's Next.
func (c *Client) Records(ctx context.Context, id string, since uint64, limit int) (wire.Records, error) {
	var v wire.Records
	q := url.Values{"since": {strconv.FormatUint(since, 10)}, "limit": {strconv.Itoa(limit)}}
	if _, err := c.do(ctx, http.MethodGet, "/targets/"+url.PathEscape(id)+"/records", q, nil, statusOK, &v, &v.BootID); err != nil {
		return wire.Records{}, err
	}
	return v, nil
}

// PutPairs uploads target id's /verify pairs for run runID: PUT
// /targets/{id}/pairs/{run_id}.
func (c *Client) PutPairs(ctx context.Context, id, runID string, u wire.Upload) (wire.Uploaded, error) {
	var v wire.Uploaded
	path := "/targets/" + url.PathEscape(id) + "/pairs/" + url.PathEscape(runID)
	if _, err := c.do(ctx, http.MethodPut, path, nil, u, statusCreated, &v, &v.BootID); err != nil {
		return wire.Uploaded{}, err
	}
	return v, nil
}

// Start starts run runID: POST /runs/{run_id}/start. A response for
// another run_id is an error.
func (c *Client) Start(ctx context.Context, runID string, s wire.Start) (wire.Started, error) {
	var v wire.Started
	path := "/runs/" + url.PathEscape(runID) + "/start"
	_, err := c.do(ctx, http.MethodPost, path, nil, s, statusOK, &v, &v.BootID)
	if err == nil && v.RunID != runID {
		err = fmt.Errorf("loadgen/client: POST %s: answered for run_id %q", path, v.RunID)
	}
	if err != nil {
		return wire.Started{}, err
	}
	return v, nil
}

// Run returns run runID: GET /runs/{run_id}. A response for another
// run_id is an error.
func (c *Client) Run(ctx context.Context, runID string) (wire.Run, error) {
	return c.run(ctx, http.MethodGet, "/runs/"+url.PathEscape(runID), runID)
}

// Cancel cancels run runID and returns it: POST /runs/{run_id}/cancel.
// A response for another run_id is an error.
func (c *Client) Cancel(ctx context.Context, runID string) (wire.Run, error) {
	return c.run(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/cancel", runID)
}

func (c *Client) run(ctx context.Context, method, path, runID string) (wire.Run, error) {
	var v wire.Run
	_, err := c.do(ctx, method, path, nil, nil, statusOK, &v, &v.BootID)
	if err == nil && v.RunID != runID {
		err = fmt.Errorf("loadgen/client: %s %s: answered for run_id %q", method, path, v.RunID)
	}
	if err != nil {
		return wire.Run{}, err
	}
	return v, nil
}

// Canary runs the policy canary: POST /canary.
func (c *Client) Canary(ctx context.Context) (wire.Canary, error) {
	var v wire.Canary
	if _, err := c.do(ctx, http.MethodPost, "/canary", nil, nil, statusOK, &v, &v.BootID); err != nil {
		return wire.Canary{}, err
	}
	return v, nil
}

// do sends one request to path, with query and with in as compact JSON
// unless in is nil, and checks its response (check), decoding the body
// into out, whose boot_id field is boot. It returns the status.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in any, ok []int, out any, boot *string) (int, error) {
	status, err := c.roundTrip(ctx, method, path, query, in, ok, out, boot)
	if err != nil {
		return 0, fmt.Errorf("loadgen/client: %s %s: %w", method, path, err)
	}
	return status, nil
}

func (c *Client) roundTrip(ctx context.Context, method, path string, query url.Values, in any, ok []int, out any, boot *string) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	u := c.base + path
	if query != nil {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, fmt.Errorf("reading the body: %w", err)
	}
	if len(b) > maxResponseBody {
		return 0, fmt.Errorf("body over %d bytes", maxResponseBody)
	}
	return resp.StatusCode, c.check(resp, b, ok, out, boot)
}

// check checks a response whose body is b, as the package doc
// describes, decoding b into out, whose boot_id field is boot, when the
// status is one of ok.
func (c *Client) check(resp *http.Response, b []byte, ok []int, out any, boot *string) error {
	header, headerErr := bootHeader(resp.Header)
	if !slices.Contains(ok, resp.StatusCode) {
		ae := &APIError{Status: resp.StatusCode}
		if headerErr != nil {
			return ae
		}
		ae.BootID = header
		var we wire.Error
		if isJSON(resp.Header) && decode(b, &we) == nil && we.BootID == header {
			ae.Message = we.Error
		}
		if err := c.expected(header); err != nil {
			return fmt.Errorf("%w: %w", err, ae)
		}
		return ae
	}
	if headerErr != nil {
		return headerErr
	}
	if err := c.expected(header); err != nil {
		return err
	}
	if !isJSON(resp.Header) {
		return fmt.Errorf("Content-Type %q, want application/json", resp.Header.Values("Content-Type"))
	}
	if err := decode(b, out); err != nil {
		return fmt.Errorf("decoding the body: %w", err)
	}
	if *boot != header {
		return fmt.Errorf("body boot_id %q, but %s %q", *boot, wire.BootIDHeader, header)
	}
	return nil
}

// bootHeader returns h's one, non-empty, wire.BootIDHeader value.
func bootHeader(h http.Header) (string, error) {
	vs := h.Values(wire.BootIDHeader)
	if len(vs) != 1 || vs[0] == "" {
		return "", fmt.Errorf("%s %q, want one boot_id", wire.BootIDHeader, vs)
	}
	return vs[0], nil
}

// isJSON reports whether h's one Content-Type is application/json, as
// loadgen sends it.
func isJSON(h http.Header) bool {
	vs := h.Values("Content-Type")
	return len(vs) == 1 && vs[0] == "application/json"
}

// decode decodes b, one JSON value, into v, as loadgen decodes a
// request body: it refuses a field v lacks and anything but white space
// after the value.
func decode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("data after the JSON value")
		}
		return err
	}
	return nil
}
