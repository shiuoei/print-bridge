package main

import (
	"fmt"
	"testing"
	"time"
)

// The About page must explain why a build cannot check for updates instead of
// rendering dead controls.
func TestUpdatePanelWithoutPlugin(t *testing.T) {
	d := newTestDesktop(t)
	texts := textsOf(t, d.aboutPage)
	if !containsText(texts, d.tr("updates_unavailable")) {
		t.Fatalf("about page does not explain that updates are unavailable: %v", texts)
	}
}

// The panel shows the last check, or the newer version a check found.
func TestUpdatePanelStates(t *testing.T) {
	d := newTestDesktop(t)
	d.updatesReady = true

	d.updateLastCheck = time.Date(2026, 11, 21, 9, 30, 0, 0, time.Local)
	texts := textsOf(t, d.aboutPage)
	if !containsText(texts, "2026-11-21 09:30") {
		t.Fatalf("about page does not show the last check time: %v", texts)
	}

	d.updateAvailable = "9.9.9"
	texts = textsOf(t, d.aboutPage)
	if want := fmt.Sprintf(d.tr("update_available"), "9.9.9"); !containsText(texts, want) {
		t.Fatalf("about page does not show %q: %v", want, texts)
	}
}

// A plain Go build has no version stamp; PRINTBRIDGE_VERSION overrides it and a
// versionless build reports dev rather than an empty string.
func TestAppVersion(t *testing.T) {
	t.Setenv("PRINTBRIDGE_VERSION", "9.9.9")
	if got := appVersion(); got != "9.9.9" {
		t.Fatalf("appVersion = %q, want the environment override", got)
	}
	t.Setenv("PRINTBRIDGE_VERSION", "")
	if got := appVersion(); got != "dev" {
		t.Fatalf("appVersion = %q, want dev for an unstamped build", got)
	}
}
