package bilibili

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"
)

// 保序是硬要求：输出按楼顺序流式落盘，续传靠「已写入的前缀 = 已完成的前缀」
// 判断进度。乱序落盘会让这个等式失效，续传时静默地重复或遗漏。
//
// 这里故意让先派发的任务慢、后派发的任务快，制造出「完成顺序与派发顺序相反」
// 的局面——不保序的实现必然在这里失败。
func TestRunOrderedEmitsInOrder(t *testing.T) {
	const n = 50
	var got []int
	err := RunOrdered(context.Background(), 8, n,
		func(ctx context.Context, i int) (int, error) {
			// 下标越小睡得越久，保证完成顺序与派发顺序相反。
			time.Sleep(time.Duration(n-i) * time.Millisecond)
			return i, nil
		},
		func(i int, v int) error {
			if v != i {
				t.Errorf("第 %d 次回调拿到值 %d", i, v)
			}
			got = append(got, v)
			return nil
		})
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if len(got) != n {
		t.Fatalf("回调 %d 次，期望 %d 次", len(got), n)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("回调顺序 = %v，期望 0..%d 递增", got, n-1)
		}
	}
}

// 并发度必须是真正的上界，不能把所有任务一次性甩出去。
func TestRunOrderedRespectsConcurrency(t *testing.T) {
	var running, peak int64
	err := RunOrdered(context.Background(), 3, 30,
		func(ctx context.Context, i int) (int, error) {
			cur := atomic.AddInt64(&running, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt64(&running, -1)
			return i, nil
		},
		func(int, int) error { return nil })
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if peak > 3 {
		t.Errorf("并发峰值 = %d，超过设定的 3", peak)
	}
}

func TestRunOrderedConcurrencyFloor(t *testing.T) {
	// 并发度 0 或负数要当成 1，而不是死锁或 panic。
	for _, c := range []int{0, -5} {
		var n int
		err := RunOrdered(context.Background(), c, 5,
			func(ctx context.Context, i int) (int, error) { return i, nil },
			func(int, int) error { n++; return nil })
		if err != nil {
			t.Fatalf("并发度 %d 失败：%v", c, err)
		}
		if n != 5 {
			t.Errorf("并发度 %d 时只回调了 %d 次", c, n)
		}
	}
}

func TestRunOrderedEmpty(t *testing.T) {
	if err := RunOrdered(context.Background(), 4, 0,
		func(context.Context, int) (int, error) { t.Fatal("不该被调用"); return 0, nil },
		func(int, int) error { t.Fatal("不该被调用"); return nil }); err != nil {
		t.Fatalf("空任务应当直接返回：%v", err)
	}
}

// 出错后必须尽快停手，不能把剩下的任务全跑完——那意味着继续发无谓的请求。
func TestRunOrderedStopsAfterError(t *testing.T) {
	var calls int64
	err := RunOrdered(context.Background(), 2, 100,
		func(ctx context.Context, i int) (int, error) {
			atomic.AddInt64(&calls, 1)
			if i == 3 {
				return 0, errors.New("第 3 个任务炸了")
			}
			time.Sleep(time.Millisecond)
			return i, nil
		},
		func(int, int) error { return nil })

	if err == nil {
		t.Fatal("应当返回错误")
	}
	var oe *OrderedError
	if !errors.As(err, &oe) || oe.Index != 3 {
		t.Errorf("错误应带上下标 3，实际 %v", err)
	}
	// 允许已经在飞的几个任务跑完，但不该把 100 个都跑掉。
	if calls > 20 {
		t.Errorf("出错后仍执行了 %d 个任务，应当尽快停下", calls)
	}
}

// 出错时不能只报「出错了」——落盘的内容是完整的前缀，
// summary 要能说清是第几栋楼失败的。
func TestRunOrderedStopsEmittingAfterError(t *testing.T) {
	var emitted int
	err := RunOrdered(context.Background(), 4, 20,
		func(ctx context.Context, i int) (int, error) { return i, nil },
		func(i int, v int) error {
			if i == 5 {
				return errors.New("写盘失败")
			}
			emitted++
			return nil
		})
	if err == nil {
		t.Fatal("应当返回错误")
	}
	// 前 5 个（0..4）成功发射，第 5 个失败后不再发射。
	if emitted != 5 {
		t.Errorf("发射了 %d 次，期望 5 次", emitted)
	}
}

// work 返回错误时也必须把所有 done 收完，否则 goroutine 泄漏（或死锁）。
// 用一个较短的超时来捕获死锁。
func TestRunOrderedNoDeadlockOnMassFailure(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- RunOrdered(context.Background(), 5, 60,
			func(ctx context.Context, i int) (int, error) {
				if i%3 == 0 {
					return 0, fmt.Errorf("任务 %d 失败", i)
				}
				return i, nil
			},
			func(int, int) error { return nil })
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("应当返回错误")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("死锁了")
	}
}

// 随机化的压力测试：无论完成顺序如何，回调顺序必须是 0..n-1。
func TestRunOrderedRandomizedOrdering(t *testing.T) {
	for round := 0; round < 5; round++ {
		n := 30 + rand.Intn(30)
		conc := 1 + rand.Intn(8)
		var got []int
		err := RunOrdered(context.Background(), conc, n,
			func(ctx context.Context, i int) (int, error) {
				time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
				return i * 7, nil
			},
			func(i int, v int) error {
				if v != i*7 {
					return fmt.Errorf("下标 %d 拿到 %d", i, v)
				}
				got = append(got, i)
				return nil
			})
		if err != nil {
			t.Fatalf("第 %d 轮失败：%v", round, err)
		}
		if len(got) != n {
			t.Fatalf("第 %d 轮回调 %d 次，期望 %d", round, len(got), n)
		}
		for i, v := range got {
			if v != i {
				t.Fatalf("第 %d 轮顺序错乱：%v", round, got)
			}
		}
	}
}
