package db

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// statusErr is the error the SDK returns for an HTTP status, with Cosmos's
// retry-after header when retryAfterMS isn't empty.
func statusErr(status int, retryAfterMS string) error {
	resp := &http.Response{StatusCode: status, Header: http.Header{}}
	if retryAfterMS != "" {
		resp.Header.Set("x-ms-retry-after-ms", retryAfterMS)
	}
	return &azcore.ResponseError{StatusCode: status, RawResponse: resp}
}

func TestWhileThrottledRetriesUntilCosmosLetsItThrough(t *testing.T) {
	calls := 0
	err := whileThrottled(context.Background(), func() error {
		if calls++; calls < 3 {
			return statusErr(http.StatusTooManyRequests, "1")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err %v after %d calls, want nil after 3", err, calls)
	}
}

func TestWhileThrottledRetriesAThrottledBatch(t *testing.T) {
	calls := 0
	start := time.Now()
	err := whileThrottled(context.Background(), func() error {
		if calls++; calls < 2 {
			return errThrottled
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err %v after %d calls, want nil after 2", err, calls)
	}
	if waited := time.Since(start); waited < throttleBackoff {
		t.Errorf("waited %v, want at least %v with no retry-after to go on", waited, throttleBackoff)
	}
}

func TestWhileThrottledGivesUp(t *testing.T) {
	calls := 0
	err := whileThrottled(context.Background(), func() error {
		calls++
		return statusErr(http.StatusTooManyRequests, "1")
	})
	if !hasStatus(err, http.StatusTooManyRequests) || calls != maxThrottleRetries+1 {
		t.Fatalf("err %v after %d calls, want the 429 after %d", err, calls, maxThrottleRetries+1)
	}
}

func TestWhileThrottledLeavesOtherErrorsAlone(t *testing.T) {
	for _, want := range []error{statusErr(http.StatusNotFound, ""), errors.New("network")} {
		calls := 0
		err := whileThrottled(context.Background(), func() error {
			calls++
			return want
		})
		if err != want || calls != 1 {
			t.Errorf("err %v after %d calls, want %v after 1", err, calls, want)
		}
	}
}

func TestWhileThrottledStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := whileThrottled(ctx, func() error {
		calls++
		return statusErr(http.StatusTooManyRequests, "60000")
	})
	if !hasStatus(err, http.StatusTooManyRequests) || calls != 1 {
		t.Fatalf("err %v after %d calls, want the 429 after 1", err, calls)
	}
}

func TestThrottleWait(t *testing.T) {
	for _, c := range []struct {
		err     error
		attempt int
		want    time.Duration
	}{
		{statusErr(http.StatusTooManyRequests, "250"), 5, 250 * time.Millisecond},
		{statusErr(http.StatusTooManyRequests, "12.5"), 0, 12500 * time.Microsecond},
		{statusErr(http.StatusTooManyRequests, "600000"), 0, maxThrottleBackoff},
		{statusErr(http.StatusTooManyRequests, ""), 0, throttleBackoff},
		{errThrottled, 2, 4 * throttleBackoff},
		{errThrottled, 30, maxThrottleBackoff},
	} {
		if got := throttleWait(c.err, c.attempt); got != c.want {
			t.Errorf("throttleWait(%v, %d) = %v, want %v", c.err, c.attempt, got, c.want)
		}
	}
}
