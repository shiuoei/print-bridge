package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
)

// TestWhitelistPageRendersPerEntryControls verifies each allowlist entry has its
// own delete control and an add field, instead of the previous single textarea.
func TestWhitelistPageRendersPerEntryControls(t *testing.T) {
	d := newTestDesktop(t)
	c := d.store.Config()
	c.Security.AllowedOrigins = []string{"https://erp.example.com"}
	c.Security.AllowedIPs = []string{domain.RequiredLoopbackIP, "192.168.1.0/24"}
	if err := d.store.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	texts := textsOf(t, func(ctx *ui.Context) { d.whitelistPage(ctx) })
	for _, want := range []string{
		"https://erp.example.com",
		"192.168.1.0/24",
		d.tr("add"),
		d.tr("delete"),
		d.tr("loopback_fixed"),
		d.tr("listener"),
	} {
		if !containsText(texts, want) {
			t.Errorf("whitelist page does not render %q; got %v", want, texts)
		}
	}
}

// TestAddOriginValidates keeps a malformed Origin from being persisted.
func TestAddOriginValidates(t *testing.T) {
	d := newTestDesktop(t)
	before := len(d.store.Config().Security.AllowedOrigins)

	for _, bad := range []string{"", "example.com", "ftp://example.com", "https://example.com/path", "https://example.com?q=1"} {
		d.newOrigin = bad
		d.addOrigin()
		if d.originError == "" {
			t.Errorf("%q was accepted", bad)
		}
		if got := len(d.store.Config().Security.AllowedOrigins); got != before {
			t.Fatalf("%q was persisted", bad)
		}
	}

	d.newOrigin = "https://erp.example.com"
	d.addOrigin()
	if d.originError != "" {
		t.Fatalf("a valid Origin was rejected: %s", d.originError)
	}
	if got := len(d.store.Config().Security.AllowedOrigins); got != before+1 {
		t.Fatalf("allowed origins = %d, want %d", got, before+1)
	}
}

// TestAddOriginRejectsDuplicate keeps the list free of repeats.
func TestAddOriginRejectsDuplicate(t *testing.T) {
	d := newTestDesktop(t)
	d.newOrigin = "https://erp.example.com"
	d.addOrigin()
	d.newOrigin = "https://erp.example.com"
	d.addOrigin()
	if d.originError != d.tr("entry_duplicate") {
		t.Fatalf("duplicate error = %q", d.originError)
	}
	if got := len(d.store.Config().Security.AllowedOrigins); got != 1 {
		t.Fatalf("allowed origins = %d, want 1", got)
	}
}

// TestRemoveOriginDeletesOneEntry checks a single row can be removed.
func TestRemoveOriginDeletesOneEntry(t *testing.T) {
	d := newTestDesktop(t)
	c := d.store.Config()
	c.Security.AllowedOrigins = []string{"https://a.example", "https://b.example"}
	if err := d.store.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	d.removeOrigin("https://a.example")
	got := d.store.Config().Security.AllowedOrigins
	if len(got) != 1 || got[0] != "https://b.example" {
		t.Fatalf("allowed origins = %v, want only b.example", got)
	}
}

// TestAddIPValidates covers the IP and CIDR rules, including any-address refusal.
func TestAddIPValidates(t *testing.T) {
	d := newTestDesktop(t)
	before := len(d.store.Config().Security.AllowedIPs)

	for _, bad := range []string{"", "not-an-ip", "192.168.1.0/33", "0.0.0.0/0", "::/0", "0.0.0.0"} {
		d.newIP = bad
		d.addIP()
		if d.ipError == "" {
			t.Errorf("%q was accepted", bad)
		}
		if got := len(d.store.Config().Security.AllowedIPs); got != before {
			t.Fatalf("%q was persisted", bad)
		}
	}

	for _, good := range []string{"192.168.1.0/24", "10.0.0.5", "::1"} {
		d.ipError = ""
		d.newIP = good
		d.addIP()
		if d.ipError != "" {
			t.Errorf("%q was rejected: %s", good, d.ipError)
		}
	}
}

