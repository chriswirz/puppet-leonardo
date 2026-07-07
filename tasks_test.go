package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTaskStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	s, err := loadTaskStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id := taskID("a fox")
	if err := s.Complete(id, " a fox ", "https://cdn.leonardo.ai/users/u/generations/g/a.jpg"); err != nil {
		t.Fatal(err)
	}

	var onDisk map[string]Task
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("file is not a JSON object of tasks: %v\n%s", err, data)
	}
	if !strings.Contains(string(data), `"uri"`) || onDisk[id].Prompt != "a fox" {
		t.Fatalf("unexpected file contents:\n%s", data)
	}

	reloaded, err := loadTaskStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.All()[id]; got.URI != "https://cdn.leonardo.ai/users/u/generations/g/a.jpg" {
		t.Fatalf("after reload: %+v", got)
	}
}

func TestTaskIDIsPromptHash(t *testing.T) {
	if got := taskID("  a fox "); got != taskID("a fox") || len(got) != 64 {
		t.Fatalf("taskID not stable over surrounding whitespace: %s", got)
	}
}

func TestTasksEndpoints(t *testing.T) {
	tasks = newTaskStore("")
	id := taskID("a fox")
	tasks.Complete(id, "a fox", "https://example/a.jpg")

	rec := httptest.NewRecorder()
	handleTasks(rec, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	var got map[string]Task
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got[id].URI != "https://example/a.jpg" || got[id].Prompt != "a fox" {
		t.Fatalf("GET /api/tasks: %d %s", rec.Code, rec.Body)
	}

	h := newTestMCP(t, MCPConfig{LegacyCompatibility: true})
	mrec := post(h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_image_tasks","arguments":{}}}`, nil)
	res := decode(t, mrec)["result"].(map[string]any)
	sc := res["structuredContent"].(map[string]any)[id].(map[string]any)
	if sc["uri"] != "https://example/a.jpg" || sc["prompt"] != "a fox" {
		t.Fatalf("get_image_tasks: %s", mrec.Body)
	}
}

func TestGetTask(t *testing.T) {
	tasks = newTaskStore("")
	done, running := taskID("a fox"), taskID("a hare")
	tasks.Complete(done, "a fox", "https://example/a.jpg")
	tasks.Queue(running, "a hare", "16:9")
	tasks.Start(running)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks/{id}", handleTask)
	get := func(id string) (int, TaskInfo) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tasks/"+id, nil))
		var info TaskInfo
		json.Unmarshal(rec.Body.Bytes(), &info)
		return rec.Code, info
	}
	if code, info := get(done); code != http.StatusOK || info.TaskID != done || info.Status != TaskCompleted || info.Prompt != "a fox" || info.URI != "https://example/a.jpg" {
		t.Fatalf("completed: %d %+v", code, info)
	}
	if code, info := get(running); code != http.StatusOK || info.TaskID != running || info.Status != TaskRunning || info.Prompt != "a hare" || info.URI != "" || info.RequestedAt.IsZero() {
		t.Fatalf("running: %d %+v", code, info)
	}
	if code, _ := get(taskID("nothing")); code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}

	h := newTestMCP(t, MCPConfig{LegacyCompatibility: true})
	rec := post(h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_image_task","arguments":{"task_id":"`+done+`"}}}`, nil)
	sc := decode(t, rec)["result"].(map[string]any)["structuredContent"].(map[string]any)
	if sc["status"] != TaskCompleted || sc["uri"] != "https://example/a.jpg" || sc["prompt"] != "a fox" {
		t.Fatalf("get_image_task: %s", rec.Body)
	}
	rec = post(h, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_image_task","arguments":{"task_id":"nope"}}}`, nil)
	if decode(t, rec)["result"].(map[string]any)["isError"] != true {
		t.Fatalf("unknown task via MCP: %s", rec.Body)
	}
}

// stubGeneration gives the test a fresh queue of the given depth and replaces
// the browser with a generation that finishes when release is called,
// returning url or err. ran reports the prompts generated so far, in order.
func stubGeneration(t *testing.T, depth int, url string, err error) (release func(), ran func() []string) {
	t.Helper()
	queue = newGenerationQueue(depth)
	gate := make(chan struct{})
	var mu sync.Mutex
	var prompts []string
	session.run = func(prompt, size string) (string, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		mu.Unlock()
		tasks.Submitted(taskID(prompt))
		<-gate
		return url, err
	}
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	ran = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
	q := queue
	t.Cleanup(func() {
		release()
		waitFor(t, q.idle)
		session.mu.Lock()
		session.run = nil
		session.mu.Unlock()
	})
	return release, ran
}

// status reads a task's status.
func status(id string) string {
	info, _ := tasks.Get(id)
	return info.Status
}

func TestQueueRunsInOrderWithPositions(t *testing.T) {
	tasks = newTaskStore("")
	release, ran := stubGeneration(t, 100, "https://example/q.jpg", nil)
	a, _, pending, err := generate("a", "16:9", 10*time.Millisecond)
	if err != nil || !pending {
		t.Fatalf("a: pending=%v err=%v", pending, err)
	}
	waitFor(t, func() bool { return status(a) == TaskRunning })
	b, _, _, _ := generate("b", "1:1", 10*time.Millisecond)
	c, _, _, _ := generate("c", "2:3", 10*time.Millisecond)

	for id, want := range map[string]int{b: 1, c: 2} {
		info, _ := lookupTask(id)
		if info.Status != TaskQueued || info.QueuePosition != want || info.RequestedAt.IsZero() || !info.StartedAt.IsZero() {
			t.Fatalf("queued task: %+v", info)
		}
	}
	if info, _ := lookupTask(c); info.Size != "2:3" {
		t.Fatalf("size not recorded: %+v", info)
	}
	if info, _ := lookupTask(a); info.QueuePosition != 0 || info.StartedAt.IsZero() {
		t.Fatalf("running task: %+v", info)
	}

	release()
	waitFor(t, func() bool { return status(c) == TaskCompleted })
	if got := strings.Join(ran(), ","); got != "a,b,c" {
		t.Fatalf("ran %s, want a,b,c", got)
	}
	for _, id := range []string{a, b, c} {
		if info, _ := tasks.Get(id); info.Status != TaskCompleted || info.StartedAt.IsZero() || info.FinishedAt.IsZero() {
			t.Fatalf("after draining: %+v", info)
		}
	}
}

func TestSamePromptJoinsTheQueuedTask(t *testing.T) {
	tasks = newTaskStore("")
	release, ran := stubGeneration(t, 100, "https://example/dup.jpg", nil)
	generate("first", "16:9", 10*time.Millisecond)
	waitFor(t, func() bool { return status(taskID("first")) == TaskRunning })
	generate("dup", "16:9", 10*time.Millisecond)
	// The same prompt again - running or queued - joins the existing task.
	generate("first", "16:9", 10*time.Millisecond)
	id, _, _, err := generate(" dup ", "16:9", 10*time.Millisecond)
	if err != nil || id != taskID("dup") || queue.Waiting() != 1 {
		t.Fatalf("id=%s err=%v waiting=%d", id, err, queue.Waiting())
	}

	// Every request for it shares one job, so a waiter that joined late still
	// gets the image.
	j1, _ := queue.Submit("dup", "16:9")
	j2, _ := queue.Submit("dup", "1:1")
	if j1 != j2 {
		t.Fatal("same prompt submitted twice made two jobs")
	}
	release()
	<-j2.done
	if j2.url != "https://example/dup.jpg" {
		t.Fatalf("joined waiter got %q", j2.url)
	}
	waitFor(t, queue.idle)
	if got := strings.Join(ran(), ","); got != "first,dup" {
		t.Fatalf("ran %s, want first,dup", got)
	}
}

func TestFullQueueRejects(t *testing.T) {
	tasks = newTaskStore("")
	stubGeneration(t, 1, "https://example/x.jpg", nil)
	generate("running", "16:9", 10*time.Millisecond)
	waitFor(t, func() bool { return status(taskID("running")) == TaskRunning })
	if _, _, _, err := generate("waiting", "16:9", 10*time.Millisecond); err != nil {
		t.Fatalf("one may wait: %v", err)
	}
	id, _, _, err := generate("overflow", "16:9", 10*time.Millisecond)
	if !errors.Is(err, errQueueFull) || id != taskID("overflow") {
		t.Fatalf("id=%s err=%v", id, err)
	}
	if _, ok := tasks.Get(id); ok {
		t.Fatal("a rejected request must not be recorded")
	}

	rec := httptest.NewRecorder()
	handleGenerate(rec, httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"prompt":"overflow"}`)))
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "queue is full") {
		t.Fatalf("REST full: %d %s", rec.Code, rec.Body)
	}
}

func TestZeroDepthAllowsOnlyTheRunningTask(t *testing.T) {
	tasks = newTaskStore("")
	stubGeneration(t, 0, "https://example/x.jpg", nil)
	if _, _, _, err := generate("only", "16:9", 10*time.Millisecond); err != nil {
		t.Fatalf("an idle queue must take one: %v", err)
	}
	waitFor(t, func() bool { return status(taskID("only")) == TaskRunning })
	if _, _, _, err := generate("next", "16:9", 10*time.Millisecond); !errors.Is(err, errQueueFull) {
		t.Fatalf("depth 0 must reject while running: %v", err)
	}
}

func TestGenerateReturnsWhenReadyInTime(t *testing.T) {
	tasks = newTaskStore("")
	release, _ := stubGeneration(t, 100, "https://example/fast.jpg", nil)
	release()
	id, url, pending, err := generate("fast", "16:9", time.Minute)
	if err != nil || pending || url != "https://example/fast.jpg" || id != taskID("fast") {
		t.Fatalf("id=%s url=%s pending=%v err=%v", id, url, pending, err)
	}
	if info, _ := tasks.Get(id); info.Status != TaskCompleted {
		t.Fatalf("%+v", info)
	}
}

func TestGenerateOutlivesTheWait(t *testing.T) {
	tasks = newTaskStore("")
	release, _ := stubGeneration(t, 100, "https://example/slow.jpg", nil)
	id, _, pending, err := generate("slow", "16:9", 10*time.Millisecond)
	if err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	waitFor(t, func() bool { return status(id) == TaskRunning })
	release()
	waitFor(t, func() bool { info, _ := tasks.Get(id); return info.Status == TaskCompleted })
	if info, _ := tasks.Get(id); info.URI != "https://example/slow.jpg" {
		t.Fatalf("after finishing: %+v", info)
	}
}

func TestGenerateFailureIsRecorded(t *testing.T) {
	tasks = newTaskStore("")
	release, _ := stubGeneration(t, 100, "", errors.New("leonardo said no"))
	id, _, pending, _ := generate("doomed", "16:9", 10*time.Millisecond)
	if !pending {
		t.Fatal("expected pending")
	}
	release()
	waitFor(t, func() bool { info, _ := tasks.Get(id); return info.Status == TaskFailed })
	if info, _ := tasks.Get(id); info.Error != "leonardo said no" || info.Prompt != "doomed" {
		t.Fatalf("%+v", info)
	}
	if got := tasks.All()[id]; got.Status != TaskFailed || got.Error != "leonardo said no" {
		t.Fatalf("dictionary entry: %+v", got)
	}
}

func TestMCPGenerateReturnsUnfinishedTask(t *testing.T) {
	tasks = newTaskStore("")
	stubGeneration(t, 100, "https://example/x.jpg", nil)
	h := newTestMCP(t, MCPConfig{LegacyCompatibility: true})
	rec := post(h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+mcpToolName+`","arguments":{"prompt":"later","wait_seconds":0}}}`, nil)
	res := decode(t, rec)["result"].(map[string]any)
	sc := res["structuredContent"].(map[string]any)
	if res["isError"] == true || (sc["status"] != TaskRunning && sc["status"] != TaskQueued) || sc["task_id"] != taskID("later") {
		t.Fatalf("%s", rec.Body)
	}
}

func TestRESTGenerateWaitSeconds(t *testing.T) {
	tasks = newTaskStore("")
	stubGeneration(t, 100, "https://example/x.jpg", nil)
	rec := httptest.NewRecorder()
	handleGenerate(rec, httptest.NewRequest(http.MethodGet, "/api/generate?prompt=later&wait_seconds=1", nil))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"task_id":"`+taskID("later")+`"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTasksFileShowsQueueAndRunningTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	tasks = newTaskStore(path)
	release, _ := stubGeneration(t, 100, "https://example/slow.jpg", nil)
	id, _, pending, _ := generate("in progress", "16:9", 10*time.Millisecond)
	if !pending {
		t.Fatal("expected pending")
	}
	waitFor(t, func() bool { return readTasksFile(t, path)[id].Status == TaskRunning })
	if got := readTasksFile(t, path)[id]; got.Prompt != "in progress" || got.URI != "" || got.StartedAt.IsZero() {
		t.Fatalf("while running, file has %+v", got)
	}
	waiting, _, _, _ := generate("waiting", "16:9", 10*time.Millisecond)
	if got := readTasksFile(t, path)[waiting]; got.Status != TaskQueued || got.Size != "16:9" {
		t.Fatalf("queued task in file: %+v", got)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "queue_position") {
		t.Fatal("queue_position must not be persisted")
	}
	release()
	waitFor(t, func() bool { return readTasksFile(t, path)[waiting].Status == TaskCompleted })
	if got := readTasksFile(t, path)[id]; got.URI != "https://example/slow.jpg" {
		t.Fatalf("after finishing, file has %+v", got)
	}
}

func TestLoadMarksInterruptedAndUpgradesOldEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	os.WriteFile(path, []byte(`{
  "old": {"prompt": "from before statuses", "uri": "https://example/old.jpg"},
  "cut": {"status": "running", "prompt": "the service died"},
  "line": {"status": "queued", "prompt": "never started"}
}`), 0o644)
	s, err := loadTaskStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("old"); got.Status != TaskCompleted || got.URI != "https://example/old.jpg" {
		t.Fatalf("old entry: %+v", got)
	}
	if got, _ := s.Get("cut"); got.Status != TaskFailed || got.Error != errInterrupted {
		t.Fatalf("interrupted entry: %+v", got)
	}
	if got, _ := s.Get("line"); got.Status != TaskFailed || got.Error != errInterrupted {
		t.Fatalf("interrupted queued entry: %+v", got)
	}
	if got := readTasksFile(t, path)["cut"]; got.Status != TaskFailed {
		t.Fatalf("interrupted entry not written back: %+v", got)
	}
}

func readTasksFile(t *testing.T, path string) map[string]Task {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]Task
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return m
}

