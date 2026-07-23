package server

import "testing"

func TestRecordMaintenanceRejectsNamedDispatchAndGuestLocalDispatch(t *testing.T) {
	s := New(false)
	s.maintenanceMu.Lock()
	s.maintenanceTargets["sat"] = true
	s.maintenanceTargets[""] = true
	s.maintenanceMu.Unlock()
	if !s.isMaintenanceTarget("sat") {
		t.Fatal("named satellite dispatch was not blocked")
	}
	if !s.isMaintenanceTarget("") {
		t.Fatal("guest-local dispatch was not blocked")
	}
	if s.isMaintenanceTarget("other") {
		t.Fatal("unrelated satellite was blocked")
	}
}
