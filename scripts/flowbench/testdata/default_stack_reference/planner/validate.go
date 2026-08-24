package planner

import "fmt"

func validateRecords(tasks map[string]taskRecord) error {
	for id, record := range tasks {
		for _, dependency := range record.task.Dependencies {
			if _, exists := tasks[dependency]; !exists {
				return fmt.Errorf("%w: %s -> %s", ErrMissingDependency, id, dependency)
			}
		}
	}
	state := make(map[string]uint8, len(tasks))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("%w: %s", ErrCycle, id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, dependency := range tasks[id].task.Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range tasks {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graph) Validate() error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return validateRecords(g.tasks)
}
