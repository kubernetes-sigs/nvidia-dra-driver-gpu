/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workqueue

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

type WorkQueue struct {
	sync.RWMutex
	queue     workqueue.TypedRateLimitingInterface[any]
	activeOps map[string]*WorkItem
}

type WorkItem struct {
	Key      string
	Object   any
	Callback func(ctx context.Context, obj any) error
}

// Return composite rate limiter that combines both per-item exponential backoff
// and an overall token bucket rate-limiting strategy. It calculates the
// exponential backoff for the individual item (based on its personal retry
// history), checks the global rate against the token bucket, and picks the
// longest delay from either strategy, ensuring that both per-item and overall
// queue health are respected.
func DefaultPrepUnprepRateLimiter() workqueue.TypedRateLimiter[any] {
	return workqueue.NewTypedMaxOfRateLimiter(
		// This is a per-item exponential backoff limiter. Each time an item
		// fails and is retried, the delay grows exponentially starting from the
		// lower value up to the upper bound.
		workqueue.NewTypedItemExponentialFailureRateLimiter[any](250*time.Millisecond, 3000*time.Millisecond),
		// Global (not per-item) rate limiter. Allows up to 5 retries per
		// second, with bursts of up to 10.
		&workqueue.TypedBucketRateLimiter[any]{Limiter: rate.NewLimiter(rate.Limit(5), 10)},
	)
}

func DefaultCDDaemonRateLimiter() workqueue.TypedRateLimiter[any] {
	return NewJitterRateLimiter(workqueue.NewTypedItemExponentialFailureRateLimiter[any](5*time.Millisecond, 6000*time.Millisecond), 0.5)
}

func DefaultControllerRateLimiter() workqueue.TypedRateLimiter[any] {
	return workqueue.DefaultTypedControllerRateLimiter[any]()
}

func New(r workqueue.TypedRateLimiter[any]) *WorkQueue {
	queue := workqueue.NewTypedRateLimitingQueue(r)
	return &WorkQueue{
		queue:     queue,
		activeOps: make(map[string]*WorkItem),
	}
}

func (q *WorkQueue) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		q.queue.ShutDown()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			q.processNextWorkItem(ctx)
		}
	}
}

func (q *WorkQueue) EnqueueRaw(obj any, callback func(ctx context.Context, obj any) error) {
	workItem := &WorkItem{
		Object:   obj,
		Callback: callback,
	}
	q.queue.AddRateLimited(workItem)
}

func (q *WorkQueue) Enqueue(obj any, callback func(ctx context.Context, obj any) error) {
	runtimeObj, ok := obj.(runtime.Object)
	if !ok {
		klog.Errorf("unexpected object type %T: runtime.Object required", obj)
		return
	}

	workItem := &WorkItem{
		Object:   runtimeObj.DeepCopyObject(),
		Callback: callback,
	}

	q.queue.AddRateLimited(workItem)
}

func (q *WorkQueue) EnqueueRawWithKey(obj any, key string, callback func(ctx context.Context, obj any) error) {
	workItem := &WorkItem{
		Key:      key,
		Object:   obj,
		Callback: callback,
	}

	q.Lock()
	q.activeOps[key] = workItem
	q.queue.AddRateLimited(workItem)
	q.Unlock()
}

func (q *WorkQueue) EnqueueWithKey(obj any, key string, callback func(ctx context.Context, obj any) error) {
	runtimeObj, ok := obj.(runtime.Object)
	if !ok {
		klog.Errorf("unexpected object type %T: runtime.Object required", obj)
		return
	}

	workItem := &WorkItem{
		Key:      key,
		Object:   runtimeObj.DeepCopyObject(),
		Callback: callback,
	}

	q.Lock()
	q.activeOps[key] = workItem
	// Do we also want to make sure here that a previously enqueued task for
	// this key isn't going to be run anymore, if not yet started? Currently,
	// the next-scheduled retry attempt is still executed, and business logic is
	// hopefully resilient enough.
	klog.V(7).Infof("enqueue with key: %s", key)
	q.queue.AddRateLimited(workItem)
	q.Unlock()
}

func (q *WorkQueue) processNextWorkItem(ctx context.Context) {
	item, shutdown := q.queue.Get()
	if shutdown {
		return
	}
	defer q.queue.Done(item)

	workItem, ok := item.(*WorkItem)
	if !ok {
		klog.Errorf("Unexpected item in queue: %v", item)
		return
	}

	attempts := q.queue.NumRequeues(item)
	err := q.reconcile(ctx, workItem)
	if err != nil {
		// Most often, this is an expected, retryable error in the context of an
		// eventually consistent system. Hence, do not log on an error level. Rely
		// on inner business logic to log unexpected errors on an error level.
		klog.V(1).Infof("Reconcile: %v (attempt %d)", err, attempts)
		// Only retry if we're still the current operation for this key
		q.Lock()
		if q.activeOps[workItem.Key] != nil && q.activeOps[workItem.Key] != workItem {
			klog.V(1).Infof("Do not re-enqueue failed work item with key '%s': a newer item was enqueued", workItem.Key)
			q.queue.Forget(workItem)
		} else {
			q.queue.AddRateLimited(workItem)
		}
		q.Unlock()
	} else {
		// Only clean up activeOps if this item is still the current one for this key
		q.Lock()
		if workItem == q.activeOps[workItem.Key] {
			delete(q.activeOps, workItem.Key)
		}
		q.queue.Forget(workItem)
		q.Unlock()
	}
}

func (q *WorkQueue) reconcile(ctx context.Context, workItem *WorkItem) error {
	if workItem.Callback == nil {
		return fmt.Errorf("no callback to process work item: %+v", workItem)
	}
	return workItem.Callback(ctx, workItem.Object)
}
