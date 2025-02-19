/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package prober

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"knative.dev/networking/pkg/http/header"
	"knative.dev/pkg/network"
)

const (
	systemName             = "test-server"
	unexpectedProbeMessage = "unexpected probe header value: whatever"
	probeInterval          = 10 * time.Millisecond
	probeTimeout           = 200 * time.Millisecond
)

func probeServeFunc(w http.ResponseWriter, r *http.Request) {
	s := r.Header.Get(header.ProbeKey)
	switch s {
	case "":
		// No header.
		w.WriteHeader(http.StatusNotFound)
	case systemName:
		// Expected header value.
		w.Write([]byte(systemName))
	default:
		// Unexpected header value.
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(unexpectedProbeMessage))
	}
}

func TestDoServing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(probeServeFunc))
	defer ts.Close()
	tests := []struct {
		name        string
		headerValue string
		want        bool
		expErr      bool
	}{{
		name:        "ok",
		headerValue: systemName,
		want:        true,
		expErr:      false,
	}, {
		name:        "wrong system",
		headerValue: "bells-and-whistles",
		want:        false,
		expErr:      true,
	}, {
		name:        "no header",
		headerValue: "",
		want:        false,
		expErr:      true,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Do(context.Background(), network.NewProberTransport(), ts.URL, WithHeader(header.ProbeKey, test.headerValue), ExpectsBody(systemName), ExpectsStatusCodes([]int{http.StatusOK}))
			if want := test.want; got != want {
				t.Errorf("Got = %v, want: %v", got, want)
			}
			if err != nil && !test.expErr {
				t.Errorf("Do() = %v, no error expected", err)
			}
			if err == nil && test.expErr {
				t.Errorf("Do() = nil, expected an error")
			}
		})
	}
}

func TestBlackHole(t *testing.T) {
	transport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: 10 * time.Millisecond,
		}).Dial,
	}
	got, err := Do(context.Background(), transport, "http://gone.fishing.svc.custer.local:8080", ExpectsStatusCodes([]int{http.StatusOK}))
	if want := false; got != want {
		t.Errorf("Got = %v, want: %v", got, want)
	}
	if err == nil {
		t.Error("Do did not return an error")
	}
}

func TestBadURL(t *testing.T) {
	_, err := Do(context.Background(), network.NewProberTransport(), ":foo", ExpectsStatusCodes([]int{http.StatusOK}))
	if err == nil {
		t.Error("Do did not return an error")
	}
	t.Log("For the curious the error was:", err)
}

func TestDoAsync(t *testing.T) {
	// This replicates the TestDo.
	ts := httptest.NewServer(http.HandlerFunc(probeServeFunc))
	defer ts.Close()

	wch := make(chan interface{})
	defer close(wch)
	tests := []struct {
		name        string
		headerValue string
		cb          Done
	}{{
		name:        "ok",
		headerValue: systemName,
		cb: func(arg interface{}, ret bool, err error) {
			defer func() {
				wch <- 42
			}()
			if got, want := arg.(string), "ok"; got != want {
				t.Errorf("arg = %s, want: %s", got, want)
			}
			if !ret {
				t.Error("result was false")
			}
		},
	}, {
		name:        "wrong system",
		headerValue: "bells-and-whistles",
		cb: func(arg interface{}, ret bool, err error) {
			defer func() {
				wch <- 1984
			}()
			if ret {
				t.Error("result was true")
			}
		},
	}, {
		name:        "no header",
		headerValue: "",
		cb: func(arg interface{}, ret bool, err error) {
			defer func() {
				wch <- 2006
			}()
			if ret {
				t.Error("result was true")
			}
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := New(test.cb, network.NewProberTransport())
			m.Offer(context.Background(), ts.URL, test.name, probeInterval, probeTimeout, WithHeader(header.ProbeKey, test.headerValue), ExpectsBody(test.headerValue), ExpectsStatusCodes([]int{http.StatusOK}))
			<-wch
		})
	}
}

type thirdTimesTheCharmProber struct {
	calls int
}

func (t *thirdTimesTheCharmProber) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.calls++
	if t.calls < 3 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(unexpectedProbeMessage))
		return
	}
	w.Write([]byte(systemName))
}

func TestDoAsyncRepeat(t *testing.T) {
	c := &thirdTimesTheCharmProber{}
	ts := httptest.NewServer(c)
	defer ts.Close()

	wch := make(chan interface{})
	defer close(wch)
	cb := func(arg interface{}, done bool, err error) {
		if !done {
			t.Error("done was false")
		}
		if err != nil {
			t.Error("Unexpected error =", err)
		}
		wch <- arg
	}
	m := New(cb, network.NewProberTransport())
	m.Offer(context.Background(), ts.URL, 42, probeInterval, probeTimeout, WithHeader(header.ProbeKey, systemName), ExpectsBody(systemName), ExpectsStatusCodes([]int{http.StatusOK}))
	<-wch
	if got, want := c.calls, 3; got != want {
		t.Errorf("Probe invocation count = %d, want: %d", got, want)
	}
}

func TestDoAsyncTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	wch := make(chan interface{})
	defer close(wch)

	cb := func(arg interface{}, done bool, err error) {
		if done {
			t.Errorf("done was true")
		}
		if !errors.Is(err, wait.ErrWaitTimeout) {
			t.Error("Unexpected error =", err)
		}
		wch <- arg
	}
	m := New(cb, network.NewProberTransport())
	m.Offer(context.Background(), ts.URL, 2009, probeInterval, probeTimeout, ExpectsStatusCodes([]int{http.StatusOK}))
	<-wch
}

func TestAsyncMultiple(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(probeServeFunc))
	defer ts.Close()

	wch := make(chan interface{})
	defer close(wch)
	cb := func(arg interface{}, done bool, err error) {
		<-wch
		wch <- 2006
	}
	m := New(cb, network.NewProberTransport())
	if !m.Offer(context.Background(), ts.URL, 1984, probeInterval, probeTimeout, ExpectsStatusCodes([]int{http.StatusOK})) {
		t.Error("First call to offer returned false")
	}
	if m.Offer(context.Background(), ts.URL, 1982, probeInterval, probeTimeout, ExpectsStatusCodes([]int{http.StatusOK})) {
		t.Error("Second call to offer returned true")
	}
	if got, want := m.len(), 1; got != want {
		t.Errorf("Number of queued items = %d, want: %d", got, want)
	}
	// Make sure we terminate the first probe.
	wch <- 2009
	<-wch

	wait.PollImmediate(probeInterval, probeTimeout, func() (bool, error) {
		return m.len() == 0, nil
	})
	if got, want := m.len(), 0; got != want {
		t.Errorf("Number of queued items = %d, want: %d", got, want)
	}
}

func TestWithPathOption(t *testing.T) {
	const path = "/correct/probe/path/"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Got: %s, Want: %s\n", r.URL.Path, path)
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	tests := []struct {
		name    string
		options []interface{}
	}{{
		name:    "no path",
		options: []interface{}{ExpectsStatusCodes([]int{http.StatusNotFound})},
	}, {
		name:    "expected path",
		options: []interface{}{WithPath(path), ExpectsStatusCodes([]int{http.StatusOK})},
	}, {
		name:    "wrong path",
		options: []interface{}{WithPath("/wrong/path"), ExpectsStatusCodes([]int{http.StatusNotFound})},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if ok, _ := Do(context.Background(), network.AutoTransport, ts.URL, test.options...); !ok {
				t.Error("Unexpected probe failure")
			}
		})
	}
}

func TestWithHostOption(t *testing.T) {
	host := "foobar.com"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Want: %s, Got: %s\n", host, r.Host)
		if r.Host != host {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	tests := []struct {
		name    string
		options []interface{}
	}{{
		name:    "no hosts",
		options: []interface{}{ExpectsStatusCodes([]int{http.StatusNotFound})},
	}, {
		name:    "expected host",
		options: []interface{}{WithHost(host), ExpectsStatusCodes([]int{http.StatusOK})},
	}, {
		name:    "wrong host",
		options: []interface{}{WithHost("nope.com"), ExpectsStatusCodes([]int{http.StatusNotFound})},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if ok, _ := Do(context.Background(), network.AutoTransport, ts.URL, test.options...); !ok {
				t.Error("Unexpected probe result")
			}
		})
	}
}

func TestExpectsHeaderOption(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Foo", "Bar")
	}))
	defer ts.Close()

	tests := []struct {
		name    string
		options []interface{}
		success bool
		expErr  bool
	}{{
		name:    "header is present",
		options: []interface{}{ExpectsHeader("Foo", "Bar"), ExpectsStatusCodes([]int{http.StatusOK})},
		success: true,
	}, {
		name:    "header is absent",
		options: []interface{}{ExpectsHeader("Baz", "Nope"), ExpectsStatusCodes([]int{http.StatusOK})},
		success: false,
		expErr:  true,
	}, {
		name:    "header value doesn't match",
		options: []interface{}{ExpectsHeader("Foo", "Baz"), ExpectsStatusCodes([]int{http.StatusOK})},
		success: false,
		expErr:  true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ok, err := Do(context.Background(), network.AutoTransport, ts.URL, test.options...)
			if ok != test.success {
				t.Errorf("unexpected probe result: want: %v, got: %v", test.success, ok)
			}
			if err != nil && !test.expErr {
				t.Errorf("Do() = %v, no error expected", err)
			}
			if err == nil && test.expErr {
				t.Errorf("Do() = nil, expected an error")
			}
		})
	}
}

func (m *Manager) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys.Len()
}
