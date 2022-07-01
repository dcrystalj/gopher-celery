// Package celery helps to work with Celery (place tasks in queues and execute them).
package celery

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/go-kit/log"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/marselester/gopher-celery/protocol"
	"github.com/marselester/gopher-celery/redis"
)

// TaskF represents a Celery task implemented by the client.
type TaskF func(context.Context, *TaskParam)

// Broker is responsible for receiving and sending task messages.
// For example, it knows how to read a message from a given queue in Redis.
// The messages can be in defferent formats depending on Celery protocol version.
type Broker interface {
	// Send puts a message to a queue.
	// Note, the method is safe to call concurrently.
	Send(msg []byte, queue string) error
	// Observe sets the queues from which the tasks should be received.
	// Note, the method is not concurrency safe.
	Observe(queues []string)
	// Receive returns a raw message from one of the queues.
	// It blocks until there is a message available for consumption.
	// Note, the method is not concurrency safe.
	Receive() ([]byte, error)
}

// NewApp creates a Celery app.
// The default broker is Redis assumed to run on localhost.
// When producing tasks the default message serializer is json and protocol is v2.
func NewApp(options ...Option) *App {
	app := App{
		conf: Config{
			logger:     log.NewNopLogger(),
			registry:   protocol.NewSerializerRegistry(),
			mime:       protocol.MimeJSON,
			protocol:   protocol.V2,
			maxWorkers: DefaultMaxWorkers,
		},
		task:      make(map[string]TaskF),
		taskQueue: make(map[string]string),
	}

	for _, opt := range options {
		opt(&app.conf)
	}
	app.sem = make(chan struct{}, app.conf.maxWorkers)

	if app.conf.broker == nil {
		app.conf.broker = redis.NewBroker()
	}

	return &app
}

// App is a Celery app to produce or consume tasks asynchronously.
type App struct {
	// conf represents app settings.
	conf Config

	// task maps a Celery task path to a task itself, e.g.,
	// "myproject.apps.myapp.tasks.mytask": TaskF.
	task map[string]TaskF
	// taskQueue helps to determine which queue a task belongs to, e.g.,
	// "myproject.apps.myapp.tasks.mytask": "important".
	taskQueue map[string]string
	// sem is a semaphore that limits number of workers.
	sem chan struct{}
}

// Register associates the task with given Python path and queue.
// For example, when "myproject.apps.myapp.tasks.mytask"
// is seen in "important" queue, the TaskF task is executed.
//
// Note, the method is not concurrency safe.
// The tasks mustn't be registered after the app starts processing tasks.
func (a *App) Register(path, queue string, task TaskF) {
	a.task[path] = task
	a.taskQueue[path] = queue
}

// Delay places the task associated with given Python path into queue.
func (a *App) Delay(path, queue string, args ...interface{}) error {
	m := protocol.Task{
		ID:   uuid.NewString(),
		Name: path,
		Args: args,
	}
	rawMsg, err := a.conf.registry.Encode(queue, a.conf.mime, a.conf.protocol, &m)
	if err != nil {
		return fmt.Errorf("failed to encode task message: %w", err)
	}

	if err = a.conf.broker.Send(rawMsg, queue); err != nil {
		return fmt.Errorf("failed to send task message to broker: %w", err)
	}
	return nil
}

// Run launches the workers that process the tasks received from the broker.
// The call is blocking until ctx is cancelled.
// The caller mustn't register any new tasks at this point.
func (a *App) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	qq := make([]string, 0, len(a.taskQueue))
	for k := range a.taskQueue {
		qq = append(qq, a.taskQueue[k])
	}
	a.conf.broker.Observe(qq)

	msgs := make(chan *protocol.Task, 1)
	g.Go(func() error {
		defer close(msgs)

		// One goroutine fetching and decoding tasks from queues
		// shouldn't be a bottleneck since the worker goroutines
		// usually take seconds/minutes to complete.
		for {
			select {
			case <-ctx.Done():
				return nil
			default:
				rawMsg, err := a.conf.broker.Receive()
				if err != nil {
					a.conf.logger.Log("msg", "failed to receive a raw task message", "err", err)
					continue
				}
				// No messages in the broker so far.
				if rawMsg == nil {
					continue
				}

				m, err := a.conf.registry.Decode(rawMsg)
				if err != nil {
					a.conf.logger.Log("msg", "failed to decode task message", "rawmsg", rawMsg, "err", err)
					continue
				}

				msgs <- m
			}
		}
	})

	go func() {
		// Start a worker when there is a task.
		for m := range msgs {
			if a.task[m.Name] == nil {
				a.conf.logger.Log("msg", "unregistered task", "taskmsg", m)
				continue
			}
			if m.IsExpired() {
				a.conf.logger.Log("msg", "task message expired", "taskmsg", m)
				continue
			}

			select {
			// Acquire a semaphore by sending a token.
			case a.sem <- struct{}{}:
			// Stop processing tasks.
			case <-ctx.Done():
				return
			}

			m := m
			g.Go(func() error {
				// Release a semaphore by discarding a token.
				defer func() { <-a.sem }()

				if err := a.executeTask(ctx, m); err != nil {
					a.conf.logger.Log("msg", "task failed", "taskmsg", m, "err", err)
				}
				return nil
			})
		}
	}()

	return g.Wait()
}

// executeTask calls the task function with args and kwargs from the message.
// If the task panics, the stack trace is returned as an error.
func (a *App) executeTask(ctx context.Context, m *protocol.Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("unexpected task error: %v: %s", r, debug.Stack())
		}
	}()

	task := a.task[m.Name]
	p := NewTaskParam(m.Args, m.Kwargs)
	task(ctx, p)
	return
}
