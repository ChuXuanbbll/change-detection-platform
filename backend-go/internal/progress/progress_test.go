package progress

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func put(t *testing.T, root, name, data string) {
	t.Helper()
	dir := filepath.Join(root, "progress")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerAggregationAndInvalidSnapshots(t *testing.T) {
	root := t.TempDir()
	put(t, root, "stage.json", `{"stage":"model_loading","progress":5}`)
	put(t, root, "worker-0.json", `{"rank":0,"world":2,"done":3,"total":4}`)
	s, ok := Read(root)
	if !ok || s.Progress != 5 {
		t.Fatalf("missing worker was counted: %+v", s)
	}
	put(t, root, "worker-1.json", `{"rank":1,"world":2,"done":1,"total":2}`)
	s, ok = Read(root)
	if !ok || s.Stage != "inference" || s.Progress != 60 {
		t.Fatalf("wrong aggregate: %+v", s)
	}
	for _, bad := range []string{`{`, `{"rank":1,"world":2,"done":3,"total":2}`, `{"rank":0,"world":2,"done":1,"total":2}`, `{"rank":1,"world":2,"total":2}`} {
		put(t, root, "worker-1.json", bad)
		s, _ = Read(root)
		if s.Progress != 5 {
			t.Fatalf("bad snapshot counted: %s", bad)
		}
	}
	put(t, root, "stage.json", `{"stage":"vectorizing","progress":96}`)
	s, ok = Read(root)
	if !ok || s.Stage != "vectorizing" || s.Progress != 96 {
		t.Fatal(s)
	}
	for _, bad := range []string{`{`, `{"stage":"unknown","progress":50}`, `{"stage":"inference","progress":100}`, `{"stage":"aligning","progress":-1}`, `{"stage":"aligning"}`} {
		put(t, root, "stage.json", bad)
		if _, ok = Read(root); ok {
			t.Fatalf("invalid stage accepted: %s", bad)
		}
	}
}

func TestSingleEmptyAndUnevenWorkers(t *testing.T) {
	root := t.TempDir()
	put(t, root, "stage.json", `{"stage":"model_loading","progress":5}`)
	for _, w := range []string{`{"rank":0,"world":1,"done":1,"total":1}`, `{"rank":0,"world":1,"done":0,"total":0}`, `{"rank":0,"world":1,"done":5,"total":5}`} {
		put(t, root, "worker-0.json", w)
		if s, ok := Read(root); !ok || s.Progress != 85 {
			t.Fatalf("%s: %+v", w, s)
		}
	}
}

func TestWatchSamplesBeforeCompletionAndJoins(t *testing.T) {
	root := t.TempDir()
	updates := make(chan Snapshot, 10)
	put(t, root, "stage.json", `{"stage":"inference","progress":25}`)
	stop := Watch(root, func(s Snapshot) { updates <- s })
	defer stop()
	select {
	case s := <-updates:
		if s.Progress != 25 {
			t.Fatal(s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no live progress")
	}
	put(t, root, "stage.json", `{"stage":"inference","progress":55}`)
	select {
	case s := <-updates:
		if s.Progress != 55 {
			t.Fatal(s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no second progress")
	}
	stop()
	stop()
	if s := <-updates; s.Progress != 55 {
		t.Fatal(s)
	}
	select {
	case s := <-updates:
		t.Fatalf("late update: %+v", s)
	case <-time.After(550 * time.Millisecond):
	}
}
