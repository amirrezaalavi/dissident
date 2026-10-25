package qualify

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

func TestCheapStageWorkerLimit(t *testing.T) {
	tester := New(Options{
		Test: config.TestConfig{
			CheapWorkers:    3,
			RealWorkers:     2,
			TCPTimeout:      50 * time.Millisecond,
			TLSTimeout:      50 * time.Millisecond,
			ProtocolTimeout: 50 * time.Millisecond,
		},
	})
	var active, maximum int32
	tester.dialFn = func(context.Context, string, string) (net.Conn, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			previous := atomic.LoadInt32(&maximum)
			if current <= previous || atomic.CompareAndSwapInt32(&maximum, previous, current) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return nil, errors.New("unreachable")
	}

	link := contract.Link{
		Protocol: contract.ProtocolSOCKS5,
		Server:   "127.0.0.1",
		Port:     1,
		Hash:     "cheap-limit",
	}
	var workers sync.WaitGroup
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := tester.Test(context.Background(), link)
			if result.OK || result.Stage != contract.StageTCP {
				t.Errorf("result = stage %q ok=%v reason=%q, want tcp failure", result.Stage, result.OK, result.Reason)
			}
		}()
	}
	workers.Wait()

	if got := atomic.LoadInt32(&maximum); got > 3 {
		t.Fatalf("cheap stage concurrency = %d, want <= 3", got)
	} else if got < 2 {
		t.Fatalf("cheap stage concurrency = %d, want at least 2", got)
	}
}

func TestCanceledStageAcquire(t *testing.T) {
	tester := New(Options{Test: config.TestConfig{CheapWorkers: 1, RealWorkers: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	tester.cheapSem <- struct{}{}
	cancel()
	if err := tester.acquire(ctx, tester.cheapSem); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire error = %v, want context.Canceled", err)
	}
}
