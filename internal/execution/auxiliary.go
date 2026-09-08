package execution

// TurnEvent describes a completed logical tool turn. ToolNames are configured
// tool names only; implementations must bound/allowlist them as metric labels.
// It never contains tool arguments, results, or transcript text.
type TurnEvent struct {
	Identity                                                          Identity
	ToolNames                                                         []string
	ToolCalls, Operations, SingleLookupCount, InspectionNoProgressRun int
	Activity, SteerReason                                             string
}

// TurnObserver is optional, so observers concerned only with exclusive billing
// do not need to retain logical-turn diagnostics.
type TurnObserver interface{ ObserveTurn(TurnEvent) }

func (s Scope) Turn(event TurnEvent) {
	if observer, ok := s.Observer.(TurnObserver); ok {
		event.Identity = s.Identity
		observer.ObserveTurn(event)
	}
}

// SkillEvent describes an actual activation or a catalog-budget observation.
// Names, descriptions, paths, and skill bodies never belong in this event.
type SkillEvent struct {
	Identity           Identity
	Source, Status     string
	Omitted, Truncated int
}

type SkillObserver interface{ ObserveSkill(SkillEvent) }

func (s Scope) Skill(event SkillEvent) {
	if observer, ok := s.Observer.(SkillObserver); ok {
		event.Identity = s.Identity
		observer.ObserveSkill(event)
	}
}