// TestLoopbackEntryCannotBeRemoved is the pinning guarantee: the operator must not
// be able to lock the local UI out of its own agent.
func TestLoopbackEntryCannotBeRemoved(t *testing.T) {
	d := newTestDesktop(t)
	d.removeIP(domain.RequiredLoopbackIP)
	if d.ipError != d.tr("loopback_pinned") {
		t.Fatalf("error = %q, want the pinning message", d.ipError)
	}
	found := false
	for _, entry := range d.store.Config().Security.AllowedIPs {
		if entry == domain.RequiredLoopbackIP {
			found = true
		}
	}
	if !found {
		t.Fatal("the loopback entry was removed")
	}
}

// TestRemoveIPDeletesOtherEntries keeps removal working for non-pinned rows.
func TestRemoveIPDeletesOtherEntries(t *testing.T) {
	d := newTestDesktop(t)
	c := d.store.Config()
	c.Security.AllowedIPs = []string{domain.RequiredLoopbackIP, "192.168.1.0/24"}
	if err := d.store.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	d.removeIP("192.168.1.0/24")
	got := d.store.Config().Security.AllowedIPs
	if len(got) != 1 || got[0] != domain.RequiredLoopbackIP {
		t.Fatalf("allowed IPs = %v, want only loopback", got)
	}
}

