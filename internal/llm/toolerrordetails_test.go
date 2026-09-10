package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCloneToolErrorDetails(t *testing.T) {
	if got := CloneToolErrorDetails(nil); got != nil {
		t.Fatalf("nil clone = %+v", got)
	}
	for _, details := range []*ToolErrorDetails{
		{},
		{LeaseConflict: &LeaseConflictDetails{BlockingJobID: "bg_owner", BlockingAgent: "review", BlockingStatus: "completed", ResourceKey: "/repo", RequestedAccess: "exclusive", ActiveAccess: "read_only", Guidance: "wait for descendants"}},
	} {
		clone := CloneToolErrorDetails(details)
		if clone == details || !reflect.DeepEqual(clone, details) {
			t.Fatalf("clone = %+v, source = %+v", clone, details)
		}
		if details.LeaseConflict != nil {
			if clone.LeaseConflict == details.LeaseConflict {
				t.Fatal("clone aliases nested details")
			}
			clone.LeaseConflict.BlockingJobID = "changed"
			if details.LeaseConflict.BlockingJobID != "bg_owner" {
				t.Fatal("clone mutation changed source")
			}
		}
	}
}

func TestToolErrorDetailsJSONContract(t *testing.T) {
	const wire = `{"lease_conflict":{"blocking_job_id":"bg_owner","blocking_agent":"review","blocking_status":"completed","resource_key":"/repo","requested_access":"exclusive","active_access":"read_only","guidance":"wait for descendants"}}`
	var details ToolErrorDetails
	if err := json.Unmarshal([]byte(wire), &details); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wire {
		t.Fatalf("JSON = %s, want %s", got, wire)
	}
	got, err = json.Marshal(ToolErrorDetails{})
	if err != nil || string(got) != "{}" {
		t.Fatalf("empty details = %s, %v", got, err)
	}
}
