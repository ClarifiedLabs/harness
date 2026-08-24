package planner

func (g *Graph) Batch(mutations []Mutation) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	tasks := cloneRecords(g.tasks)
	next := g.next
	for _, mutation := range mutations {
		switch mutation.Kind {
		case MutationAdd:
			if mutation.ID != "" {
				return ErrInvalidMutation
			}
			if err := addRecord(tasks, &next, mutation.Task); err != nil {
				return err
			}
		case MutationRemove:
			if mutation.ID == "" || mutation.Task.ID != "" || len(mutation.Task.Dependencies) != 0 || mutation.Task.Priority != 0 || mutation.Task.Payload != nil {
				return ErrInvalidMutation
			}
			if err := removeRecord(tasks, mutation.ID); err != nil {
				return err
			}
		default:
			return ErrInvalidMutation
		}
	}
	if err := validateRecords(tasks); err != nil {
		return err
	}
	g.tasks, g.next = tasks, next
	return nil
}

func (g *Graph) Topological() ([]Task, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := validateRecords(g.tasks); err != nil {
		return nil, err
	}
	done := make(map[string]bool, len(g.tasks))
	out := make([]Task, 0, len(g.tasks))
	for len(out) < len(g.tasks) {
		ready := readyRecords(g.tasks, done)
		if len(ready) == 0 {
			return nil, ErrCycle
		}
		selected := ready[0]
		done[selected.task.ID] = true
		out = append(out, cloneTask(selected.task))
	}
	return out, nil
}
