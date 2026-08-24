package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

type snapshotEnvelope struct {
	Version int            `json:"version"`
	Tasks   []snapshotTask `json:"tasks"`
}

type snapshotTask struct {
	ID           string   `json:"id"`
	Dependencies []string `json:"dependencies"`
	Priority     int      `json:"priority"`
	Payload      []byte   `json:"payload"`
	Order        uint64   `json:"order"`
}

func (g *Graph) Snapshot() ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	records := make([]taskRecord, 0, len(g.tasks))
	for _, record := range g.tasks {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].order < records[j].order })
	out := snapshotEnvelope{Version: 1, Tasks: make([]snapshotTask, 0, len(records))}
	for _, record := range records {
		out.Tasks = append(out.Tasks, snapshotTask{
			ID: record.task.ID, Dependencies: append([]string(nil), record.task.Dependencies...),
			Priority: record.task.Priority, Payload: append([]byte(nil), record.task.Payload...), Order: record.order,
		})
	}
	return json.Marshal(out)
}

func (g *Graph) Restore(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var input snapshotEnvelope
	if err := decoder.Decode(&input); err != nil {
		return errors.Join(ErrInvalidSnapshot, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.Join(ErrInvalidSnapshot, err)
	}
	if input.Version != 1 {
		return ErrInvalidSnapshot
	}
	tasks := make(map[string]taskRecord, len(input.Tasks))
	orders := make(map[uint64]bool, len(input.Tasks))
	var next uint64
	for _, saved := range input.Tasks {
		task := Task{ID: saved.ID, Dependencies: saved.Dependencies, Priority: saved.Priority, Payload: saved.Payload}
		if err := validateTask(task); err != nil || orders[saved.Order] {
			return errors.Join(ErrInvalidSnapshot, err)
		}
		if _, exists := tasks[task.ID]; exists {
			return ErrInvalidSnapshot
		}
		orders[saved.Order] = true
		tasks[task.ID] = taskRecord{task: cloneTask(task), order: saved.Order}
		if saved.Order >= next {
			next = saved.Order + 1
		}
	}
	if err := validateRecords(tasks); err != nil {
		return errors.Join(ErrInvalidSnapshot, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tasks, g.next = tasks, next
	return nil
}
