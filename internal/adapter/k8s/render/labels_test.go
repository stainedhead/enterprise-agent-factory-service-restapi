package render

import "testing"

func TestManagedLabels(t *testing.T) {
	o := Owner{Kind: "team", ID: "team_X", TeamID: "team_X", TeamSlug: "payments"}
	u := New("v1", "Namespace", "", "afs-t-payments")
	u.SetLabels(map[string]string{"keep": "me"})
	Managed(u, o)
	l := u.GetLabels()
	if l["keep"] != "me" || l[LabelManagedBy] != ManagedByValue || l[LabelTeamID] != "team_X" || l[LabelTeamSlug] != "payments" || l[LabelResourceKind] != "team" || l[LabelResourceID] != "team_X" {
		t.Errorf("labels = %v", l)
	}
	if !IsManaged(u) || IsManaged(New("v1", "Namespace", "", "x")) {
		t.Error("IsManaged")
	}
	if got := OwnerOf(u); got != o {
		t.Errorf("OwnerOf = %+v", got)
	}
	// a singleton has no team labels, and Managed works on an object with no labels
	s := Managed(New("v1", "Namespace", "", "x"), Owner{Kind: "admission", ID: "afs-admission"})
	if _, ok := s.GetLabels()[LabelTeamID]; ok {
		t.Error("team label on a singleton")
	}
	if New("v1", "ConfigMap", "ns", "n").GetNamespace() != "ns" {
		t.Error("namespace")
	}
}
