package aws_test

import (
	"bufio"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-aws/aws"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// host is a small stand-in for Mamela's host, for what the conformance suite cannot reach: an
// explicit operation on a world that differs from the declaration (an instance of another type, a
// log group that keeps its logs longer), and the adopted world right after adopting. It runs the
// provider in the test's process, sends its requests to the fake, and holds them to what Mamela's
// host does: only declared calls the shipped catalogs classify; during observe, lookup and prepare,
// only reads; during apply, only the change's approved requests, each bound to the change's
// objects (bind) and within the catalog's values. It injects one fault at the at-th changing
// request: not sent, or sent with its answer lost.
type host struct {
	t     *testing.T
	api   http.Handler
	cats  map[string]protocol.Catalog
	calls []protocol.Call
	in    io.Writer
	out   *bufio.Reader
	next  int64

	change *protocol.Change // being applied
	at     int
	kind   string // faultNotSent or faultLost
	n      int    // changing requests sent so far
	hit    bool
	sent   []string // the changing requests that reached the API
}

const (
	faultNotSent = "not-sent"
	faultLost    = "lost"
)

func newHost(t *testing.T, api http.Handler) *host {
	t.Helper()
	toProvider, in := io.Pipe()
	out, fromProvider := io.Pipe()
	go func() {
		sdk.ServeIO(aws.Provider, toProvider, fromProvider)
		fromProvider.Close()
	}()
	t.Cleanup(func() { in.Close() })
	m := aws.Provider.Manifest()
	calls := slices.Clone(m.ObserveCalls)
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			calls = append(calls, o.Calls...)
		}
	}
	return &host{t: t, api: api, cats: shipped(t), calls: calls, in: in, out: bufio.NewReaderSize(out, 1<<20), at: -1}
}

// call calls a method of the provider, serving its host/requests.
func (h *host) call(method string, params, result any) error {
	h.t.Helper()
	h.next++
	id := h.next
	p, _ := json.Marshal(params)
	h.write(protocol.Message{JSONRPC: "2.0", ID: id, Method: method, Params: p})
	for {
		line, err := h.out.ReadBytes('\n')
		if err != nil {
			h.t.Fatalf("%s: the provider: %v", method, err)
		}
		var m protocol.Message
		if err := json.Unmarshal(line, &m); err != nil {
			h.t.Fatalf("%s: %v", method, err)
		}
		switch {
		case m.Method == protocol.MethodHostRequest:
			var r protocol.HostRequest
			json.Unmarshal(m.Params, &r)
			resp, perr := h.request(method, r)
			reply := protocol.Message{JSONRPC: "2.0", ID: m.ID}
			if perr != nil {
				reply.Error = perr
			} else {
				reply.Result, _ = json.Marshal(resp)
			}
			h.write(reply)
		case m.Method != "":
			h.t.Fatalf("%s: the provider asks %s", method, m.Method)
		case m.ID == id:
			if m.Error != nil {
				return m.Error
			}
			if result != nil {
				return json.Unmarshal(m.Result, result)
			}
			return nil
		}
	}
}

func (h *host) write(m protocol.Message) {
	b, _ := json.Marshal(m, json.Deterministic(true))
	h.in.Write(append(b, '\n'))
}