func TestTaskTimestamps(t *testing.T) {
	tasks = newTaskStore("")
	id := taskID("timed")
	tasks.Queue(id, "timed", "16:9")
	got, _ := tasks.Get(id)
	if got.Status != TaskQueued || got.RequestedAt.IsZero() || !got.StartedAt.IsZero() {
		t.Fatalf("queued: %+v", got)
	}
	tasks.Start(id)
	got, _ = tasks.Get(id)
	if got.Status != TaskRunning || got.StartedAt.IsZero() || !got.SubmittedAt.IsZero() || got.Attempts != 0 {
		t.Fatalf("before submitting (still signing in): %+v", got)
	}
	tasks.Submitted(id)
	tasks.Submitted(id) // a retry
	got, _ = tasks.Get(id)
	if got.SubmittedAt.IsZero() || got.Attempts != 2 || !got.FinishedAt.IsZero() {
		t.Fatalf("after submitting: %+v", got)
	}
	tasks.Complete(id, "timed", "https://example/t.jpg")
	got, _ = tasks.Get(id)
	if got.FinishedAt.IsZero() || got.RequestedAt.IsZero() || got.Attempts != 2 || got.Status != TaskCompleted {
		t.Fatalf("after completing: %+v", got)
	}
	// Submitted after the fact is ignored rather than reopening the task.
	tasks.Submitted(id)
	if again, _ := tasks.Get(id); again.Attempts != 2 {
		t.Fatalf("submitted on a finished task: %+v", again)
	}

	data, _ := json.Marshal(got)
	for _, key := range []string{`"requested_at":"`, `"started_at":"`, `"submitted_at":"`, `"finished_at":"`, `"attempts":2`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("%s missing from %s", key, data)
		}
	}
	running, _ := json.Marshal(Task{Status: TaskRunning, Prompt: "p"})
	if strings.Contains(string(running), "_at") {
		t.Fatalf("unset timestamps should be omitted: %s", running)
	}
}

func TestUpdatesIgnoreUnknownTasks(t *testing.T) {
	tasks = newTaskStore("")
	tasks.Start("ghost")
	tasks.Submitted("ghost")
	if _, ok := tasks.Get("ghost"); ok {
		t.Fatal("an update must not create a task")
	}
}

func TestSaveSurvivesConcurrentReaders(t *testing.T) {
	// On Windows, replacing a file that another process has open fails; the
	// save must retry or fall back rather than lose the update.
	path := filepath.Join(t.TempDir(), "tasks.json")
	s := newTaskStore(path)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				os.ReadFile(path)
			}
		}
	}()
	for i := range 50 {
		if err := s.Complete(taskID("p"), "p", fmt.Sprintf("https://example/%d.jpg", i)); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if got := readTasksFile(t, path)[taskID("p")]; got.URI != "https://example/49.jpg" {
		t.Fatalf("file has %+v", got)
	}
}
