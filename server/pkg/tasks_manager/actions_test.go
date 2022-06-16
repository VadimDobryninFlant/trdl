package tasks_manager

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/stretchr/testify/assert"

	"github.com/werf/trdl/server/pkg/tasks_manager/worker"
)

// check that Manager.RunTask queues task or returns the busy error
func TestManager_RunTask(t *testing.T) {
	ctx := context.Background()
	m := initManagerWithoutWorker()
	storage := &logical.InmemStorage{}

	assert.Nil(t, m.Storage, "must be initialized on the first action call")
	var uuids []string
	// check the first task
	{
		uuid, err := m.RunTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.NotEmpty(t, uuid)
		assert.NotNil(t, m.Storage, "must be initialized on the first action call")

		assertQueuedTaskInStorage(t, ctx, storage, uuid)

		uuids = append(uuids, uuid)
	}

	// check the second task
	{
		uuid, err := m.RunTask(ctx, storage, noneTask)
		if assert.Error(t, err) {
			assert.Equal(t, err, ErrBusy)
		}
		assert.Empty(t, uuid)
	}

	// check queue
	for _, uuid := range uuids {
		task := <-m.taskChan
		assert.Equal(t, task.UUID, uuid)
	}
}

// check that Manager.RunTask queues task or returns the busy error
func TestManager_RunTaskWithCurrentRunningTask(t *testing.T) {
	ctx := context.Background()
	m := initManagerWithoutWorker()
	storage := &logical.InmemStorage{}

	m.Storage = storage
	runningTaskUUID := assertAndAddRunningTaskToStorage(t, ctx, storage)

	{
		uuid, err := m.RunTask(ctx, storage, noneTask)
		if assert.Error(t, err) {
			assert.Equal(t, err, ErrBusy)
		}
		assert.Empty(t, uuid)
		assert.NotNil(t, m.Storage, "must be initialized on the first action call")
	}

	err := storage.Delete(ctx, taskStorageKey(taskStateRunning, runningTaskUUID))
	assert.Nil(t, err)

	{
		uuid, err := m.RunTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.NotEmpty(t, uuid)

		assertQueuedTaskInStorage(t, ctx, storage, uuid)

		task := <-m.taskChan
		assert.Equal(t, task.UUID, uuid)
	}
}

// check that Manager.RunTask invalidates inconsistent storage
func TestManager_RunTaskInvalidateStorage(t *testing.T) {
	ctx := context.Background()
	m := initManagerWithoutWorker()
	storage := &logical.InmemStorage{}

	// imitate inconsistent storage condition after previous plugin run
	queuedTaskUUID := assertAndAddNewTaskToStorage(t, ctx, storage)
	runningTaskUUID := assertAndAddRunningTaskToStorage(t, ctx, storage)
	assert.Nil(t, m.Storage, "must be initialized on the first action call")

	uuid, err := m.RunTask(ctx, storage, noneTask)
	assert.Nil(t, err)
	assert.NotEmpty(t, uuid)

	// check queue and running task invalidation
	{
		task, err := getTaskFromStorage(ctx, storage, taskStateRunning, runningTaskUUID)
		assert.Nil(t, err)
		assert.Nil(t, task)

		task, err = getTaskFromStorage(ctx, storage, taskStateCompleted, runningTaskUUID)
		assert.Nil(t, err)
		if assert.NotNil(t, task) {
			assert.Equal(t, string(taskStatusCanceled), task.Status)
			assert.Equal(t, taskReasonInvalidatedTask, task.Reason)
		}
	}

	// check queue task invalidation
	{
		task, err := getTaskFromStorage(ctx, storage, taskStateQueued, queuedTaskUUID)
		assert.Nil(t, err)
		assert.Nil(t, task)

		task, err = getTaskFromStorage(ctx, storage, taskStateCompleted, queuedTaskUUID)
		assert.Nil(t, err)
		if assert.NotNil(t, task) {
			assert.Equal(t, string(taskStatusCanceled), task.Status)
			assert.Equal(t, taskReasonInvalidatedTask, task.Reason)
		}
	}
}

// check that Manager.AddTask queues all tasks
func TestManager_AddTask(t *testing.T) {
	ctx := context.Background()
	m := initManagerWithoutWorker()
	storage := &logical.InmemStorage{}

	assert.Nil(t, m.Storage, "must be initialized on the first action call")
	var uuids []string
	for i := 0; i < 2; i++ {
		uuid, err := m.AddTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.NotEmpty(t, uuid)
		if i == 0 {
			assert.NotNil(t, m.Storage, "must be initialized on the first action call")
		}

		assertQueuedTaskInStorage(t, ctx, storage, uuid)

		uuids = append(uuids, uuid)
	}

	// check queue and task order
	for _, uuid := range uuids {
		task := <-m.taskChan
		assert.Equal(t, task.UUID, uuid)
	}
}

// check that Manager.AddOptionalTask queues task when manager not busy
func TestManager_AddOptionalTask(t *testing.T) {
	ctx := context.Background()
	m := initManagerWithoutWorker()
	storage := &logical.InmemStorage{}

	assert.Nil(t, m.Storage, "must be initialized on the first action call")
	var uuids []string
	// check the first task
	{
		uuid, added, err := m.AddOptionalTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.NotEmpty(t, uuid)
		assert.True(t, added)
		assert.NotNil(t, m.Storage, "must be initialized on the first action call")

		assertQueuedTaskInStorage(t, ctx, storage, uuid)

		uuids = append(uuids, uuid)
	}

	// check the second task
	{
		uuid, added, err := m.AddOptionalTask(ctx, storage, noneTask)
		assert.Nil(t, err)
		assert.Empty(t, uuid)
		assert.False(t, added)
	}

	// check queue
	for _, uuid := range uuids {
		task := <-m.taskChan
		assert.Equal(t, task.UUID, uuid)
	}
}

func TestManager_WrapTaskFunc(t *testing.T) {
	m := initManagerWithoutWorker()
	storage := &logical.InmemStorage{}

	// storage must be initialized when calling the method
	m.Storage = storage

	// wrap task func
	taskFuncErrCh := make(chan error)
	wrappedTaskErrCh := make(chan error)
	doneCh := make(chan bool)
	wrappedTaskFunc := m.WrapTaskFunc(func(ctx context.Context, _ logical.Storage) error {
		defer func() { doneCh <- true }()
		return <-taskFuncErrCh
	}, defaultTaskTimeoutDuration)

	// run wrapped task func
	ctx, ctxCancelFunc := context.WithCancel(context.Background())
	go func() {
		wrappedTaskErrCh <- wrappedTaskFunc(ctx)
	}()

	// cancel context and check context canceled error
	ctxCancelFunc()
	assert.Equal(t, ErrContextCanceled, <-wrappedTaskErrCh)

	// check that main action running in background
	taskFuncErrCh <- fmt.Errorf("error")
	<-doneCh
}

func initManagerWithoutWorker() *Manager {
	taskChan := make(chan *worker.Task, taskChanSize)
	m := &Manager{taskChan: taskChan, logger: hclog.L()}
	return m
}

func noneTask(_ context.Context, _ logical.Storage) error { return nil }

func assertQueuedTaskInStorage(t *testing.T, ctx context.Context, storage logical.Storage, uuid string) {
	task, err := getTaskFromStorage(ctx, storage, taskStateQueued, uuid)
	assert.Nil(t, err)
	assert.NotNil(t, task)
	assert.Equal(t, task.Status, string(taskStatusQueued))
}
