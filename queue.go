package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// errQueueFull is returned when queue_depth tasks are already waiting.
var errQueueFull = errors.New("the generation queue is full")

// job is one generation, queued or running. Everyone waiting on the same task
// id shares it, and done is closed when it finishes.
type job struct {
	id, prompt, size string

	done chan struct{}
	url  string
	err  error
}

// generationQueue runs generations one at a time, in the order they were
// requested, with up to depth more waiting behind the one that is running. A
// request for a prompt that is already queued or running joins that task
// instead of adding another, so a client that retries gets the same task back
// rather than a second generation.
type generationQueue struct {
	depth int

	mu      sync.Mutex
	pending []*job          // waiting, oldest first
	current *job            // running, or nil
	byID    map[string]*job // pending and current, by task id
	wake    chan struct{}
	start   sync.Once
}

// queue is the process-wide queue; main replaces it with one sized from
// cfg.QueueDepth before serving.
var queue = newGenerationQueue(100)

func newGenerationQueue(depth int) *generationQueue {
	return &generationQueue{
		depth: max(depth, 0),
		byID:  map[string]*job{},
		wake:  make(chan struct{}, 1),
	}
}

// Submit queues a generation, or returns the queued or running job for the
// same prompt. It records the task as queued in the task dictionary.
func (q *generationQueue) Submit(prompt, size string) (*job, error) {
	q.start.Do(func() { go q.work() })
	id := taskID(prompt)

	q.mu.Lock()
	defer q.mu.Unlock()
	if j, ok := q.byID[id]; ok {
		return j, nil
	}
	// The running job does not count against the depth: depth is how many may
	// wait. With nothing running and nothing waiting the job starts at once.
	if len(q.pending) >= q.depth && (q.current != nil || len(q.pending) > 0) {
		return nil, fmt.Errorf("%w (%d waiting); try again later", errQueueFull, len(q.pending))
	}
	j := &job{id: id, prompt: prompt, size: size, done: make(chan struct{})}
	q.pending = append(q.pending, j)
	q.byID[id] = j
	tasks.Queue(id, prompt, size)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return j, nil
}

// Position is the place of a task in the queue: 1 is next to run. It is 0 for
// a task that is running or not in the queue.
func (q *generationQueue) Position(id string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, j := range q.pending {
		if j.id == id {
			return i + 1
		}
	}
	return 0
}

// Waiting reports how many tasks are queued behind the running one.
func (q *generationQueue) Waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// idle reports whether nothing is running or waiting.
func (q *generationQueue) idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.current == nil && len(q.pending) == 0
}

// work runs queued jobs one at a time for the life of the process.
func (q *generationQueue) work() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.mu.Unlock()
			<-q.wake
			continue
		}
		j := q.pending[0]
		q.pending = q.pending[1:]
		q.current = j
		q.mu.Unlock()

		j.url, j.err = session.runJob(j)

		q.mu.Lock()
		q.current = nil
		delete(q.byID, j.id)
		q.mu.Unlock()
		close(j.done)
	}
}

// runJob performs one generation and records its outcome.
func (s *browserSession) runJob(j *job) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks.Start(j.id)
	run := s.run
	if run == nil {
		run = s.generateLocked
	}
	url, err := run(j.prompt, j.size)
	if err != nil {
		log.Printf("task %s failed: %v", j.id, err)
		tasks.Fail(j.id, j.prompt, err)
		return "", err
	}
	log.Printf("task %s generated: %s", j.id, url)
	if err := tasks.Complete(j.id, j.prompt, url); err != nil {
		// The image exists either way; failing to persist it is not the
		// caller's problem.
		log.Printf("could not save task %s to %s: %v", j.id, cfg.TasksFile, err)
	}
	return url, nil
}

// generate queues a generation (or joins the one already queued or running for
// the same prompt) and waits for it.
//
// The generation runs in the background, so it finishes and is recorded even
// when the caller stops waiting. wait bounds how long this call blocks: 0 waits
// for the result; otherwise, if the image is not ready in time, pending is true
// and the caller should look the task up later (GET /api/tasks/{id},
// get_image_task). A full queue is reported straight away.
func generate(prompt, size string, wait time.Duration) (id, url string, pending bool, err error) {
	j, err := queue.Submit(prompt, size)
	if err != nil {
		return taskID(prompt), "", false, err
	}
	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-j.done:
		return j.id, j.url, false, j.err
	case <-timeout:
		return j.id, "", true, nil
	}
}

// lookupTask returns a task with its current queue position filled in.
func lookupTask(id string) (TaskInfo, bool) {
	info, ok := tasks.Get(id)
	if ok && info.Status == TaskQueued {
		info.QueuePosition = queue.Position(info.TaskID)
	}
	return info, ok
}
