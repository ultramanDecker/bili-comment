package bilibili

import (
	"context"
	"sync"
	"sync/atomic"
)

// RunOrdered 用 concurrency 个 worker 并发处理下标 0..n-1，但**严格按下标顺序**回调结果。
//
// 为什么要保序：输出是按楼顺序流式写盘的，续传靠「已写入的前缀 = 已完成的前缀」
// 来判断进度。若结果乱序落盘，这个等式就不成立，续传会重复或遗漏。
// 保序的代价只是一块与并发度同量的缓冲，值得。
//
// 注意并发并不提高请求速率——真正的刹车是限速器，它按固定间隔发放请求许可。
// 并发的作用只是消除「等上一个响应回来」的空转，把间隔用满。
func RunOrdered[R any](
	ctx context.Context,
	concurrency, n int,
	work func(ctx context.Context, i int) (R, error),
	emit func(i int, r R) error,
) error {
	if n == 0 {
		return nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > n {
		concurrency = n
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		results = make([]R, n)
		errs    = make([]error, n)
		done    = make([]chan struct{}, n)
	)
	for i := range done {
		done[i] = make(chan struct{})
	}

	var (
		next int64 = -1
		wg   sync.WaitGroup
	)
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= n {
					return
				}
				// 每个被领取的下标都必须关闭自己的 done，否则先出错时
				// 发射端会永远阻塞在 <-done[i] 上。用闭包 + defer 保证这一点。
				func() {
					defer close(done[i])
					if err := ctx.Err(); err != nil {
						errs[i] = err
						return
					}
					r, err := work(ctx, i)
					results[i] = r
					errs[i] = err
					if err != nil {
						// 让其余 worker 尽快停下，别再打无谓的请求。
						cancel()
					}
				}()
			}
		}()
	}

	var (
		emitErr  error
		firstErr error
		firstIdx = -1
	)
	// 无论成败都要把 n 个 done 收完，否则 worker 可能阻塞在发送上。
	for i := 0; i < n; i++ {
		<-done[i]
		if errs[i] != nil && firstErr == nil {
			firstErr = errs[i]
			firstIdx = i
		}
		// 出错之后不再发射，但要继续收 done 让 goroutine 退出。
		if firstErr == nil && emitErr == nil {
			if err := emit(i, results[i]); err != nil {
				emitErr = err
				cancel()
			}
		}
	}
	wg.Wait()

	if firstErr != nil {
		return &OrderedError{Index: firstIdx, Err: firstErr}
	}
	return emitErr
}

// OrderedError 包住某个下标上的失败，便于上层知道是哪一栋楼出的问题。
type OrderedError struct {
	Index int
	Err   error
}

func (e *OrderedError) Error() string { return e.Err.Error() }
func (e *OrderedError) Unwrap() error { return e.Err }
