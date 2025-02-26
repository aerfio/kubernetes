/*
Copyright 2023 The Kubernetes Authors.

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

package cacher

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage"
	testingclock "k8s.io/utils/clock/testing"
)

// verifies the cacheWatcher.process goroutine is properly cleaned up even if
// the writes to cacheWatcher.result channel is blocked.
func TestCacheWatcherCleanupNotBlockedByResult(t *testing.T) {
	var lock sync.RWMutex
	var w *cacheWatcher
	count := 0
	filter := func(string, labels.Set, fields.Set) bool { return true }
	forget := func(drainWatcher bool) {
		lock.Lock()
		defer lock.Unlock()
		count++
		// forget() has to stop the watcher, as only stopping the watcher
		// triggers stopping the process() goroutine which we are in the
		// end waiting for in this test.
		w.setDrainInputBufferLocked(drainWatcher)
		w.stopLocked()
	}
	initEvents := []*watchCacheEvent{
		{Object: &v1.Pod{}},
		{Object: &v1.Pod{}},
	}
	// set the size of the buffer of w.result to 0, so that the writes to
	// w.result is blocked.
	w = newCacheWatcher(0, filter, forget, testVersioner{}, time.Now(), false, schema.GroupResource{Resource: "pods"}, "")
	go w.processInterval(context.Background(), intervalFromEvents(initEvents), 0)
	w.Stop()
	if err := wait.PollImmediate(1*time.Second, 5*time.Second, func() (bool, error) {
		lock.RLock()
		defer lock.RUnlock()
		return count == 2, nil
	}); err != nil {
		t.Fatalf("expected forget() to be called twice, because sendWatchCacheEvent should not be blocked by the result channel: %v", err)
	}
}

func TestCacheWatcherHandlesFiltering(t *testing.T) {
	filter := func(_ string, _ labels.Set, field fields.Set) bool {
		return field["spec.nodeName"] == "host"
	}
	forget := func(bool) {}

	testCases := []struct {
		events   []*watchCacheEvent
		expected []watch.Event
	}{
		// properly handle starting with the filter, then being deleted, then re-added
		{
			events: []*watchCacheEvent{
				{
					Type:            watch.Added,
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
					ObjFields:       fields.Set{"spec.nodeName": "host"},
					ResourceVersion: 1,
				},
				{
					Type:            watch.Modified,
					PrevObject:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
					PrevObjFields:   fields.Set{"spec.nodeName": "host"},
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
					ObjFields:       fields.Set{"spec.nodeName": ""},
					ResourceVersion: 2,
				},
				{
					Type:            watch.Modified,
					PrevObject:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
					PrevObjFields:   fields.Set{"spec.nodeName": ""},
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "3"}},
					ObjFields:       fields.Set{"spec.nodeName": "host"},
					ResourceVersion: 3,
				},
			},
			expected: []watch.Event{
				{Type: watch.Added, Object: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}}},
				{Type: watch.Deleted, Object: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}}},
				{Type: watch.Added, Object: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "3"}}},
			},
		},
		// properly handle ignoring changes prior to the filter, then getting added, then deleted
		{
			events: []*watchCacheEvent{
				{
					Type:            watch.Added,
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
					ObjFields:       fields.Set{"spec.nodeName": ""},
					ResourceVersion: 1,
				},
				{
					Type:            watch.Modified,
					PrevObject:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
					PrevObjFields:   fields.Set{"spec.nodeName": ""},
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
					ObjFields:       fields.Set{"spec.nodeName": ""},
					ResourceVersion: 2,
				},
				{
					Type:            watch.Modified,
					PrevObject:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
					PrevObjFields:   fields.Set{"spec.nodeName": ""},
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "3"}},
					ObjFields:       fields.Set{"spec.nodeName": "host"},
					ResourceVersion: 3,
				},
				{
					Type:            watch.Modified,
					PrevObject:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "3"}},
					PrevObjFields:   fields.Set{"spec.nodeName": "host"},
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "4"}},
					ObjFields:       fields.Set{"spec.nodeName": "host"},
					ResourceVersion: 4,
				},
				{
					Type:            watch.Modified,
					PrevObject:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "4"}},
					PrevObjFields:   fields.Set{"spec.nodeName": "host"},
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "5"}},
					ObjFields:       fields.Set{"spec.nodeName": ""},
					ResourceVersion: 5,
				},
				{
					Type:            watch.Modified,
					PrevObject:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "5"}},
					PrevObjFields:   fields.Set{"spec.nodeName": ""},
					Object:          &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "6"}},
					ObjFields:       fields.Set{"spec.nodeName": ""},
					ResourceVersion: 6,
				},
			},
			expected: []watch.Event{
				{Type: watch.Added, Object: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "3"}}},
				{Type: watch.Modified, Object: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "4"}}},
				{Type: watch.Deleted, Object: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "5"}}},
			},
		},
	}

TestCase:
	for i, testCase := range testCases {
		// set the size of the buffer of w.result to 0, so that the writes to
		// w.result is blocked.
		for j := range testCase.events {
			testCase.events[j].ResourceVersion = uint64(j) + 1
		}

		w := newCacheWatcher(0, filter, forget, testVersioner{}, time.Now(), false, schema.GroupResource{Resource: "pods"}, "")
		go w.processInterval(context.Background(), intervalFromEvents(testCase.events), 0)

		ch := w.ResultChan()
		for j, event := range testCase.expected {
			e := <-ch
			if !reflect.DeepEqual(event, e) {
				t.Errorf("%d: unexpected event %d: %s", i, j, diff.ObjectReflectDiff(event, e))
				break TestCase
			}
		}
		select {
		case obj, ok := <-ch:
			t.Errorf("%d: unexpected excess event: %#v %t", i, obj, ok)
			break TestCase
		default:
		}
		w.setDrainInputBufferLocked(false)
		w.stopLocked()
	}
}

func TestCacheWatcherStoppedInAnotherGoroutine(t *testing.T) {
	var w *cacheWatcher
	done := make(chan struct{})
	filter := func(string, labels.Set, fields.Set) bool { return true }
	forget := func(drainWatcher bool) {
		w.setDrainInputBufferLocked(drainWatcher)
		w.stopLocked()
		done <- struct{}{}
	}

	maxRetriesToProduceTheRaceCondition := 1000
	// Simulating the timer is fired and stopped concurrently by set time
	// timeout to zero and run the Stop goroutine concurrently.
	// May sure that the watch will not be blocked on Stop.
	for i := 0; i < maxRetriesToProduceTheRaceCondition; i++ {
		w = newCacheWatcher(0, filter, forget, testVersioner{}, time.Now(), false, schema.GroupResource{Resource: "pods"}, "")
		go w.Stop()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("stop is blocked when the timer is fired concurrently")
		}
	}

	deadline := time.Now().Add(time.Hour)
	// After that, verifies the cacheWatcher.process goroutine works correctly.
	for i := 0; i < maxRetriesToProduceTheRaceCondition; i++ {
		w = newCacheWatcher(2, filter, emptyFunc, testVersioner{}, deadline, false, schema.GroupResource{Resource: "pods"}, "")
		w.input <- &watchCacheEvent{Object: &v1.Pod{}, ResourceVersion: uint64(i + 1)}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		go w.processInterval(ctx, intervalFromEvents(nil), 0)
		select {
		case <-w.ResultChan():
		case <-time.After(time.Second):
			t.Fatal("expected received a event on ResultChan")
		}
		w.setDrainInputBufferLocked(false)
		w.stopLocked()
	}
}

func TestCacheWatcherStoppedOnDestroy(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	// Wait until cacher is initialized.
	if err := cacher.ready.wait(); err != nil {
		t.Fatalf("unexpected error waiting for the cache to be ready")
	}

	w, err := cacher.Watch(context.Background(), "pods/ns", storage.ListOptions{ResourceVersion: "0", Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}

	watchClosed := make(chan struct{})
	go func() {
		defer close(watchClosed)
		for event := range w.ResultChan() {
			switch event.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				// ok
			default:
				t.Errorf("unexpected event %#v", event)
			}
		}
	}()

	cacher.Stop()

	select {
	case <-watchClosed:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("timed out waiting for watch to close")
	}

}

func TestTimeBucketWatchersBasic(t *testing.T) {
	filter := func(_ string, _ labels.Set, _ fields.Set) bool {
		return true
	}
	forget := func(bool) {}

	newWatcher := func(deadline time.Time) *cacheWatcher {
		return newCacheWatcher(0, filter, forget, testVersioner{}, deadline, true, schema.GroupResource{Resource: "pods"}, "")
	}

	clock := testingclock.NewFakeClock(time.Now())
	watchers := newTimeBucketWatchers(clock, defaultBookmarkFrequency)
	now := clock.Now()
	watchers.addWatcher(newWatcher(now.Add(10 * time.Second)))
	watchers.addWatcher(newWatcher(now.Add(20 * time.Second)))
	watchers.addWatcher(newWatcher(now.Add(20 * time.Second)))

	if len(watchers.watchersBuckets) != 2 {
		t.Errorf("unexpected bucket size: %#v", watchers.watchersBuckets)
	}
	watchers0 := watchers.popExpiredWatchers()
	if len(watchers0) != 0 {
		t.Errorf("unexpected bucket size: %#v", watchers0)
	}

	clock.Step(10 * time.Second)
	watchers1 := watchers.popExpiredWatchers()
	if len(watchers1) != 1 || len(watchers1[0]) != 1 {
		t.Errorf("unexpected bucket size: %v", watchers1)
	}
	watchers1 = watchers.popExpiredWatchers()
	if len(watchers1) != 0 {
		t.Errorf("unexpected bucket size: %#v", watchers1)
	}

	clock.Step(12 * time.Second)
	watchers2 := watchers.popExpiredWatchers()
	if len(watchers2) != 1 || len(watchers2[0]) != 2 {
		t.Errorf("unexpected bucket size: %#v", watchers2)
	}
}

func makeWatchCacheEvent(rv uint64) *watchCacheEvent {
	return &watchCacheEvent{
		Type: watch.Added,
		Object: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", rv),
				ResourceVersion: fmt.Sprintf("%d", rv),
			},
		},
		ResourceVersion: rv,
	}
}

// TestCacheWatcherDraining verifies the cacheWatcher.process goroutine is properly cleaned up when draining was requested
func TestCacheWatcherDraining(t *testing.T) {
	var lock sync.RWMutex
	var w *cacheWatcher
	count := 0
	filter := func(string, labels.Set, fields.Set) bool { return true }
	forget := func(drainWatcher bool) {
		lock.Lock()
		defer lock.Unlock()
		count++
		w.setDrainInputBufferLocked(drainWatcher)
		w.stopLocked()
	}
	initEvents := []*watchCacheEvent{
		makeWatchCacheEvent(5),
		makeWatchCacheEvent(6),
	}
	w = newCacheWatcher(1, filter, forget, testVersioner{}, time.Now(), true, schema.GroupResource{Resource: "pods"}, "")
	go w.processInterval(context.Background(), intervalFromEvents(initEvents), 1)
	if !w.add(makeWatchCacheEvent(7), time.NewTimer(1*time.Second)) {
		t.Fatal("failed adding an even to the watcher")
	}
	forget(true) // drain the watcher

	eventCount := 0
	for range w.ResultChan() {
		eventCount++
	}
	if eventCount != 3 {
		t.Errorf("Unexpected number of objects received: %d, expected: 3", eventCount)
	}
	if err := wait.PollImmediate(1*time.Second, 5*time.Second, func() (bool, error) {
		lock.RLock()
		defer lock.RUnlock()
		return count == 2, nil
	}); err != nil {
		t.Fatalf("expected forget() to be called twice, because processInterval should call Stop(): %v", err)
	}
}

// TestCacheWatcherDrainingRequestedButNotDrained verifies the cacheWatcher.process goroutine is properly cleaned up when draining was requested
// but the client never actually get any data
func TestCacheWatcherDrainingRequestedButNotDrained(t *testing.T) {
	var lock sync.RWMutex
	var w *cacheWatcher
	count := 0
	filter := func(string, labels.Set, fields.Set) bool { return true }
	forget := func(drainWatcher bool) {
		lock.Lock()
		defer lock.Unlock()
		count++
		w.setDrainInputBufferLocked(drainWatcher)
		w.stopLocked()
	}
	initEvents := []*watchCacheEvent{
		makeWatchCacheEvent(5),
		makeWatchCacheEvent(6),
	}
	w = newCacheWatcher(1, filter, forget, testVersioner{}, time.Now(), true, schema.GroupResource{Resource: "pods"}, "")
	go w.processInterval(context.Background(), intervalFromEvents(initEvents), 1)
	if !w.add(makeWatchCacheEvent(7), time.NewTimer(1*time.Second)) {
		t.Fatal("failed adding an even to the watcher")
	}
	forget(true) // drain the watcher
	w.Stop()     // client disconnected, timeout expired or ctx was actually closed
	if err := wait.PollImmediate(1*time.Second, 5*time.Second, func() (bool, error) {
		lock.RLock()
		defer lock.RUnlock()
		return count == 3, nil
	}); err != nil {
		t.Fatalf("expected forget() to be called three times, because processInterval should call Stop(): %v", err)
	}
}
