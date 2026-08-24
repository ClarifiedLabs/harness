package planner

import (
	"fmt"
	"sort"
	"sync"
)

type taskRecord struct {
	task  Task
	order uint64
}

type Graph struct {
	mu    sync.RWMutex
	tasks map[string]taskRecord
	next  uint64
}

func New() *Graph { return &Graph{tasks: make(map[string]taskRecord)} }

func cloneTask(task Task) Task {
	task.Dependencies = append([]string(nil), task.Dependencies...)
	sort.Strings(task.Dependencies)
	task.Payload = append([]byte(nil), task.Payload...)
	return task
}

func validateTask(task Task) error {
	if task.ID == "" {
		return ErrInvalidID
	}
	seen := make(map[string]bool, len(task.Dependencies))
	for _, dependency := range task.Dependencies {
		if dependency == "" || dependency == task.ID || seen[dependency] {
			return ErrInvalidDependency
		}
		seen[dependency] = true
	}
	return nil
}

func addRecord(tasks map[string]taskRecord, next *uint64, task Task) error {
	if err := validateTask(task); err != nil {
		return err
	}
	if _, exists := tasks[task.ID]; exists {
		return ErrDuplicate
	}
	tasks[task.ID] = taskRecord{task: cloneTask(task), order: *next}
	*next++
	return nil
}

func removeRecord(tasks map[string]taskRecord, id string) error {
	if _, exists := tasks[id]; !exists {
		return ErrNotFound
	}
	for _, record := range tasks {
		for _, dependency := range record.task.Dependencies {
			if dependency == id {
				return fmt.Errorf("%w: %s", ErrInUse, id)
			}
		}
	}
	delete(tasks, id)
	return nil
}

func cloneRecords(input map[string]taskRecord) map[string]taskRecord {
	out := make(map[string]taskRecord, len(input))
	for id, record := range input {
		record.task = cloneTask(record.task)
		out[id] = record
	}
	return out
}

func (g *Graph) Add(task Task) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return addRecord(g.tasks, &g.next, task)
}

func (g *Graph) Get(id string) (Task, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	record, ok := g.tasks[id]
	return cloneTask(record.task), ok
}

func (g *Graph) Remove(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return removeRecord(g.tasks, id)
}

func (g *Graph) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.tasks)
}
