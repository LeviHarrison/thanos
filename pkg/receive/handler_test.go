// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package receive

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/thanos-io/thanos/pkg/errutil"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"github.com/thanos-io/thanos/pkg/store/storepb/prompb"
	"github.com/thanos-io/thanos/pkg/testutil"
)

func TestDetermineWriteErrorCause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		threshold int
		exp       error
	}{
		{
			name: "nil",
		},
		{
			name: "nil multierror",
			err:  errutil.NonNilMultiError([]error{}),
		},
		{
			name:      "matching simple",
			err:       errConflict,
			threshold: 1,
			exp:       errConflict,
		},
		{
			name: "non-matching multierror",
			err: errutil.NonNilMultiError([]error{
				errors.New("foo"),
				errors.New("bar"),
			}),
			exp: errors.New("2 errors: foo; bar"),
		},
		{
			name: "nested non-matching multierror",
			err: errors.Wrap(errutil.NonNilMultiError([]error{
				errors.New("foo"),
				errors.New("bar"),
			}), "baz"),
			threshold: 1,
			exp:       errors.New("baz: 2 errors: foo; bar"),
		},
		{
			name: "deep nested non-matching multierror",
			err: errors.Wrap(errutil.NonNilMultiError([]error{
				errors.New("foo"),
				errutil.NonNilMultiError([]error{
					errors.New("bar"),
					errors.New("qux"),
				}),
			}), "baz"),
			threshold: 1,
			exp:       errors.New("baz: 2 errors: foo; 2 errors: bar; qux"),
		},
		{
			name: "matching multierror",
			err: errutil.NonNilMultiError([]error{
				storage.ErrOutOfOrderSample,
				errors.New("foo"),
				errors.New("bar"),
			}),
			threshold: 1,
			exp:       errConflict,
		},
		{
			name: "matching but below threshold multierror",
			err: errutil.NonNilMultiError([]error{
				storage.ErrOutOfOrderSample,
				errors.New("foo"),
				errors.New("bar"),
			}),
			threshold: 2,
			exp:       errors.New("3 errors: out of order sample; foo; bar"),
		},
		{
			name: "matching multierror many",
			err: errutil.NonNilMultiError([]error{
				storage.ErrOutOfOrderSample,
				errConflict,
				status.Error(codes.AlreadyExists, "conflict"),
				errors.New("foo"),
				errors.New("bar"),
			}),
			threshold: 1,
			exp:       errConflict,
		},
		{
			name: "matching multierror many, one above threshold",
			err: errutil.NonNilMultiError([]error{
				storage.ErrOutOfOrderSample,
				errConflict,
				tsdb.ErrNotReady,
				tsdb.ErrNotReady,
				tsdb.ErrNotReady,
				errors.New("foo"),
			}),
			threshold: 2,
			exp:       errNotReady,
		},
		{
			name: "matching multierror many, both above threshold, conflict have precedence",
			err: errutil.NonNilMultiError([]error{
				storage.ErrOutOfOrderSample,
				errConflict,
				tsdb.ErrNotReady,
				tsdb.ErrNotReady,
				tsdb.ErrNotReady,
				status.Error(codes.AlreadyExists, "conflict"),
				errors.New("foo"),
			}),
			threshold: 2,
			exp:       errConflict,
		},
		{
			name: "nested matching multierror",
			err: errors.Wrap(errors.Wrap(errutil.NonNilMultiError([]error{
				storage.ErrOutOfOrderSample,
				errors.New("foo"),
				errors.New("bar"),
			}), "baz"), "qux"),
			threshold: 1,
			exp:       errConflict,
		},
		{
			name: "deep nested matching multierror",
			err: errors.Wrap(errutil.NonNilMultiError([]error{
				errutil.NonNilMultiError([]error{
					errors.New("qux"),
					status.Error(codes.AlreadyExists, "conflict"),
					status.Error(codes.AlreadyExists, "conflict"),
				}),
				errors.New("foo"),
				errors.New("bar"),
			}), "baz"),
			threshold: 1,
			exp:       errors.New("baz: 3 errors: 3 errors: qux; rpc error: code = AlreadyExists desc = conflict; rpc error: code = AlreadyExists desc = conflict; foo; bar"),
		},
	} {
		err := determineWriteErrorCause(tc.err, tc.threshold)
		if tc.exp != nil {
			testutil.NotOk(t, err)
			testutil.Equals(t, tc.exp.Error(), err.Error())
			continue
		}
		testutil.Ok(t, err)
	}
}