// request checks and sends one request, as the host would.
func (h *host) request(method string, r protocol.HostRequest) (protocol.HostResponse, *protocol.Error) {
	refuse := func(format string, args ...any) (protocol.HostResponse, *protocol.Error) {
		h.t.Errorf("refused during %s: "+format, append([]any{method}, args...)...)
		return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeRefused, Message: fmt.Sprintf(format, args...)}
	}
	params, err := protocol.JSONParams(r.JSON)
	if err != nil {
		return refuse("%v", err)
	}
	for _, part := range []map[string][]string{r.Query, r.Form, r.Header} {
		maps.Copy(params, part)
	}
	call, _, ok := protocol.FindCall(h.calls, r.API, r.Method, r.Path, params, true)
	if !ok {
		return refuse("%s %s %v is not a declared call", r.Method, r.Path, params)
	}
	e, ok := h.cats[call.API].LookupCall(call)
	if !ok {
		return refuse("%v is not in the %s catalog", call.Fixed, call.API)
	}
	for k := range params {
		if e.In(k) == "" {
			return refuse("%v: parameter %s is not in its catalog entry", call.Fixed, k)
		}
	}
	if e.Effect != protocol.EffectRead {
		if method != protocol.MethodApply || h.change == nil {
			return refuse("%v changes (%s), and %s only reads", call.Fixed, e.Effect, method)
		}
		approved := slices.ContainsFunc(h.change.Requests, func(a protocol.Approved) bool {
			return a.API == r.API && a.Method == r.Method && a.Path == r.Path && maps.EqualFunc(a.Params, params, slices.Equal)
		})
		if !approved {
			return refuse("%v %v is not one of the change's approved requests", call.Fixed, params)
		}
		for _, b := range e.Bind {
			if v := params[b]; len(v) != 1 || protocol.ObjectText(e.In(b), v[0]) != h.change.IDs[b] {
				return refuse("%s=%v is not the change's %s %q", b, v, b, h.change.IDs[b])
			}
		}
		if err := e.CheckLimits(params, nil); err != nil {
			return refuse("%v", err)
		}
		i := h.n
		h.n++
		if i == h.at {
			h.hit = true
			if h.kind == faultNotSent {
				return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeNotSent, Message: "injected: connection refused"}
			}
			h.send(r)
			h.sent = append(h.sent, describe(call))
			return protocol.HostResponse{}, &protocol.Error{Code: protocol.CodeUncertain, Message: "injected: the answer was lost"}
		}
		h.sent = append(h.sent, describe(call))
	}
	return h.send(r), nil
}

func describe(c protocol.Call) string { return c.Fixed["Action"] + c.Fixed["X-Amz-Target"] }

