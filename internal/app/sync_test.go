package app

import (
	"fmt"
	"testing"
)

func TestShouldSkipEventType(t *testing.T) {
	tests := []struct {
		eventType string
		skip      bool
	}{
		{"", false},
		{"default", false},
		{"focusTime", false},
		{"outOfOffice", false},
		{"workingLocation", true},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			if got := shouldSkipEventType(tt.eventType); got != tt.skip {
				t.Errorf("shouldSkipEventType(%q) = %v, want %v", tt.eventType, got, tt.skip)
			}
		})
	}
}

func TestIsNotFoundError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect bool
	}{
		{"nil", nil, false},
		{"not found", fmt.Errorf("API status 404: not found"), true},
		{"forbidden", fmt.Errorf("API status 403: forbidden"), false},
		{"other", fmt.Errorf("network timeout"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFoundError(tt.err); got != tt.expect {
				t.Errorf("isNotFoundError() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestIsPermissionError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect bool
	}{
		{"nil", nil, false},
		{"403 status", fmt.Errorf("API status 403: insufficient permissions"), true},
		{"forbidden", fmt.Errorf("forbidden"), true},
		{"404", fmt.Errorf("API status 404: not found"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermissionError(tt.err); got != tt.expect {
				t.Errorf("isPermissionError() = %v, want %v", got, tt.expect)
			}
		})
	}
}