func newHandlerHashring(appendables []*fakeAppendable, replicationFactor uint64) ([]*Handler, Hashring) {
	cfg := []HashringConfig{
		{
			Hashring: "test",
		},
	}
	var handlers []*Handler
	// create a fake peer group where we manually fill the cache with fake addresses pointed to our handlers
	// This removes the network from the tests and creates a more consistent testing harness.
	peers := &peerGroup{
		dialOpts: nil,
		m:        sync.RWMutex{},
		cache:    map[string]storepb.WriteableStoreClient{},
		dialer: func(context.Context, string, ...grpc.DialOption) (*grpc.ClientConn, error) {
			// dialer should never be called since we are creating fake clients with fake addresses
			// this protects against some leaking test that may attempt to dial random IP addresses
			// which may pose a security risk.
			return nil, errors.New("unexpected dial called in testing")
		},
	}

	for i := range appendables {
		h := NewHandler(nil, &Options{
			TenantHeader:      DefaultTenantHeader,
			ReplicaHeader:     DefaultReplicaHeader,
			ReplicationFactor: replicationFactor,
			ForwardTimeout:    5 * time.Second,
			Writer:            NewWriter(log.NewNopLogger(), newFakeTenantAppendable(appendables[i])),
		})
		handlers = append(handlers, h)
		h.peers = peers
		addr := randomAddr()
		h.options.Endpoint = addr
		cfg[0].Endpoints = append(cfg[0].Endpoints, h.options.Endpoint)
		peers.cache[addr] = &fakeRemoteWriteGRPCServer{h: h}
	}
	hashring := newMultiHashring(cfg)
	for _, h := range handlers {
		h.Hashring(hashring)
	}
	return handlers, hashring
}

func TestReceiveQuorum(t *testing.T) {
	appenderErrFn := func() error { return errors.New("failed to get appender") }
	conflictErrFn := func() error { return storage.ErrOutOfBounds }
	commitErrFn := func() error { return errors.New("failed to commit") }
	wreq1 := &prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: []labelpb.ZLabel{
					{
						Name:  "foo",
						Value: "bar",
					},
				},
				Samples: []prompb.Sample{
					{
						Value:     1,
						Timestamp: 1,
					},
					{
						Value:     2,
						Timestamp: 2,
					},
					{
						Value:     3,
						Timestamp: 3,
					},
				},
			},
		},
	}
	for _, tc := range []struct {
		name              string
		status            int
		replicationFactor uint64
		wreq              *prompb.WriteRequest
		appendables       []*fakeAppendable
	}{
		{
			name:              "size 1 success",
			status:            http.StatusOK,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 1 commit error",
			status:            http.StatusInternalServerError,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
			},
		},
		{
			name:              "size 1 conflict",
			status:            http.StatusConflict,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
			},
		},
		{
			name:              "size 2 success",
			status:            http.StatusOK,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 success",
			status:            http.StatusOK,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 success with replication",
			status:            http.StatusOK,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 commit error",
			status:            http.StatusInternalServerError,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
			},
		},
		{
			name:              "size 3 commit error with replication",
			status:            http.StatusInternalServerError,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
			},
		},
		{
			name:              "size 3 appender error with replication",
			status:            http.StatusInternalServerError,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender:    newFakeAppender(nil, nil, nil),
					appenderErr: appenderErrFn,
				},
				{
					appender:    newFakeAppender(nil, nil, nil),
					appenderErr: appenderErrFn,
				},
				{
					appender:    newFakeAppender(nil, nil, nil),
					appenderErr: appenderErrFn,
				},
			},
		},
		{
			name:              "size 3 conflict with replication",
			status:            http.StatusConflict,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
			},
		},
		{
			name:              "size 3 conflict and commit error with replication",
			status:            http.StatusConflict,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(conflictErrFn, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, commitErrFn, nil),
				},
			},
		},
		{
			name:              "size 3 with replication and one faulty",
			status:            http.StatusOK,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(cycleErrors([]error{storage.ErrOutOfBounds, storage.ErrOutOfOrderSample, storage.ErrDuplicateSampleForTimestamp}), nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 with replication and one commit error",
			status:            http.StatusOK,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 with replication and two conflicts",
			status:            http.StatusConflict,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(cycleErrors([]error{storage.ErrOutOfBounds, storage.ErrOutOfOrderSample, storage.ErrDuplicateSampleForTimestamp}), nil, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 with replication one conflict and one commit error",
			status:            http.StatusInternalServerError,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(cycleErrors([]error{storage.ErrOutOfBounds, storage.ErrOutOfOrderSample, storage.ErrDuplicateSampleForTimestamp}), nil, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 with replication two commit errors",
			status:            http.StatusInternalServerError,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handlers, hashring := newHandlerHashring(tc.appendables, tc.replicationFactor)
			tenant := "test"
			// Test from the point of view of every node
			// so that we know status code does not depend
			// on which node is erroring and which node is receiving.
			for i, handler := range handlers {
				// Test that the correct status is returned.
				rec, err := makeRequest(handler, tenant, tc.wreq)
				if err != nil {
					t.Fatalf("handler %d: unexpectedly failed making HTTP request: %v", tc.status, err)
				}
				if rec.Code != tc.status {
					t.Errorf("handler %d: got unexpected HTTP status code: expected %d, got %d; body: %s", i, tc.status, rec.Code, rec.Body.String())
				}
			}
			// Test that each time series is stored
			// the correct amount of times in each fake DB.
			for _, ts := range tc.wreq.Timeseries {
				lset := make(labels.Labels, len(ts.Labels))
				for j := range ts.Labels {
					lset[j] = labels.Label{
						Name:  ts.Labels[j].Name,
						Value: ts.Labels[j].Value,
					}
				}
				for j, a := range tc.appendables {
					var expectedMin int
					n := a.appender.(*fakeAppender).Get(lset)
					got := uint64(len(n))
					if a.appenderErr == nil && endpointHit(t, hashring, tc.replicationFactor, handlers[j].options.Endpoint, tenant, &ts) {
						// We have len(handlers) copies of each sample because the test case
						// is run once for each handler and they all use the same appender.
						expectedMin = int((tc.replicationFactor/2)+1) * len(ts.Samples)
					}
					if uint64(expectedMin) > got {
						t.Errorf("handler: %d, labels %q: expected minimum of %d samples, got %d", j, lset.String(), expectedMin, got)
					}
				}
			}
		})
	}
}

