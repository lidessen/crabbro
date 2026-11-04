package session

import "testing"

func TestTrajectoryPages(t *testing.T) {
	s := New(16)
	s.Start("login")
	s.Note("verify", "", "req_1", "", "replay")
	s.Note("find", "", "", "", "the login call")

	page, next, err := s.Trajectory("", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || next == "" {
		t.Fatalf("page=%d next=%q", len(page), next)
	}
	if page[0].Kind != "session_started" || page[1].Kind != "verify" {
		t.Fatalf("order = %s, %s", page[0].Kind, page[1].Kind)
	}
	rest, next2, err := s.Trajectory("", next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Kind != "find" || next2 != "" {
		t.Fatalf("rest=%v next=%q", rest, next2)
	}
	if _, _, err := s.Trajectory("", "nope", 1); err == nil {
		t.Fatal("expected invalid cursor")
	}
}
