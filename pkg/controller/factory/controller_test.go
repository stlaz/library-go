package factory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/library-go/pkg/operator/events"
)

func makeFakeSecret() *v1.Secret {
	return &v1.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test",
		},
		Data: map[string][]byte{
			"test": {},
		},
	}
}

func TestResyncController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	factory := New().ResyncEvery(100 * time.Millisecond)

	controllerSynced := make(chan struct{})
	syncCallCount := 0
	controller := factory.WithSync(func(ctx context.Context, controllerContext SyncContext) error {
		syncCallCount++
		if syncCallCount == 3 {
			defer close(controllerSynced)
		}
		t.Logf("controller sync called (%d)", syncCallCount)
		return nil
	}).ToController("PeriodicController", events.NewInMemoryRecorder("periodic-controller"))

	go controller.Run(ctx, 1)
	time.Sleep(1 * time.Second) // Give controller time to start

	select {
	case <-controllerSynced:
		cancel()
	case <-time.After(10 * time.Second):
		t.Fatal("failed to resync at least three times")
	}
}

func TestMultiWorkerControllerShutdown(t *testing.T) {
	controllerCtx, shutdown := context.WithCancel(context.TODO())
	factory := New().ResyncEvery(10 * time.Minute) // make sure we only call 1 sync manually
	var workersShutdownMutex sync.Mutex
	var syncCallCountMutex sync.Mutex

	workersShutdownCount := 0
	syncCallCount := 0
	allWorkersBusy := make(chan struct{})

	// simulate a long running sync logic that is signalled to shutdown
	controller := factory.WithSync(func(ctx context.Context, syncContext SyncContext) error {
		syncCallCountMutex.Lock()
		syncCallCount++
		switch syncCallCount {
		case 1:
			syncContext.Queue().Add("TestKey1")
			syncContext.Queue().Add("TestKey2")
			syncContext.Queue().Add("TestKey3")
			syncContext.Queue().Add("TestKey4")
		case 5:
			close(allWorkersBusy)
		}
		syncCallCountMutex.Unlock()

		// block until the shutdown is seen
		<-ctx.Done()

		// count workers shutdown
		workersShutdownMutex.Lock()
		workersShutdownCount++
		workersShutdownMutex.Unlock()

		return nil
	}).ToController("ShutdownController", events.NewInMemoryRecorder("shutdown-controller"))

	// wait for all workers to be busy, then signal shutdown
	go func() {
		defer shutdown()
		<-allWorkersBusy
	}()

	// this blocks until all workers are shut down.
	controller.Run(controllerCtx, 5)

	workersShutdownMutex.Lock()
	if workersShutdownCount != 5 {
		t.Fatalf("expected all workers to gracefully shutdown, got %d", workersShutdownCount)
	}
	workersShutdownMutex.Unlock()
}

func TestControllerWithInformer(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))
	ctx, cancel := context.WithCancel(context.TODO())
	go kubeInformers.Start(ctx.Done())

	factory := New().WithInformers(kubeInformers.Core().V1().Secrets().Informer())

	controllerSynced := make(chan struct{})
	controller := factory.WithSync(func(ctx context.Context, syncContext SyncContext) error {
		defer close(controllerSynced)
		if syncContext.Queue() == nil {
			t.Errorf("expected queue to be initialized, it is not")
		}
		if syncContext.QueueKey() != "key" {
			t.Errorf("expected queue key to be 'key', got %q", syncContext.QueueKey())
		}
		return nil
	}).ToController("FakeController", events.NewInMemoryRecorder("fake-controller"))

	go controller.Run(ctx, 1)
	time.Sleep(1 * time.Second) // Give controller time to start

	if _, err := kubeClient.CoreV1().Secrets("test").Create(context.TODO(), makeFakeSecret(), meta.CreateOptions{}); err != nil {
		t.Fatalf("failed to create fake secret: %v", err)
	}

	select {
	case <-controllerSynced:
		cancel()
	case <-time.After(30 * time.Second):
		t.Fatal("test timeout")
	}
}

func TestControllerWithQueueFunction(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))
	ctx, cancel := context.WithCancel(context.TODO())
	go kubeInformers.Start(ctx.Done())

	queueFn := func(obj runtime.Object) string {
		metaObj, err := apimeta.Accessor(obj)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s/%s", metaObj.GetNamespace(), metaObj.GetName())
	}

	factory := New().WithInformersQueueKeyFunc(queueFn, kubeInformers.Core().V1().Secrets().Informer())

	controllerSynced := make(chan struct{})
	controller := factory.WithSync(func(ctx context.Context, syncContext SyncContext) error {
		defer close(controllerSynced)
		if syncContext.Queue() == nil {
			t.Errorf("expected queue to be initialized, it is not")
		}
		if syncContext.QueueKey() != "test/test-secret" {
			t.Errorf("expected queue key to be 'test/test-secret', got %q", syncContext.QueueKey())
		}
		return nil
	}).ToController("FakeController", events.NewInMemoryRecorder("fake-controller"))

	go controller.Run(ctx, 1)
	time.Sleep(1 * time.Second) // Give controller time to start

	if _, err := kubeClient.CoreV1().Secrets("test").Create(context.TODO(), makeFakeSecret(), meta.CreateOptions{}); err != nil {
		t.Fatalf("failed to create fake secret: %v", err)
	}

	select {
	case <-controllerSynced:
		cancel()
	case <-time.After(30 * time.Second):
		t.Fatal("test timeout")
	}
}
