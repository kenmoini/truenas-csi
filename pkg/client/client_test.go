package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// =============================================================================
// Connection Management Tests
// =============================================================================

func TestConnect_Success(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := newTestClient(mock)
	defer client.Close() //nolint:errcheck

	err := client.Connect(testContext(t))
	assertNoError(t, err)
	assertTrue(t, client.Connected())
}

func TestConnect_InvalidURL(t *testing.T) {
	client := New(Config{
		URL:    "ws://invalid-host-that-does-not-exist:9999",
		APIKey: "test-key",
	})
	defer client.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.Connect(ctx)
	assertError(t, err)
	// Connect retries on connection errors (to survive TrueNAS reboots),
	// so an unreachable host will time out waiting for reconnection.
}

func TestConnect_AuthenticationFailed(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()
	mock.SetAuthFailure(true)

	client := newTestClient(mock)
	defer client.Close() //nolint:errcheck

	err := client.Connect(testContext(t))
	assertError(t, err)

	var rpcErr *RPCError
	assertTrue(t, errors.As(err, &rpcErr))
}

func TestConnect_WrongAPIKey(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()
	mock.SetAPIKey("correct-key")

	client := New(Config{
		URL:    mock.URL,
		APIKey: "wrong-key",
	})
	defer client.Close() //nolint:errcheck

	err := client.Connect(testContext(t))
	assertError(t, err)
	assertTrue(t, errors.Is(err, ErrAuthFailed))
}

func TestConnect_AlreadyConnected(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := connectTestClient(t, mock)

	// Second connect should be idempotent
	err := client.Connect(testContext(t))
	assertNoError(t, err)
	assertTrue(t, client.Connected())

	// Should only have connected once
	assertEqual(t, mock.ConnectionCount(), 1)
}

func TestConnect_ContextCanceled(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := newTestClient(mock)
	defer client.Close() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := client.Connect(ctx)
	assertError(t, err)
}

func TestConnect_AfterClose(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := newTestClient(mock)
	err := client.Connect(testContext(t))
	assertNoError(t, err)

	err = client.Close()
	assertNoError(t, err)

	// Connect after close should fail
	err = client.Connect(testContext(t))
	assertTrue(t, errors.Is(err, ErrClosed))
}

func TestClose_NotConnected(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := newTestClient(mock)

	// Close without connecting should be safe
	err := client.Close()
	assertNoError(t, err)
}

func TestClose_WhileConnected(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := connectTestClient(t, mock)

	err := client.Close()
	assertNoError(t, err)
	assertFalse(t, client.Connected())
}

func TestClose_Idempotent(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := newTestClient(mock)
	err := client.Connect(testContext(t))
	assertNoError(t, err)

	// Multiple closes should be safe
	err = client.Close()
	assertNoError(t, err)

	err = client.Close()
	assertNoError(t, err)
}

func TestPing_Success(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := connectTestClient(t, mock)

	err := client.Ping(testContext(t))
	assertNoError(t, err)
}

func TestPing_NotConnected(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := newTestClient(mock)
	defer client.Close() //nolint:errcheck

	err := client.Ping(testContext(t))
	assertTrue(t, errors.Is(err, ErrNotConnected))
}

func TestCall_Success(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	mock.SetResponse("test.method", MockResponse{
		Result: map[string]string{"key": "value"},
	})

	client := connectTestClient(t, mock)

	var result map[string]string
	err := client.Call(testContext(t), "test.method", nil, &result)
	assertNoError(t, err)
	assertEqual(t, result["key"], "value")

	assertRequestMethod(t, mock, "test.method")
}

func TestCall_WithParams(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	mock.SetResponse("test.method", MockResponse{
		Result: "ok",
	})

	client := connectTestClient(t, mock)

	params := []any{"param1", 42}
	var result string
	err := client.Call(testContext(t), "test.method", params, &result)
	assertNoError(t, err)
	assertEqual(t, result, "ok")

	// Verify params were sent
	requests := mock.GetRequestsByMethod("test.method")
	assertLen(t, requests, 1)

	var sentParams []any
	err = json.Unmarshal(requests[0].Params, &sentParams)
	assertNoError(t, err)
	assertEqual(t, len(sentParams), 2)
}

