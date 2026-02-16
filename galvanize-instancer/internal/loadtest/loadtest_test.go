package loadtest

import "testing"

func TestParsePhasesDefault(t *testing.T) {
	phases, err := ParsePhases("")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(phases) != 3 {
		t.Fatalf("expected 3 phases, got %d", len(phases))
	}
	if phases[0].Name != "deploy" || phases[1].Name != "status" || phases[2].Name != "terminate" {
		t.Fatalf("unexpected phase order: %#v", phases)
	}
}

func TestParsePhasesCustom(t *testing.T) {
	phases, err := ParsePhases("status,terminate")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(phases))
	}
	if phases[0].Name != "status" || phases[1].Name != "terminate" {
		t.Fatalf("unexpected phase order: %#v", phases)
	}
}

func TestParsePhasesUnknown(t *testing.T) {
	_, err := ParsePhases("deploy,unknown")
	if err == nil {
		t.Fatalf("expected error for unknown phase")
	}
}