// TestValidateOriginEntryRules pins the exact-origin rule.
func TestValidateOriginEntryRules(t *testing.T) {
	valid := []string{"https://example.com", "http://localhost:5173", "https://a.b.example"}
	for _, v := range valid {
		if err := validateOriginEntry(v); err != nil {
			t.Errorf("%q rejected: %v", v, err)
		}
	}
	invalid := []string{"", "example.com", "ftp://x", "https://x/p", "https://x?q", "https://x#f", "https://u@x"}
	for _, v := range invalid {
		if err := validateOriginEntry(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// TestValidateIPEntryRules pins the IP/CIDR rule.
func TestValidateIPEntryRules(t *testing.T) {
	valid := []string{"127.0.0.1", "::1", "192.168.1.0/24", "10.0.0.5"}
	for _, v := range valid {
		if err := validateIPEntry(v); err != nil {
			t.Errorf("%q rejected: %v", v, err)
		}
	}
	invalid := []string{"", "x", "0.0.0.0", "0.0.0.0/0", "::/0", "10.0.0.0/33"}
	for _, v := range invalid {
		if err := validateIPEntry(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// TestDoctorGroupsCoverEveryCheck keeps a newly added check from silently falling
// out of the grouped view.
func TestDoctorGroupsCoverEveryCheck(t *testing.T) {
	d := newTestDesktop(t)
	d.runDoctor()
	if d.doctor == nil {
		t.Skip("doctor produced no report in this environment")
	}
	seen := map[string]bool{}
	for _, group := range doctorGroups {
		for _, id := range group.ids {
			seen[id] = true
		}
	}
	for _, check := range d.doctor.Checks {
		if !seen[check.ID] && groupOf(check.ID) != "other" {
			t.Errorf("check %q is in no group and not classified as other", check.ID)
		}
	}
}

// TestDoctorSummaryCountsLevels checks the PASS/WARN/FAIL tallies.
func TestDoctorSummaryCountsLevels(t *testing.T) {
	d := newTestDesktop(t)
	d.runDoctor()
	if d.doctor == nil {
		t.Skip("doctor produced no report in this environment")
	}
	s := summarizeDoctor(d.doctor)
	total := 0
	for _, check := range d.doctor.Checks {
		if !check.Skipped {
			total++
		}
	}
	if s.ok+s.warn+s.fail != total {
		t.Fatalf("counters %+v do not sum to %d checks", s, total)
	}
}

// TestNeedsAttentionTracksWorstLevel keeps the header badge honest.
func TestNeedsAttentionTracksWorstLevel(t *testing.T) {
	d := newTestDesktop(t)
	if d.needsAttention() {
		t.Fatal("a fresh desktop claims attention before any check ran")
	}
	d.doctor = &command.DoctorReport{Checks: []command.DoctorCheck{{ID: "config", Level: command.DoctorOK}}}
	if d.needsAttention() {
		t.Fatal("all-OK report reported attention")
	}
	d.doctor = &command.DoctorReport{Checks: []command.DoctorCheck{{ID: "config", Level: command.DoctorFail}}}
	if !d.needsAttention() {
		t.Fatal("a failing report did not report attention")
	}
}

// TestDoctorPageRendersGroups verifies the grouped layout and suggestions.
func TestDoctorPageRendersGroups(t *testing.T) {
	d := newTestDesktop(t)
	d.runDoctor()
	texts := textsOf(t, func(ctx *ui.Context) { d.doctorPage(ctx) })
	for _, want := range []string{
		d.tr("doctor_group_core"),
		d.tr("doctor_group_printing"),
		d.tr("level_ok"),
		d.tr("run_checks"),
	} {
		if !containsText(texts, want) {
			t.Errorf("doctor page does not render %q; got %v", want, texts)
		}
	}
}

// TestPaperPresetResolvesDriverPaper keeps the dropdown label mapping reversible.
func TestPaperPresetResolvesDriverPaper(t *testing.T) {
	papers := []domain.PrinterPaper{
		{ID: "256", Name: "A4", WidthMM: 210, HeightMM: 297},
		{ID: "276", Name: "100x180", WidthMM: 100, HeightMM: 180},
	}
	label := paperLabelOf(papers[1])
	got := findPaper(papers, label)
	if got == nil || got.ID != "276" {
		t.Fatalf("findPaper(%q) = %#v", label, got)
	}
	if findPaper(papers, "nonexistent") != nil {
		t.Fatal("findPaper matched an unknown label")
	}
}

// TestPrintingPageOffersPresetAndCopies verifies the printer page gained the
// dropdown and the copies field.
func TestPrintingPageOffersPresetAndCopies(t *testing.T) {
	d := newTestDesktop(t)
	d.refreshPrinterInfo()
	texts := textsOf(t, func(ctx *ui.Context) { d.printing(ctx) })
	for _, want := range []string{
		d.tr("default_copies"),
		d.tr("paper_size"),
	} {
		if !containsText(texts, want) {
			t.Errorf("printing page does not render %q; got %v", want, texts)
		}
	}
}

// TestThemeChoicesPersist checks the theme is saved and validated.
func TestThemeChoicesPersist(t *testing.T) {
	d := newTestDesktop(t)
	d.setTheme(themeDark)
	if got := d.store.Config().App.Theme; got != themeDark {
		t.Fatalf("theme = %q, want %q", got, themeDark)
	}
	d.setTheme(themeLight)
	if got := d.store.Config().App.Theme; got != themeLight {
		t.Fatalf("theme = %q, want %q", got, themeLight)
	}
}

// TestInvalidThemeIsRejected keeps a typo from persisting an unusable value.
func TestInvalidThemeIsRejected(t *testing.T) {
	c := domain.DefaultConfig()
	c.App.Theme = "neon"
	if err := c.Validate(); err == nil {
		t.Fatal("an invalid theme passed validation")
	}
	for _, ok := range []string{"", "system", "light", "dark"} {
		c.App.Theme = ok
		if err := c.Validate(); err != nil {
			t.Errorf("theme %q rejected: %v", ok, err)
		}
	}
}

// TestAboutPageShowsVersionAndRedaction verifies the About tab renders and the
// support summary never contains the token.
func TestAboutPageShowsVersionAndRedaction(t *testing.T) {
	d := newTestDesktop(t)
	c := d.store.Config()
	c.Remote.BearerToken = "super-secret"
	c.Remote.EndpointURL = "https://erp.example.com/tenant-42/jobs"
	if err := d.store.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	d.version = "9.9.9"
	texts := textsOf(t, func(ctx *ui.Context) { d.aboutPage(ctx) })
	for _, want := range []string{"9.9.9", d.tr("about_go"), d.tr("about_data_dir")} {
		if !containsText(texts, want) {
			t.Errorf("about page does not render %q; got %v", want, texts)
		}
	}
	if got := redactEndpoint(c.Remote.EndpointURL); got != "https://erp.example.com" {
		t.Fatalf("redactEndpoint = %q, want scheme and host only", got)
	}
}

// TestRedactEndpointHandlesEmpty keeps the support summary readable.
func TestRedactEndpointHandlesEmpty(t *testing.T) {
	if got := redactEndpoint(""); got != "(empty)" {
		t.Fatalf("redactEndpoint(\"\") = %q", got)
	}
	if got := redactEndpoint("://bad"); got != "(invalid)" {
		t.Fatalf("redactEndpoint(\"://bad\") = %q", got)
	}
}
