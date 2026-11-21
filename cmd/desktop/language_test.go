package main

import (
	"testing"
)

// A data directory with no config.json must come up in the product's default
// language, Simplified Chinese, both in d.language and on a rendered page. en
// remains a choice, not the default.
func TestFreshConfigStartsWithChinese(t *testing.T) {
	d := newTestDesktop(t)
	if d.language != "zh-CN" {
		t.Fatalf("fresh config language = %q, want zh-CN", d.language)
	}
	if got, want := d.tr("language"), catalogs["zh-CN"]["language"]; got != want {
		t.Fatalf("tr(language) = %q, want the zh-CN string %q", got, want)
	}
	texts := textsOf(t, d.settings)
	if !containsText(texts, "界面语言") {
		t.Fatalf("settings page did not render the Chinese language label: %v", texts)
	}
}