func TestReceiveWithConsistencyDelay(t *testing.T) {
	appenderErrFn := func() error { return errors.New("failed to get appender") }
	conflictErrFn := func() error { return storage.ErrOutOfBounds }
	commitErrFn := func() error { return errors.New("failed to commit") }
	wreq1 := &prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: []labelpb.ZLabel{
					{
						Name:  "foo",
						Value: "bar",
					},
				},
				Samples: []prompb.Sample{
					{
						Value:     1,
						Timestamp: 1,
					},
					{
						Value:     2,
						Timestamp: 2,
					},
					{
						Value:     3,
						Timestamp: 3,
					},
				},
			},
		},
	}
	for _, tc := range []struct {
		name              string
		status            int
		replicationFactor uint64
		wreq              *prompb.WriteRequest
		appendables       []*fakeAppendable
	}{
		{
			name:              "size 1 success",
			status:            http.StatusOK,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 1 commit error",
			status:            http.StatusInternalServerError,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
			},
		},
		{
			name:              "size 1 conflict",
			status:            http.StatusConflict,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
			},
		},
		{
			name:              "size 2 success",
			status:            http.StatusOK,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 success",
			status:            http.StatusOK,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 success with replication",
			status:            http.StatusOK,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 commit error",
			status:            http.StatusInternalServerError,
			replicationFactor: 1,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
			},
		},
		{
			name:              "size 3 commit error with replication",
			status:            http.StatusInternalServerError,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
			},
		},
		{
			name:              "size 3 appender error with replication",
			status:            http.StatusInternalServerError,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender:    newFakeAppender(nil, nil, nil),
					appenderErr: appenderErrFn,
				},
				{
					appender:    newFakeAppender(nil, nil, nil),
					appenderErr: appenderErrFn,
				},
				{
					appender:    newFakeAppender(nil, nil, nil),
					appenderErr: appenderErrFn,
				},
			},
		},
		{
			name:              "size 3 conflict with replication",
			status:            http.StatusConflict,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
			},
		},
		{
			name:              "size 3 conflict and commit error with replication",
			status:            http.StatusConflict,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(conflictErrFn, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, commitErrFn, nil),
				},
			},
		},
		{
			name:              "size 3 with replication and one faulty",
			status:            http.StatusOK,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(cycleErrors([]error{storage.ErrOutOfBounds, storage.ErrOutOfOrderSample, storage.ErrDuplicateSampleForTimestamp}), nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 with replication and one commit error",
			status:            http.StatusOK,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 with replication and two conflicts",
			status:            http.StatusConflict,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(cycleErrors([]error{storage.ErrOutOfBounds, storage.ErrOutOfOrderSample, storage.ErrDuplicateSampleForTimestamp}), nil, nil),
				},
				{
					appender: newFakeAppender(conflictErrFn, nil, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 with replication one conflict and one commit error",
			status:            http.StatusInternalServerError,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(cycleErrors([]error{storage.ErrOutOfBounds, storage.ErrOutOfOrderSample, storage.ErrDuplicateSampleForTimestamp}), nil, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
		{
			name:              "size 3 with replication two commit errors",
			status:            http.StatusInternalServerError,
			replicationFactor: 3,
			wreq:              wreq1,
			appendables: []*fakeAppendable{
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, commitErrFn, nil),
				},
				{
					appender: newFakeAppender(nil, nil, nil),
				},
			},
		},
	} {
		// Run the quorum tests with consistency delay, which should allow us
		// to see all requests completing all the time, since we're using local
		// network we are not expecting anything to go wrong with these.
		t.Run(tc.name, func(t *testing.T) {
			handlers, hashring := newHandlerHashring(tc.appendables, tc.replicationFactor)
			tenant := "test"
			// Test from the point of view of every node
			// so that we know status code does not depend
			// on which node is erroring and which node is receiving.
			for i, handler := range handlers {
				// Test that the correct status is returned.
				rec, err := makeRequest(handler, tenant, tc.wreq)
				if err != nil {
					t.Fatalf("handler %d: unexpectedly failed making HTTP request: %v", tc.status, err)
				}
				if rec.Code != tc.status {
					t.Errorf("handler %d: got unexpected HTTP status code: expected %d, got %d; body: %s", i, tc.status, rec.Code, rec.Body.String())
				}
			}

			time.Sleep(50 * time.Millisecond)

			// Test that each time series is stored
			// the correct amount of times in each fake DB.
			for _, ts := range tc.wreq.Timeseries {
				lset := make(labels.Labels, len(ts.Labels))
				for j := range ts.Labels {
					lset[j] = labels.Label{
						Name:  ts.Labels[j].Name,
						Value: ts.Labels[j].Value,
					}
				}
				for j, a := range tc.appendables {
					var expected int
					n := a.appender.(*fakeAppender).Get(lset)
					got := uint64(len(n))
					if a.appenderErr == nil && endpointHit(t, hashring, tc.replicationFactor, handlers[j].options.Endpoint, tenant, &ts) {
						// We have len(handlers) copies of each sample because the test case
						// is run once for each handler and they all use the same appender.
						expected = len(handlers) * len(ts.Samples)
					}
					if uint64(expected) != got {
						t.Errorf("handler: %d, labels %q: expected %d samples, got %d", j, lset.String(), expected, got)
					}
				}
			}
		})
	}
}

