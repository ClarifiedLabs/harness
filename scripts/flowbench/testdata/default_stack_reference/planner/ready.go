package planner

import "sort"

func lessRecord(left, right taskRecord) bool {
	if left.task.Priority != right.task.Priority {
		return left.task.Priority > right.task.Priority
	}
	if left.order != right.order {
		return left.order < right.order
	}
	return left.task.ID < right.task.ID
}

func readyRecords(tasks map[string]taskRecord, completed map[string]bool) []taskRecord {
	var ready []taskRecord
	for id, record := range tasks {
		if completed[id] {
			continue
		}
		ok := true
		for _, dependency := range record.task.Dependencies {
			if !completed[dependency] {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, record)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return lessRecord(ready[i], ready[j]) })
	return ready
}

func (g *Graph) Ready(completed []string) ([]Task, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := validateRecords(g.tasks); err != nil {
		return nil, err
	}
	done := make(map[string]bool, len(completed))
	for _, id := range completed {
		if _, exists := g.tasks[id]; !exists {
			return nil, ErrUnknownCompleted
		}
		done[id] = true
	}
	records := readyRecords(g.tasks, done)
	out := make([]Task, len(records))
	for i, record := range records {
		out[i] = cloneTask(record.task)
	}
	return out, nil
}