// send sends a request to the fake, as the host writes it.
func (h *host) send(r protocol.HostRequest) protocol.HostResponse {
	var req *http.Request
	if len(r.Header["X-Amz-Target"]) == 1 {
		body := "{}"
		if len(r.JSON) > 0 {
			var parts []string
			for _, k := range slices.Sorted(maps.Keys(r.JSON)) {
				name, _ := json.Marshal(k)
				parts = append(parts, string(name)+":"+string(r.JSON[k]))
			}
			body = "{" + strings.Join(parts, ",") + "}"
		}
		req = httptest.NewRequest(r.Method, r.Path, strings.NewReader(body))
		req.Header.Set("X-Amz-Target", r.Header["X-Amz-Target"][0])
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	} else {
		req = httptest.NewRequest(r.Method, r.Path, strings.NewReader(url.Values(r.Form).Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("X-Mamela-Api", r.API)
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	return protocol.HostResponse{Status: rec.Code, Body: rec.Body.String()}
}

func (h *host) observe(desired []protocol.Resource, adopting []protocol.Adopting) []protocol.Observed {
	h.t.Helper()
	var out protocol.ObserveResult
	if err := h.call(protocol.MethodObserve, protocol.ObserveParams{Resources: desired, Adopting: adopting}, &out); err != nil {
		h.t.Fatalf("observe: %v", err)
	}
	return out.Resources
}

func observed(t *testing.T, os []protocol.Observed, id string) protocol.Observed {
	t.Helper()
	i := slices.IndexFunc(os, func(o protocol.Observed) bool { return o.ID == id })
	if i < 0 {
		t.Fatalf("%s is not observed", id)
	}
	return os[i]
}

// diff diffs what is ready (every dependency exists and is Mamela's), as Mamela does.
func (h *host) diff(desired []protocol.Resource, os []protocol.Observed) []protocol.Change {
	h.t.Helper()
	byID := map[string]protocol.Observed{}
	for _, o := range os {
		byID[o.ID] = o
	}
	var ready []protocol.Resource
	deps := map[string]protocol.Observed{}
	for _, d := range desired {
		ok := true
		for _, a := range d.After {
			if o := byID[a]; !o.Exists || !o.Owned {
				ok = false
			}
		}
		if ok {
			ready = append(ready, d)
			for _, a := range d.After {
				deps[a] = byID[a]
			}
		}
	}
	var out protocol.DiffResult
	if err := h.call(protocol.MethodDiff, protocol.DiffParams{Desired: ready, Observed: os, Dependencies: deps}, &out); err != nil {
		h.t.Fatalf("diff: %v", err)
	}
	return out.Changes
}

func (h *host) prepare(op protocol.Op, desired *protocol.Resource, o protocol.Observed) (protocol.Change, error) {
	var out protocol.PrepareResult
	err := h.call(protocol.MethodPrepare, protocol.PrepareParams{Op: op, Desired: desired, Observed: o}, &out)
	return out.Change, err
}

// apply applies a change under its permit, with the fault set for it (at: the index of its
// changing request, -1 for none).
func (h *host) apply(c protocol.Change, at int, kind string) error {
	h.t.Helper()
	p, err := protocol.NewPermit(aws.Provider.Manifest(), c, "op", "sha256:plan", "sha256:providers", "2999-01-01T00:00:00Z")
	if err != nil {
		h.t.Fatalf("permit for %s %s: %v", c.Op, c.Resource, err)
	}
	h.change, h.at, h.kind, h.n, h.hit = &c, at, kind, 0, false
	defer func() { h.change, h.at = nil, -1 }()
	return h.call(protocol.MethodApply, protocol.ApplyParams{Permit: p, Change: c}, nil)
}

func (h *host) lookup(c protocol.Change) protocol.LookupStatus {
	h.t.Helper()
	var out protocol.LookupResult
	if err := h.call(protocol.MethodLookup, protocol.LookupParams{Change: c}, &out); err != nil {
		h.t.Fatalf("lookup of %s %s: %v", c.Op, c.Resource, err)
	}
	return out.Status
}

// converge applies what diff says until it says nothing (and fails if it never does).
func (h *host) converge(desired []protocol.Resource, adopting []protocol.Adopting) {
	h.t.Helper()
	for range 2 + len(desired) {
		cs := h.diff(desired, h.observe(desired, adopting))
		if len(cs) == 0 {
			return
		}
		for _, c := range cs {
			if err := h.apply(c, -1, ""); err != nil {
				h.t.Fatalf("%s %s: %v", c.Op, c.Resource, err)
			}
		}
	}
	h.t.Fatalf("the declaration does not converge: %v", h.diff(desired, h.observe(desired, adopting)))
}

// explicitWithFaults carries out an explicit operation that prepare makes of the observed world,
// once as it should go and, on fresh worlds, with each of its changing requests not sent, or sent
// with its answer lost: lookup never says done of what was not done, nor not done of what took
// effect, and preparing and applying it again finishes it. world makes a fresh world and returns
// the declaration and the host.
func explicitWithFaults(t *testing.T, op protocol.Op, id string, world func(t *testing.T) ([]protocol.Resource, *host), check func(t *testing.T, h *host)) {
	t.Helper()
	run := func(t *testing.T, at int, kind string) {
		desired, h := world(t)
		d := &desired[slices.IndexFunc(desired, func(r protocol.Resource) bool { return r.ID == id })]
		c, err := h.prepare(op, d, observed(t, h.observe(desired, nil), id))
		if err != nil {
			t.Fatalf("prepare %s %s: %v", op, id, err)
		}
		if s := h.lookup(c); s == protocol.LookupDone {
			t.Fatalf("%s %s: lookup says done before anything was sent", op, id)
		}
		err = h.apply(c, at, kind)
		hit := h.hit
		status := h.lookup(c)
		switch {
		case at < 0 && err != nil:
			t.Fatalf("%s %s: %v", op, id, err)
		case at >= 0 && !hit:
			t.Skipf("the change has fewer than %d changing requests", at+1)
		case at >= 0 && err == nil:
			t.Fatalf("%s %s: a request failed (%s at %d), and the apply succeeded", op, id, kind, at)
		case at < 0 && status != protocol.LookupDone && status != protocol.LookupUnknown:
			t.Fatalf("%s %s: done, and lookup says %s", op, id, status)
		case kind == faultNotSent && at == 0 && status == protocol.LookupDone:
			t.Fatalf("%s %s: nothing was sent, and lookup says done", op, id)
		case kind == faultLost && status == protocol.LookupNotDone:
			t.Fatalf("%s %s: a request took effect (its answer was lost), and lookup says %s", op, id, status)
		}
		if at >= 0 {
			// Again, as a person runs the operation again: from what is observed now.
			o := observed(t, h.observe(desired, nil), id)
			if c2, err := h.prepare(op, d, o); err == nil {
				if err := h.apply(c2, -1, ""); err != nil {
					t.Fatalf("%s %s again: %v", op, id, err)
				}
			} else if !strings.Contains(err.Error(), "already") && !strings.Contains(err.Error(), "not shorter") {
				t.Fatalf("prepare %s %s again: %v", op, id, err) // only what is done already is not prepared again
			}
		}
		check(t, h)
	}
	t.Run("as-it-should", func(t *testing.T) { run(t, -1, "") })
	for at := range 3 {
		for _, kind := range []string{faultNotSent, faultLost} {
			t.Run(fmt.Sprintf("%s-at-%d", kind, at), func(t *testing.T) { run(t, at, kind) })
		}
	}
}