func TestCall_RPCError(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	mock.SetResponse("test.method", MockResponse{
		Error: &RPCError{Code: -1, Message: "test error"},
	})

	client := connectTestClient(t, mock)

	var result any
	err := client.Call(testContext(t), "test.method", nil, &result)
	assertError(t, err)

	var rpcErr *RPCError
	assertTrue(t, errors.As(err, &rpcErr))
	assertEqual(t, rpcErr.Code, -1)
	assertEqual(t, rpcErr.Message, "test error")
}

func TestCall_Timeout(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	// Set a response function that delays
	mock.SetResponseFunc(func(method string, params json.RawMessage) MockResponse {
		time.Sleep(2 * time.Second)
		return MockResponse{Result: "ok"}
	})

	client := connectTestClient(t, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var result any
	err := client.Call(ctx, "test.slow", nil, &result)
	assertError(t, err)
	assertTrue(t, errors.Is(err, context.DeadlineExceeded))
}

func TestCall_NotConnected(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := newTestClient(mock)
	defer client.Close() //nolint:errcheck

	var result any
	err := client.Call(testContext(t), "test.method", nil, &result)
	assertTrue(t, errors.Is(err, ErrNotConnected))
}

func TestConnected_States(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	client := newTestClient(mock)

	// Before connecting
	assertFalse(t, client.Connected())

	// After connecting
	err := client.Connect(testContext(t))
	assertNoError(t, err)
	assertTrue(t, client.Connected())

	// After closing
	err = client.Close()
	assertNoError(t, err)
	assertFalse(t, client.Connected())
}

// =============================================================================
// Error Type Tests
// =============================================================================

func TestIsNotFoundError(t *testing.T) {
	tests := []BoolTestCase{
		{
			Name:     "nil error",
			Input:    nil,
			Expected: false,
		},
		{
			Name:     "ErrNotFound sentinel",
			Input:    ErrNotFound,
			Expected: true,
		},
		{
			Name:     "RPC error with ENOENT code",
			Input:    &RPCError{Code: -6, Message: "some error"},
			Expected: true,
		},
		{
			Name:     "RPC error with 'not found' message",
			Input:    &RPCError{Code: 0, Message: "Resource not found"},
			Expected: true,
		},
		{
			Name:     "RPC error with 'does not exist' message",
			Input:    &RPCError{Code: 0, Message: "Dataset does not exist"},
			Expected: true,
		},
		{
			Name:     "RPC error with 'no such' message",
			Input:    &RPCError{Code: 0, Message: "No such file or directory"},
			Expected: true,
		},
		{
			Name:     "RPC error with not found in Data field",
			Input:    &RPCError{Code: 0, Message: "validation error", Data: json.RawMessage(`"instancenotfound"`)},
			Expected: true,
		},
		{
			Name:     "RPC error with ENOENT in Data field",
			Input:    &RPCError{Code: 0, Message: "error", Data: json.RawMessage(`"ENOENT: no such"`)},
			Expected: true,
		},
		{
			Name:     "RPC error with unrelated error",
			Input:    &RPCError{Code: -1, Message: "Internal server error"},
			Expected: false,
		},
		{
			Name:     "Other error type",
			Input:    errors.New("some other error"),
			Expected: false,
		},
	}

	runBoolTableTests(t, tests, IsNotFoundError)
}

func TestIsConnectionError(t *testing.T) {
	tests := []BoolTestCase{
		{
			Name:     "nil error",
			Input:    nil,
			Expected: false,
		},
		{
			Name:     "ConnectionError dial",
			Input:    &ConnectionError{Op: "dial", Err: errors.New("connection refused")},
			Expected: true,
		},
		{
			Name:     "ConnectionError read",
			Input:    &ConnectionError{Op: "read", Err: errors.New("connection reset")},
			Expected: true,
		},
		{
			Name:     "ConnectionError write",
			Input:    &ConnectionError{Op: "write", Err: errors.New("broken pipe")},
			Expected: true,
		},
		{
			Name:     "RPC error (not connection error)",
			Input:    &RPCError{Code: -1, Message: "error"},
			Expected: false,
		},
		{
			Name:     "Other error type",
			Input:    errors.New("some error"),
			Expected: false,
		},
	}

	runBoolTableTests(t, tests, IsConnectionError)
}

func TestRPCError_Error(t *testing.T) {
	tests := []struct {
		name     string
		err      *RPCError
		expected string
	}{
		{
			name:     "basic error",
			err:      &RPCError{Code: -1, Message: "test error"},
			expected: "truenas: rpc error -1: test error",
		},
		{
			name:     "error with data",
			err:      &RPCError{Code: -2, Message: "validation", Data: json.RawMessage(`"extra info"`)},
			expected: `truenas: rpc error -2: validation (data: "extra info")`,
		},
		{
			name:     "error with empty data",
			err:      &RPCError{Code: -3, Message: "error", Data: nil},
			expected: "truenas: rpc error -3: error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertEqual(t, tc.err.Error(), tc.expected)
		})
	}
}

