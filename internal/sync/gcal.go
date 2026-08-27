package sync

import (
	"context"

	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

// The generic Google Calendar client and types live in platform/calendar. These
// aliases keep existing app code (sync, handlers, tests) referring to the short
// names; the sync-specific placeholder logic below stays here until M8 moves it
// into an internal/sync module.
type (
	Calendar           = calendar.Calendar
	GCalEvent          = calendar.GCalEvent
	EventTime          = calendar.EventTime
	Attendee           = calendar.Attendee
	Reminders          = calendar.Reminders
	ExtendedProperties = calendar.ExtendedProperties
	ListEventsResult   = calendar.ListEventsResult
)

// ErrSyncTokenExpired re-exports the client's sentinel for existing call sites.
var ErrSyncTokenExpired = calendar.ErrSyncTokenExpired

// IsDeclined reports whether the authenticated user declined the event.
func IsDeclined(event GCalEvent) bool { return calendar.IsDeclined(event) }

// FormatAttendees formats an attendee list as a human-readable string.
func FormatAttendees(attendees []Attendee) string { return calendar.FormatAttendees(attendees) }

// --- Sync-specific: placeholder marker, queries, and construction ---

// ListPlaceholders fetches placeholder events on a calendar for a specific source.
func ListPlaceholders(ctx context.Context, cal *calendar.Client, token, calendarID, sourceCalendarID string) ([]GCalEvent, error) {
	return cal.ListEventsByProperty(ctx, token, calendarID, calendar.EventQuery{
		PrivateProps: map[string]string{
			"calendarSyncMarker": "v1",
			"sourceCalendarId":   sourceCalendarID,
		},
		SingleEvents: true,
	})
}

// ListSyncPlaceholders fetches all placeholder events created by this app on a
// calendar. Exported as sync's cross-module query — the tools module uses it for its
// sync-only filter rather than duplicating the placeholder marker. (Moves to
// internal/sync in M8.)
func ListSyncPlaceholders(ctx context.Context, cal *calendar.Client, token, calendarID string) ([]GCalEvent, error) {
	return cal.ListEventsByProperty(ctx, token, calendarID, calendar.EventQuery{
		PrivateProps: map[string]string{"calendarSyncMarker": "v1"},
		SingleEvents: true,
	})
}

// IsPlaceholder returns true if the event was created by the sync engine.
func IsPlaceholder(event GCalEvent) bool {
	if event.ExtendedProperties == nil {
		return false
	}
	return event.ExtendedProperties.Private["calendarSyncMarker"] == "v1"
}

// SourceEventID returns the source event ID from a placeholder's extended properties.
func SourceEventID(event GCalEvent) string {
	if event.ExtendedProperties == nil {
		return ""
	}
	return event.ExtendedProperties.Private["sourceEventId"]
}

// SourceUpdated returns the source event's Updated timestamp stamped on a placeholder.
// This is the single change-detection value both sync directions (and both two-tier
// tiers) compare against, so an unchanged source event produces no rewrites regardless
// of the placeholder's own Updated. Empty for placeholders created before this stamp
// existed (they get one on the next update).
func SourceUpdated(event GCalEvent) string {
	if event.ExtendedProperties == nil {
		return ""
	}
	return event.ExtendedProperties.Private["sourceUpdated"]
}

// PlaceholderOptions configures how a placeholder event looks on the target calendar.
type PlaceholderOptions struct {
	EmojiPrefix string // prepended to title, e.g. "🔄 "
	ColorID     string // Google Calendar colorId (1-11), empty for default
}

// BuildPlaceholder creates a placeholder event from a source event. sourceEventID and
// sourceUpdated are the ORIGIN event's id and Updated timestamp — passed explicitly
// because outbound builds from the hub placeholder, whose own .ID is not the origin's
// (stamping source.ID there is the bug that broke outbound adoption). Both are stamped
// into the placeholder so identity and change detection stay consistent across
// directions and tiers (see SourceEventID / SourceUpdated).
func BuildPlaceholder(source GCalEvent, sourceCalID, sourceEventID, sourceUpdated string, opts PlaceholderOptions) GCalEvent {
	desc := source.Description
	if len(source.Attendees) > 0 {
		if desc != "" {
			desc += "\n\n"
		}
		desc += "---\nAttendees: " + FormatAttendees(source.Attendees)
	}

	summary := source.Summary
	if opts.EmojiPrefix != "" {
		summary = opts.EmojiPrefix + " " + summary
	}

	p := GCalEvent{
		Summary:        summary,
		Description:    desc,
		Location:       source.Location,
		Start:          source.Start,
		End:            source.End,
		Transparency:   source.Transparency, // empty means "opaque" (busy) — the API default
		ConferenceData: source.ConferenceData,
		Attachments:    source.Attachments,
		Reminders:      &Reminders{UseDefault: false},
		ExtendedProperties: &ExtendedProperties{
			Private: map[string]string{
				"calendarSyncMarker": "v1",
				"sourceCalendarId":   sourceCalID,
				"sourceEventId":      sourceEventID,
				"sourceUpdated":      sourceUpdated,
			},
		},
	}

	if opts.ColorID == "source" {
		p.ColorID = source.ColorID
	} else if opts.ColorID != "" {
		p.ColorID = opts.ColorID
	}

	return p
}