// endpointHit is a helper to determine if a given endpoint in a hashring would be selected
// for a given time series, tenant, and replication factor.
func endpointHit(t *testing.T, h Hashring, rf uint64, endpoint, tenant string, timeSeries *prompb.TimeSeries) bool {
	for i := uint64(0); i < rf; i++ {
		e, err := h.GetN(tenant, timeSeries, i)
		if err != nil {
			t.Fatalf("got unexpected error querying hashring: %v", err)
		}
		if e == endpoint {
			return true
		}
	}
	return false
}

// cycleErrors returns an error generator that cycles through every given error.
func cycleErrors(errs []error) func() error {
	var mu sync.Mutex
	var i int
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		err := errs[i]
		i++
		if i >= len(errs) {
			i = 0
		}
		return err
	}
}

// makeRequest is a helper to make a correct request against a remote write endpoint given a request.
func makeRequest(h *Handler, tenant string, wreq *prompb.WriteRequest) (*httptest.ResponseRecorder, error) {
	buf, err := proto.Marshal(wreq)
	if err != nil {
		return nil, errors.Wrap(err, "marshal request")
	}
	req, err := http.NewRequest("POST", h.options.Endpoint, bytes.NewBuffer(snappy.Encode(nil, buf)))
	if err != nil {
		return nil, errors.Wrap(err, "create request")
	}
	req.Header.Add(h.options.TenantHeader, tenant)

	rec := httptest.NewRecorder()
	h.receiveHTTP(rec, req)
	rec.Flush()

	return rec, nil
}

func randomAddr() string {
	return fmt.Sprintf("http://%d.%d.%d.%d:%d", rand.Intn(256), rand.Intn(256), rand.Intn(256), rand.Intn(256), rand.Intn(35000)+30000)
}

type fakeRemoteWriteGRPCServer struct {
	h storepb.WriteableStoreServer
}

func (f *fakeRemoteWriteGRPCServer) RemoteWrite(ctx context.Context, in *storepb.WriteRequest, opts ...grpc.CallOption) (*storepb.WriteResponse, error) {
	return f.h.RemoteWrite(ctx, in)
}
