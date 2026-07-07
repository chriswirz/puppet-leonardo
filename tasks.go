package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"strings"
	"sync"
	"time"
)

// taskID identifies a generation request: the SHA-256 of its prompt, so the
// same prompt always maps to the same task.
func taskID(prompt string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(prompt)))
	return hex.EncodeToString(sum[:])
}

// Task statuses.
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskCompleted = "completed"
	TaskFailed    = "failed"
)

// errInterrupted is recorded against a task the file still shows as queued or
// running when the service starts: the queue and whatever was generating it
// are gone.
const errInterrupted = "interrupted: the service stopped before this generation finished"

// Task is one entry of the task dictionary. When a prompt that already has an
// image is generated again, URI keeps the previous image while the new run is
// queued or running, or if it fails, until a new image replaces it.
//
// The timestamps tell a caller where a task is: RequestedAt is when it was
// queued, StartedAt when it left the queue and began (signing in to Leonardo),
// SubmittedAt when the prompt was last submitted to Leonardo (the Generate
// click), and FinishedAt when it completed or failed. Attempts counts
// submissions; a failed first attempt is retried once.
type Task struct {
	Status      string    `json:"status"`
	Prompt      string    `json:"prompt"`
	Size        string    `json:"size,omitempty"`
	URI         string    `json:"uri,omitempty"`
	Error       string    `json:"error,omitempty"`
	RequestedAt time.Time `json:"requested_at,omitzero"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	Attempts    int       `json:"attempts,omitempty"`
}

// now is the clock, in UTC so the file reads the same wherever it is written.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

// TaskInfo is a Task together with its id, as returned for a single lookup.
// QueuePosition (1 = next to run) is filled in for a queued task; it changes
// as the queue moves, so it is never written to the file.
type TaskInfo struct {
	TaskID string `json:"task_id"`
	Task
	QueuePosition int `json:"queue_position,omitempty"`
}

// taskStore is the dictionary of task id -> task, persisted as a JSON object
// so it survives restarts. It is written whenever a task is queued, starts, is
// submitted to Leonardo, completes or fails, so the file always shows the
// queue and the generation in progress.
type taskStore struct {
	mu    sync.Mutex
	path  string
	tasks map[string]Task
}

// tasks is the process-wide store; main replaces it with the one loaded from
// cfg.TasksFile.
var tasks = newTaskStore("")

func newTaskStore(path string) *taskStore {
	return &taskStore{path: path, tasks: map[string]Task{}}
}

// loadTaskStore reads the dictionary from path. A missing file is an empty
// dictionary, not an error.
func loadTaskStore(path string) (*taskStore, error) {
	s := newTaskStore(path)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.tasks); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	interrupted := false
	for id, t := range s.tasks {
		switch t.Status {
		case "":
			// Written before tasks had a status, when only finished images
			// were recorded.
			t.Status = TaskCompleted
		case TaskQueued, TaskRunning:
			t.Status, t.Error = TaskFailed, errInterrupted
			interrupted = true
		}
		s.tasks[id] = t
	}
	if interrupted {
		if err := s.saveLocked(); err != nil {
			log.Printf("could not save %s: %v", path, err)
		}
	}
	return s, nil
}

// All returns a copy of the dictionary.
func (s *taskStore) All() map[string]Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.tasks)
}

// Get returns one task, and false if the id is unknown.
func (s *taskStore) Get(id string) (TaskInfo, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	return TaskInfo{TaskID: id, Task: t}, ok
}

// Queue records a task as waiting in the queue and writes the file. It starts
// the task's record afresh, keeping only the previous image.
func (s *taskStore) Queue(id, prompt, size string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[id] = Task{Status: TaskQueued, Prompt: strings.TrimSpace(prompt), Size: size, URI: s.tasks[id].URI, RequestedAt: now()}
	if err := s.saveLocked(); err != nil {
		log.Printf("could not save task %s to %s: %v", id, s.path, err)
	}
}

// Start records that a queued task has begun and writes the file.
func (s *taskStore) Start(id string) {
	s.update(id, func(t *Task) {
		t.Status, t.StartedAt = TaskRunning, now()
	})
}

// Submitted records that the prompt of a running task was just submitted to
// Leonardo, and writes the file.
func (s *taskStore) Submitted(id string) {
	s.update(id, func(t *Task) {
		if t.Status == TaskRunning {
			t.SubmittedAt = now()
			t.Attempts++
		}
	})
}

// Fail records a run that ended in an error and writes the file.
func (s *taskStore) Fail(id, prompt string, err error) {
	s.update(id, func(t *Task) {
		t.Status, t.Prompt, t.Error, t.FinishedAt = TaskFailed, strings.TrimSpace(prompt), err.Error(), now()
	})
}

// Complete records a finished task and writes the file.
func (s *taskStore) Complete(id, prompt, uri string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	t.Status, t.Prompt, t.URI, t.Error, t.FinishedAt = TaskCompleted, strings.TrimSpace(prompt), uri, "", now()
	s.tasks[id] = t
	return s.saveLocked()
}

// update changes one existing task and writes the file, logging rather than
// returning a write failure: the record in memory is still right.
func (s *taskStore) update(id string, change func(*Task)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return
	}
	change(&t)
	s.tasks[id] = t
	if err := s.saveLocked(); err != nil {
		log.Printf("could not save task %s to %s: %v", id, s.path, err)
	}
}

// saveLocked writes the dictionary through a temporary file and a rename, so
// an interrupted write cannot leave it truncated.
func (s *taskStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	out, err := json.MarshalIndent(s.tasks, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	// On Windows the rename fails while anything - an editor, a script, an
	// antivirus scan - has the file open, so it is retried briefly. If it still
	// fails, the file is overwritten in place: a reader might then catch it half
	// written, but the update is not lost, which a stale file would mean.
	for wait := 5 * time.Millisecond; wait <= 80*time.Millisecond; wait *= 2 {
		if err = os.Rename(tmp, s.path); err == nil {
			return nil
		}
		time.Sleep(wait)
	}
	if werr := os.WriteFile(s.path, append(out, '\n'), 0o644); werr != nil {
		return fmt.Errorf("%w; writing in place: %v", err, werr)
	}
	os.Remove(tmp)
	return nil
}
