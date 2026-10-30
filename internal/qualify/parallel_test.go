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
	"dissident/internal/platform"
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

// TestPromotionUsesDedicatedSlotWhenRealWorkersBusy saturates the cycle's deep
// test semaphore, then proves trusted promotion still gets past the acquire
// instead of blocking until the context deadline. The xray binary is
// deliberately absent: the promotion must reach render/start and fail there.
func TestPromotionUsesDedicatedSlotWhenRealWorkersBusy(t *testing.T) {
	link := socksLink(t, "127.0.0.1:1")
	for name, promote := range map[string]func(*Tester, context.Context, contract.Link) (contract.Result, *platform.Xray, error){
		"trusted": (*Tester).PromoteTrusted,
		"full":    (*Tester).Promote,
	} {
		t.Run(name, func(t *testing.T) {
			tester := New(Options{
				Test:    config.TestConfig{RealWorkers: 1, XrayTimeout: time.Second},
				XrayBin: "/nonexistent/xray-for-promotion-slot-test",
			})
			tester.realSem <- struct{}{}
			defer func() { <-tester.realSem }()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			_, xray, err := promote(tester, ctx, link)
			if xray != nil {
				t.Fatal("promotion with a missing xray binary returned a process")
			}
			if err == nil {
				t.Fatal("promotion with a missing xray binary should fail at start")
			}
			if ctx.Err() != nil {
				t.Fatalf("promotion blocked on realSem until ctx deadline: %v", err)
			}
			if elapsed := time.Since(start); elapsed >= time.Second {
				t.Fatalf("promotion took %v, looks blocked on realSem", elapsed)
			}
			if got := len(tester.promoteSem); got != 0 {
				t.Fatalf("promotion slot held after failure: %d", got)
			}
		})
	}
}

// TestPromotionSlotCapacityOneAndCancel proves the promotion slot has capacity
// one and that a canceled acquire does not leak the slot.
func TestPromotionSlotCapacityOneAndCancel(t *testing.T) {
	tester := New(Options{
		Test:    config.TestConfig{RealWorkers: 1, XrayTimeout: time.Second},
		XrayBin: "/nonexistent/xray-for-promotion-slot-test",
	})
	tester.promoteSem <- struct{}{} // simulate an in-flight promotion

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, xray, err := tester.PromoteTrusted(ctx, socksLink(t, "127.0.0.1:1"))
	if err == nil || xray != nil {
		t.Fatalf("second promotion = (%v, %v), want capacity-1 rejection", xray, err)
	}
	if ctx.Err() == nil {
		t.Fatal("second promotion returned before the slot deadline; capacity is not one")
	}
	if got := len(tester.promoteSem); got != 1 {
		t.Fatalf("manually held promotion slot = %d, want 1", got)
	}
	<-tester.promoteSem

	canceled, cancelCanceled := context.WithCancel(context.Background())
	cancelCanceled()
	if _, xray, err := tester.PromoteTrusted(canceled, socksLink(t, "127.0.0.1:1")); err == nil || xray != nil {
		t.Fatalf("canceled promotion = (%v, %v), want context error", xray, err)
	}
	if got := len(tester.promoteSem); got != 0 {
		t.Fatalf("promotion slot leaked after canceled acquire: %d", got)
	}
}
