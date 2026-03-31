package app

import (
	"testing"
)

func TestIsPlaceholder(t *testing.T) {
	tests := []struct {
		name   string
		event  GCalEvent
		expect bool
	}{
		{
			name:   "no extended properties",
			event:  GCalEvent{ID: "1"},
			expect: false,
		},
		{
			name: "has marker",
			event: GCalEvent{
				ID: "1",
				ExtendedProperties: &ExtendedProperties{
					Private: map[string]string{"calendarSyncMarker": "v1"},
				},
			},
			expect: true,
		},
		{
			name: "wrong marker value",
			event: GCalEvent{
				ID: "1",
				ExtendedProperties: &ExtendedProperties{
					Private: map[string]string{"calendarSyncMarker": "v2"},
				},
			},
			expect: false,
		},
		{
			name: "empty private map",
			event: GCalEvent{
				ID:                 "1",
				ExtendedProperties: &ExtendedProperties{Private: map[string]string{}},
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPlaceholder(tt.event)
			if got != tt.expect {
				t.Errorf("IsPlaceholder() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestIsDeclined(t *testing.T) {
	tests := []struct {
		name   string
		event  GCalEvent
		expect bool
	}{
		{
			name:   "no attendees",
			event:  GCalEvent{},
			expect: false,
		},
		{
			name: "self accepted",
			event: GCalEvent{
				Attendees: []Attendee{{Email: "me@x.com", Self: true, ResponseStatus: "accepted"}},
			},
			expect: false,
		},
		{
			name: "self declined",
			event: GCalEvent{
				Attendees: []Attendee{{Email: "me@x.com", Self: true, ResponseStatus: "declined"}},
			},
			expect: true,
		},
		{
			name: "other declined, self accepted",
			event: GCalEvent{
				Attendees: []Attendee{
					{Email: "other@x.com", Self: false, ResponseStatus: "declined"},
					{Email: "me@x.com", Self: true, ResponseStatus: "accepted"},
				},
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsDeclined(tt.event)
			if got != tt.expect {
				t.Errorf("IsDeclined() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestBuildPlaceholder(t *testing.T) {
	source := GCalEvent{
		ID:           "src-123",
		Summary:      "Team Standup",
		Description:  "Daily sync",
		Location:     "Room A",
		Start:        EventTime{DateTime: "2026-04-01T09:00:00Z"},
		End:          EventTime{DateTime: "2026-04-01T09:30:00Z"},
		Transparency: "opaque",
		Attendees: []Attendee{
			{Email: "alice@x.com", DisplayName: "Alice", Organizer: true},
			{Email: "bob@x.com", DisplayName: "Bob"},
		},
	}

	p := BuildPlaceholder(source, "work@example.com", PlaceholderOptions{})

	// Fields copied
	if p.Summary != "Team Standup" {
		t.Errorf("Summary = %q, want %q", p.Summary, "Team Standup")
	}
	if p.Location != "Room A" {
		t.Errorf("Location = %q, want %q", p.Location, "Room A")
	}
	if p.Start.DateTime != "2026-04-01T09:00:00Z" {
		t.Errorf("Start = %q, want %q", p.Start.DateTime, "2026-04-01T09:00:00Z")
	}
	if p.Transparency != "opaque" {
		t.Errorf("Transparency = %q, want %q", p.Transparency, "opaque")
	}

	// Reminders disabled
	if p.Reminders == nil || p.Reminders.UseDefault {
		t.Error("Reminders should be {UseDefault: false}")
	}

	// Extended properties set
	if p.ExtendedProperties == nil {
		t.Fatal("ExtendedProperties is nil")
	}
	if p.ExtendedProperties.Private["calendarSyncMarker"] != "v1" {
		t.Error("missing calendarSyncMarker")
	}
	if p.ExtendedProperties.Private["sourceCalendarId"] != "work@example.com" {
		t.Error("wrong sourceCalendarId")
	}
	if p.ExtendedProperties.Private["sourceEventId"] != "src-123" {
		t.Error("wrong sourceEventId")
	}

	// Attendees in description, not as attendees
	if len(p.Attendees) != 0 {
		t.Errorf("placeholder should have no attendees, got %d", len(p.Attendees))
	}
	if p.Description == "Daily sync" {
		t.Error("description should include attendees")
	}
	if !contains(p.Description, "Alice") || !contains(p.Description, "Bob") {
		t.Error("description should mention attendees")
	}
	if !contains(p.Description, "(organizer)") {
		t.Error("description should mark organizer")
	}

	// ID not copied
	if p.ID != "" {
		t.Errorf("placeholder ID should be empty, got %q", p.ID)
	}
}

func TestBuildPlaceholderWithEmoji(t *testing.T) {
	source := GCalEvent{ID: "src-1", Summary: "Meeting"}
	p := BuildPlaceholder(source, "cal@x.com", PlaceholderOptions{EmojiPrefix: "🔄"})
	if p.Summary != "🔄 Meeting" {
		t.Errorf("Summary = %q, want %q", p.Summary, "🔄 Meeting")
	}
}

func TestBuildPlaceholderWithColor(t *testing.T) {
	source := GCalEvent{ID: "src-1", Summary: "Meeting"}
	p := BuildPlaceholder(source, "cal@x.com", PlaceholderOptions{ColorID: "5"})
	if p.ColorID != "5" {
		t.Errorf("ColorID = %q, want %q", p.ColorID, "5")
	}
}

func TestBuildPlaceholderNoOptions(t *testing.T) {
	source := GCalEvent{ID: "src-1", Summary: "Meeting"}
	p := BuildPlaceholder(source, "cal@x.com", PlaceholderOptions{})
	if p.Summary != "Meeting" {
		t.Errorf("Summary = %q, want %q", p.Summary, "Meeting")
	}
	if p.ColorID != "" {
		t.Errorf("ColorID should be empty, got %q", p.ColorID)
	}
}

func TestBuildPlaceholderNoAttendees(t *testing.T) {
	source := GCalEvent{
		ID:          "src-456",
		Summary:     "Focus time",
		Description: "Deep work",
	}

	p := BuildPlaceholder(source, "cal@x.com", PlaceholderOptions{})

	if p.Description != "Deep work" {
		t.Errorf("Description = %q, want %q", p.Description, "Deep work")
	}
}

func TestFormatAttendees(t *testing.T) {
	attendees := []Attendee{
		{Email: "alice@x.com", DisplayName: "Alice", Organizer: true},
		{Email: "bob@x.com"},
	}

	result := FormatAttendees(attendees)
	if !contains(result, "Alice") || !contains(result, "(organizer)") || !contains(result, "bob@x.com") {
		t.Errorf("FormatAttendees = %q, missing expected content", result)
	}
}

func TestSourceEventID(t *testing.T) {
	event := GCalEvent{
		ExtendedProperties: &ExtendedProperties{
			Private: map[string]string{"sourceEventId": "abc"},
		},
	}
	if got := SourceEventID(event); got != "abc" {
		t.Errorf("SourceEventID = %q, want %q", got, "abc")
	}

	empty := GCalEvent{}
	if got := SourceEventID(empty); got != "" {
		t.Errorf("SourceEventID (empty) = %q, want empty", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