func TestConnectionError_Error(t *testing.T) {
	err := &ConnectionError{Op: "dial", Err: errors.New("connection refused")}
	assertEqual(t, err.Error(), "truenas: dial: connection refused")
}

func TestConnectionError_Unwrap(t *testing.T) {
	innerErr := errors.New("inner error")
	err := &ConnectionError{Op: "read", Err: innerErr}

	unwrapped := err.Unwrap()
	assertEqual(t, unwrapped, innerErr)
}

// =============================================================================
// Configuration Tests
// =============================================================================

func TestNew_DefaultConfig(t *testing.T) {
	client := New(Config{
		URL:    "ws://localhost:8080",
		APIKey: "test-key",
	})
	defer client.Close() //nolint:errcheck

	// Verify defaults are applied
	assertEqual(t, client.config.CallTimeout, defaultCallTimeout)
	assertEqual(t, client.config.PingInterval, defaultPingInterval)
	assertEqual(t, client.config.ReconnectMin, defaultReconnectMin)
	assertEqual(t, client.config.ReconnectMax, defaultReconnectMax)
	assertEqual(t, client.config.ReconnectFactor, defaultReconnectFactor)
}

func TestNew_CustomConfig(t *testing.T) {
	client := New(Config{
		URL:             "ws://localhost:8080",
		APIKey:          "test-key",
		CallTimeout:     1 * time.Minute,
		PingInterval:    5 * time.Minute,
		ReconnectMin:    5 * time.Second,
		ReconnectMax:    2 * time.Minute,
		ReconnectFactor: 1.5,
	})
	defer client.Close() //nolint:errcheck

	assertEqual(t, client.config.CallTimeout, 1*time.Minute)
	assertEqual(t, client.config.PingInterval, 5*time.Minute)
	assertEqual(t, client.config.ReconnectMin, 5*time.Second)
	assertEqual(t, client.config.ReconnectMax, 2*time.Minute)
	assertEqual(t, client.config.ReconnectFactor, 1.5)
}

func TestNew_InsecureSkipVerify(t *testing.T) {
	client := New(Config{
		URL:                "wss://localhost:8080",
		APIKey:             "test-key",
		InsecureSkipVerify: true,
	})
	defer client.Close() //nolint:errcheck

	assertNotNil(t, client.config.TLSConfig)
	assertTrue(t, client.config.TLSConfig.InsecureSkipVerify)
}

// =============================================================================
// Response Handling Tests
// =============================================================================

func TestCall_NilResult(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	mock.SetResponse("test.method", MockResponse{
		Result: "ignored",
	})

	client := connectTestClient(t, mock)

	// Call with nil result (don't care about response)
	err := client.Call(testContext(t), "test.method", nil, nil)
	assertNoError(t, err)
}

func TestCall_EmptyResult(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	mock.SetResponse("test.method", MockResponse{
		Result: nil,
	})

	client := connectTestClient(t, mock)

	var result string
	err := client.Call(testContext(t), "test.method", nil, &result)
	assertNoError(t, err)
	assertEqual(t, result, "")
}

func TestCall_DynamicResponse(t *testing.T) {
	mock := NewMockTrueNASServer()
	defer mock.Close()

	callCount := 0
	mock.SetResponseFunc(func(method string, params json.RawMessage) MockResponse {
		callCount++
		return MockResponse{Result: callCount}
	})

	client := connectTestClient(t, mock)

	var result1 int
	err := client.Call(testContext(t), "test.method", nil, &result1)
	assertNoError(t, err)
	assertEqual(t, result1, 1)

	var result2 int
	err = client.Call(testContext(t), "test.method", nil, &result2)
	assertNoError(t, err)
	assertEqual(t, result2, 2)
}
